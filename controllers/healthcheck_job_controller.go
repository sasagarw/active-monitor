package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	activemonitorv1alpha1 "github.com/keikoproj/active-monitor/api/v1alpha1"
	"github.com/keikoproj/active-monitor/metrics"
	iebackoff "github.com/keikoproj/inverse-exp-backoff"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robfig/cron/v3"
	"k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"sync"
	"time"
)

const (
	hcjKind               = "HealthCheckJob"
	jobInstanceIdLabelKey = "jobs.keikoproj.io/controller-instanceid"
	jobInstanceId         = "activemonitor-jobs"
)

// HealthCheckJobReconciler reconciles a HealthCheck object
type HealthCheckJobReconciler struct {
	client.Client
	DynClient          dynamic.Interface
	Recorder           record.EventRecorder
	kubeclient         *kubernetes.Clientset
	Log                logr.Logger
	MaxParallel        int
	RepeatTimersByName map[string]*time.Timer
	TimerLock          sync.RWMutex
}

func (r *HealthCheckJobReconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues(hcjKind, req.NamespacedName)
	log.Info("Starting HealthCheckJob reconcile for ...")

	// initialize timers map if not already done
	if r.RepeatTimersByName == nil {
		r.RepeatTimersByName = make(map[string]*time.Timer)
	}

	var healthCheck = &activemonitorv1alpha1.HealthCheckJob{}
	if err := r.Get(ctx, req.NamespacedName, healthCheck); err != nil {
		// if our healthcheckjob was deleted, this Reconcile method is invoked with an empty resource cache
		// see: https://book.kubebuilder.io/cronjob-tutorial/controller-implementation.html#1-load-the-cronjob-by-name
		log.Info("HealthCheckJob object not found for reconciliation, likely deleted")
		// stop timer corresponding to next schedule run of job since parent healthcheck no longer exists
		if r.GetTimerByName(req.NamespacedName.Name) != nil {
			log.Info("Cancelling rescheduled job for this healthcheck due to deletion")
			r.Recorder.Event(healthCheck, corev1.EventTypeNormal,
				"Normal",
				"Cancelling job for this healthcheck due to deletion")
			r.GetTimerByName(req.NamespacedName.Name).Stop()
		}
		return ctrl.Result{}, ignoreNotFound(err)
	}
	return r.processOrUpdateHealthCheck(ctx, log, healthCheck)
}

func (r *HealthCheckJobReconciler) processOrUpdateHealthCheck(ctx context.Context, log logr.Logger,
	healthCheck *activemonitorv1alpha1.HealthCheckJob) (ctrl.Result, error) {
	defer func() {
		if err := recover(); err != nil {
			log.Info("Error: Panic occurred during execAdd %s/%s due to %s", healthCheck.Name, healthCheck.Namespace, err)
		}
	}()
	// Process HealthCheck
	ret, procErr := r.processHealthCheck(ctx, log, healthCheck)
	if procErr != nil {
		log.Error(procErr, "Job for this healthcheck has an error")
		if r.IsStorageError(procErr) {
			// avoid update errors for resources already deleted
			return ctrl.Result{}, nil
		}
		return reconcile.Result{RequeueAfter: 1 * time.Second}, procErr
	}
	err := r.Update(ctx, healthCheck)
	if err != nil {
		log.Error(err, "Error updating healthcheck resource")
		r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error updating healthcheck resource")
		// Force retry when status fails to update
		return ctrl.Result{}, err
	}
	return ret, procErr
}

func (r *HealthCheckJobReconciler) processHealthCheck(ctx context.Context, log logr.Logger,
	healthCheck *activemonitorv1alpha1.HealthCheckJob) (ctrl.Result, error) {
	hcSpec := healthCheck.Spec
	if hcSpec.Job.Resource != nil {
		now := metav1.Time{Time: time.Now()}
		var finishedAtTime int64
		if healthCheck.Status.FinishedAt != nil {
			finishedAtTime = healthCheck.Status.FinishedAt.Time.Unix()
		}
		// jobs can be paused by setting repeatAfterSec to <= 0 and not specifying the schedule for cron.
		if hcSpec.RepeatAfterSec <= 0 && hcSpec.Job.Resource.Schedule == "" {
			log.Info("Job will be skipped due to repeatAfterSec value", "repeatAfterSec", hcSpec.RepeatAfterSec)
			healthCheck.Status.Status = "Stopped"
			healthCheck.Status.ErrorMessage = fmt.Sprintf("job execution is stopped; "+
				"either spec.RepeatAfterSec or spec.Schedule must be provided. spec.RepeatAfterSec set to %d. "+
				"spec.Schedule set to %+v", hcSpec.RepeatAfterSec, hcSpec.Job.Resource.Schedule)
			healthCheck.Status.FinishedAt = &now
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning",
				"Job execution is stopped; either spec.RepeatAfterSec or spec.Schedule must be provided")
			err := r.updateHealthCheckStatus(ctx, log, healthCheck)
			if err != nil {
				log.Error(err, "Error updating healthcheck resource")
				r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error updating healthcheck resource")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else if hcSpec.RepeatAfterSec <= 0 && hcSpec.Job.Resource.Schedule != "" {
			log.Info("Job to be set with Schedule", "Cron", hcSpec.Job.Resource.Schedule)
			schedule, err := cron.ParseStandard(hcSpec.Job.Resource.Schedule)
			if err != nil {
				log.Error(err, "fail to parse cron")
				r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Fail to parse cron schedule")
			}
			// The value from schedule next and substracting from current time is in fraction as we convert to int it will be 1 less than
			// the intended reschedule so we need to add 1sec to get the actual value
			// we need to update the spec so have to healthCheck.Spec.RepeatAfterSec instead of local variable hcSpec
			healthCheck.Spec.RepeatAfterSec = int(schedule.Next(time.Now()).Sub(time.Now())/time.Second) + 1
			log.Info("spec.RepeatAfterSec value is set", "RepeatAfterSec", healthCheck.Spec.RepeatAfterSec)
		} else if int(time.Now().Unix()-finishedAtTime) < hcSpec.RepeatAfterSec {
			log.Info("Job already executed", "finishedAtTime", finishedAtTime)
			return ctrl.Result{}, nil
		}

		log.Info("Creating Job", "namespace", hcSpec.Job.Resource.Namespace, "generateNamePrefix", hcSpec.Job.GenerateName)
		generatedJobName, err := r.createSubmitJob(ctx, log, healthCheck)
		if err != nil {
			log.Error(err, "Error creating or submitting job")
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error creating or submitting job")
			return ctrl.Result{}, err
		}

		err = r.watchJobReschedule(ctx, ctrl.Request{}, log, hcSpec.Job.Resource.Namespace, generatedJobName, healthCheck)
		if err != nil {
			log.Error(err, "Error executing Job")
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error executing Job")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// this function exists to assist with how a function called by the timer.AfterFunc() method operates to call a
// function which takes parameters, it is easiest to build this closure which holds access to the parameters we need.
// the helper returns a function object taking no parameters directly, this is what we want to give AfterFunc
func (r *HealthCheckJobReconciler) createSubmitJobHelper(ctx context.Context, log logr.Logger,
	jobNamespace string, hc *activemonitorv1alpha1.HealthCheckJob) func() {
	return func() {
		log.Info("Creating and Submitting Job...")
		jobName, err := r.createSubmitJob(ctx, log, hc)
		if err != nil {
			log.Error(err, "Error creating or submitting job")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error creating or submitting job")
		}
		err = r.watchJobReschedule(ctx, ctrl.Request{}, log, jobNamespace, jobName, hc)
		if err != nil {
			log.Error(err, "Error watching or rescheduling job")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error watching or rescheduling job")
		}
	}
}

func (r *HealthCheckJobReconciler) createSubmitJob(ctx context.Context, log logr.Logger,
	hc *activemonitorv1alpha1.HealthCheckJob) (cjName string, err error) {
	job := &v1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind: "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: hc.Spec.Job.GenerateName,
			Namespace:    hc.Spec.Job.Resource.Namespace,
		},
		Spec: v1.JobSpec{
			ActiveDeadlineSeconds:   hc.Spec.Job.Resource.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: hc.Spec.Job.Resource.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: hc.Spec.Job.Resource.Template.Spec.RestartPolicy,
				},
			},
		},
	}
	r.TimerLock.RLock()
	if hc.Spec.Job.Resource.Labels == nil {
		job.SetLabels(map[string]string{
			jobInstanceIdLabelKey: jobInstanceId,
		})
	} else {
		job.SetLabels(hc.Spec.Job.Resource.Labels)
	}
	if hc.Spec.Job.Resource.Annotations != nil {
		job.SetAnnotations(hc.Spec.Job.Resource.Annotations)
	}
	r.TimerLock.RUnlock()
	// set the owner references for workflow
	ownerReferences := job.GetOwnerReferences()
	trueVar := true
	newRef := metav1.OwnerReference{
		Kind:       hcjKind,
		APIVersion: hcVersion,
		Name:       hc.GetName(),
		UID:        hc.GetUID(),
		Controller: &trueVar,
	}
	ownerReferences = append(ownerReferences, newRef)
	job.SetOwnerReferences(ownerReferences)
	log.Info("Added new owner reference", "UID", newRef.UID)

	// Attach the containers to the cronjob
	var containers []corev1.Container
	for _, container := range hc.Spec.Job.Resource.Template.Spec.Containers {
		containers = append(containers, corev1.Container{
			Name:    container.Name,
			Image:   container.Image,
			Command: container.Command,
			Args:    container.Args,
		})
	}
	job.Spec.Template.Spec.Containers = containers

	// finally, attempt to create job using the kube client
	err = r.Create(ctx, job)
	if err != nil {
		log.Error(err, "Error creating job")
		return "", err
	}
	generatedName := job.GetName()
	log.Info("Created job", "generatedName", generatedName)
	r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Successfully created job")
	return generatedName, nil
}

func (r *HealthCheckJobReconciler) watchJobReschedule(ctx context.Context, req ctrl.Request, log logr.Logger,
	jobNamespace, jobName string, hc *activemonitorv1alpha1.HealthCheckJob) error {
	var now metav1.Time
	then := metav1.Time{Time: time.Now()}
	repeatAfterSec := hc.Spec.RepeatAfterSec
	maxTime := time.Duration(hc.Spec.Job.Timeout/2) * time.Second
	if maxTime <= 0 {
		maxTime = time.Second
	}
	minTime := time.Duration(hc.Spec.Job.Timeout/60) * time.Second
	if minTime <= 0 {
		minTime = time.Second
	}
	timeout := time.Duration(hc.Spec.Job.Timeout) * time.Second
	log.Info("IEB with timeout times are", "maxTime:", maxTime, "minTime:", minTime, "timeout:", timeout)
	for ieTimer, err1 := iebackoff.NewIEBWithTimeout(maxTime, minTime, timeout, 0.5, time.Now()); ; err1 = ieTimer.Next() {
		now = metav1.Time{Time: time.Now()}
		// grab job object by name and check its status; update healthcheck accordingly
		// do this once per second until the job reaches a terminal state (success/failure)
		var job = &v1.Job{}
		if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: jobNamespace}, job); err != nil {
			// if the job wasn't found, it is most likely the case that its parent healthcheck was deleted
			// we can swallow this error and simply not reschedule
			return ignoreNotFound(err)
		}
		status, ok := v1.JobStatus{}, false
		if job.Status.String() != "" {
			status, ok = job.Status, true
		}
		log.Info("status of job", "status:", status, "ok:", ok)

		if err1 != nil {
			//status, ok = map[string]interface{}{"phase": failStr, "message": failStr}, true
			// TODO: check if this is ideal way of doing this
			status, ok = v1.JobStatus{Failed: 1}, true
			log.Error(err1, "iebackoff err message")
			log.Info("status of job is updated to Failed", "status:", status)
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Job timed out")
		}
		if ok {
			log.Info("Job status", "status", "done", "succeeded", status.Succeeded, "Failed", status.Failed)
			if status.Succeeded == 1 {
				r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Job status is Succeeded")
				hc.Status.Status = succStr
				hc.Status.StartedAt = &then
				hc.Status.FinishedAt = &now
				log.Info("Time:", "hc.Status.StartedAt:", hc.Status.StartedAt)
				log.Info("Time:", "hc.Status.FinishedAt:", hc.Status.FinishedAt)
				hc.Status.SuccessCount++
				hc.Status.TotalHealthCheckRuns = hc.Status.SuccessCount + hc.Status.FailedCount
				hc.Status.LastSuccessfulJob = jobName
				metrics.MonitorSuccess.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Inc()
				metrics.MonitorRuntime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(now.Time.Sub(then.Time).Seconds())
				metrics.MonitorStartedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(then.Unix()))
				metrics.MonitorFinishedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(hc.Status.FinishedAt.Unix()))
				if !hc.Spec.RemedyJob.IsEmpty() && hc.Status.RemedyTotalRuns >= 1 {
					// HealthCheck Passed so remedy values are reset
					hc.Status.RemedyTotalRuns = 0
					hc.Status.RemedyFinishedAt = nil
					hc.Status.RemedyStartedAt = nil
					hc.Status.RemedyFailedCount = 0
					hc.Status.RemedySuccessCount = 0
					hc.Status.RemedyLastFailedAt = nil
					hc.Status.RemedyStatus = "HealthCheck Passed so Remedy is reset"
					log.Info("HealthCheck passed so Remedy is reset",
						"RemedyTotalRuns:", hc.Status.RemedyTotalRuns,
						"RemedySuccessCount", hc.Status.RemedySuccessCount,
						"RemedyFailedCount", hc.Status.RemedyFailedCount)
					r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "HealthCheck passed so Remedy is reset")
				}
				break
			} else if status.Failed == 1 {
				r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Job status is Failed")
				hc.Status.Status = failStr
				hc.Status.StartedAt = &then
				hc.Status.FinishedAt = &now
				hc.Status.LastFailedAt = &now
				hc.Status.ErrorMessage = status.String()
				hc.Status.FailedCount++
				hc.Status.TotalHealthCheckRuns = hc.Status.SuccessCount + hc.Status.FailedCount
				hc.Status.LastFailedJob = jobName
				metrics.MonitorError.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Inc()
				metrics.MonitorStartedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(then.Unix()))
				metrics.MonitorFinishedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(now.Time.Unix()))
				log.Info("Remedy values:", "RemedyTotalRuns:", hc.Status.RemedyTotalRuns)
				if !hc.Spec.RemedyJob.IsEmpty() {
					// TODO: Implement remedy job handling
				}
				break
			}
		}
	}

	// since the job has taken an unknown duration of time to complete, it's possible that its parent
	// healthcheck may no longer exist; ensure that it still does before attempting to update it and reschedule
	// see: https://book.kubebuilder.io/reference/using-finalizers.html
	if hc.ObjectMeta.DeletionTimestamp.IsZero() {
		// since the underlying job has completed, we update the healthcheck accordingly
		err := r.updateHealthCheckStatus(ctx, log, hc)
		if err != nil {
			log.Error(err, "Error updating healthcheck resource")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error updating healthcheck resource")
			if r.GetTimerByName(req.NamespacedName.Name) != nil {
				log.Info("Cancelling rescheduled job for this healthcheck due to deletion")
				r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Cancelling rescheduled job for this healthcheck due to deletion")
				r.GetTimerByName(hc.GetName()).Stop()
			}
			return err
		}

		// reschedule next run of job
		helper := r.createSubmitJobHelper(ctx, log, jobNamespace, hc)
		r.TimerLock.Lock()
		r.RepeatTimersByName[hc.GetName()] = time.AfterFunc(time.Duration(repeatAfterSec)*time.Second, helper)
		r.TimerLock.Unlock()
		log.Info("Rescheduled job for next run", "namespace", jobNamespace, "name", jobName)
		r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Rescheduled job for next run")
	}
	return nil
}

func (r *HealthCheckJobReconciler) updateHealthCheckStatus(ctx context.Context, log logr.Logger, hc *activemonitorv1alpha1.HealthCheckJob) error {
	if err := r.Status().Update(ctx, hc); err != nil {
		log.Error(err, "HealthCheckJob status could not be updated.")
		r.Recorder.Event(hc, "Warning", "Failed", fmt.Sprintf("HealthCheck %s/%s status could not be updated. %v", hc.Namespace, hc.Name, err))
		return err
	}
	return nil
}

func (r *HealthCheckJobReconciler) ContainsEqualFoldSubstring(str, substr string) bool {
	x := strings.ToLower(str)
	y := strings.ToLower(substr)
	if strings.Contains(x, y) {
		return true
	}
	return false
}

func (r *HealthCheckJobReconciler) IsStorageError(err error) bool {
	if r.ContainsEqualFoldSubstring(err.Error(), "StorageError: invalid object") {
		return true
	}
	return false
}

func (r *HealthCheckJobReconciler) GetTimerByName(name string) *time.Timer {
	r.TimerLock.RLock()
	s := r.RepeatTimersByName[name]
	r.TimerLock.RUnlock()
	return s
}

// SetupWithManager as used in main package by kubebuilder v2.0.0.alpha4
func (r *HealthCheckJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.kubeclient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	return ctrl.NewControllerManagedBy(mgr).
		For(&activemonitorv1alpha1.HealthCheckJob{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxParallel}).
		Complete(r)
}