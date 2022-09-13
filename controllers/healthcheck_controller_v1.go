package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	activemonitorv1 "github.com/keikoproj/active-monitor/api/v1"
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
	hcVersionV1 = "v1"
)

// HealthCheckReconcilerV1 reconciles a HealthCheck object
type HealthCheckReconcilerV1 struct {
	client.Client
	DynClient          dynamic.Interface
	Recorder           record.EventRecorder
	kubeclient         *kubernetes.Clientset
	Log                logr.Logger
	MaxParallel        int
	RepeatTimersByName map[string]*time.Timer
	TimerLock          sync.RWMutex
}

func (r *HealthCheckReconcilerV1) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues(hcKind, req.NamespacedName)
	log.Info("Starting HealthCheckV1 reconcile for ...")

	// initialize timers map if not already done
	if r.RepeatTimersByName == nil {
		r.RepeatTimersByName = make(map[string]*time.Timer)
	}

	var healthCheck = &activemonitorv1.HealthCheck{}
	if err := r.Get(ctx, req.NamespacedName, healthCheck); err != nil {
		// if our healthcheck was deleted, this Reconcile method is invoked with an empty resource cache
		// see: https://book.kubebuilder.io/cronjob-tutorial/controller-implementation.html#1-load-the-cronjob-by-name
		log.Info("HealthcheckV1 object not found for reconciliation, likely deleted")
		// stop timer corresponding to next schedule run of workflow since parent healthcheck no longer exists
		if r.GetTimerByName(req.NamespacedName.Name) != nil {
			log.Info("Cancelling rescheduled workflow for this healthcheck due to deletion")
			r.Recorder.Event(healthCheck, corev1.EventTypeNormal,
				"Normal",
				"Cancelling workflow for this healthcheck due to deletion")
			r.GetTimerByName(req.NamespacedName.Name).Stop()
		}
		return ctrl.Result{}, ignoreNotFound(err)
	}
	return r.processOrUpdateHealthCheck(ctx, log, healthCheck)
}

func (r *HealthCheckReconcilerV1) processOrUpdateHealthCheck(ctx context.Context, log logr.Logger,
	healthCheck *activemonitorv1.HealthCheck) (ctrl.Result, error) {
	defer func() {
		if err := recover(); err != nil {
			log.Info("Error: Panic occurred during execAdd %s/%s due to %s", healthCheck.Name, healthCheck.Namespace, err)
		}
	}()
	// Process HealthCheck
	ret, procErr := r.processHealthCheck(ctx, log, healthCheck)
	if procErr != nil {
		log.Error(procErr, "Workflow for this healthcheck has an error")
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

func (r *HealthCheckReconcilerV1) processHealthCheck(ctx context.Context, log logr.Logger,
	healthCheck *activemonitorv1.HealthCheck) (ctrl.Result, error) {
	hcSpec := healthCheck.Spec
	if hcSpec.CronJobWorkflow.Resource != nil {
		now := metav1.Time{Time: time.Now()}
		var finishedAtTime int64
		if healthCheck.Status.FinishedAt != nil {
			finishedAtTime = healthCheck.Status.FinishedAt.Time.Unix()
		}
		// workflows can be paused by setting repeatAfterSec to <= 0 and not specifying the schedule for cron.
		if hcSpec.RepeatAfterSec <= 0 && hcSpec.CronJobWorkflow.Resource.Schedule == "" {
			log.Info("Workflow will be skipped due to repeatAfterSec value", "repeatAfterSec", hcSpec.RepeatAfterSec)
			healthCheck.Status.Status = "Stopped"
			healthCheck.Status.ErrorMessage = fmt.Sprintf("workflow execution is stopped; either spec.RepeatAfterSec or spec.Schedule must be provided. spec.RepeatAfterSec set to %d. spec.Schedule set to %+v", hcSpec.RepeatAfterSec, hcSpec.CronJobWorkflow.Resource.Schedule)
			healthCheck.Status.FinishedAt = &now
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Workflow execution is stopped; either spec.RepeatAfterSec or spec.Schedule must be provided")
			err := r.updateHealthCheckStatus(ctx, log, healthCheck)
			if err != nil {
				log.Error(err, "Error updating healthcheck resource")
				r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error updating healthcheck resource")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else if hcSpec.RepeatAfterSec <= 0 && hcSpec.CronJobWorkflow.Resource.Schedule != "" {
			log.Info("Workflow to be set with Schedule", "Cron", hcSpec.CronJobWorkflow.Resource.Schedule)
			schedule, err := cron.ParseStandard(hcSpec.CronJobWorkflow.Resource.Schedule)
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
			log.Info("Workflow already executed", "finishedAtTime", finishedAtTime)
			return ctrl.Result{}, nil
		}

		log.Info("Creating Workflow", "namespace", hcSpec.CronJobWorkflow.Resource.Namespace, "generateNamePrefix", hcSpec.CronJobWorkflow.GenerateName)
		generatedJobName, err := r.createSubmitWorkflow(ctx, log, healthCheck)
		if err != nil {
			log.Error(err, "Error creating or submitting workflow")
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error creating or submitting workflow")
			return ctrl.Result{}, err
		}

		err = r.watchWorkflowReschedule(ctx, ctrl.Request{}, log, hcSpec.CronJobWorkflow.Resource.Namespace, generatedJobName, healthCheck)
		if err != nil {
			log.Error(err, "Error executing Workflow")
			r.Recorder.Event(healthCheck, corev1.EventTypeWarning, "Warning", "Error executing Workflow")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// this function exists to assist with how a function called by the timer.AfterFunc() method operates to call a
// function which takes parameters, it is easiest to build this closure which holds access to the parameters we need.
// the helper returns a function object taking no parameters directly, this is what we want to give AfterFunc
func (r *HealthCheckReconcilerV1) createSubmitWorkflowHelper(ctx context.Context, log logr.Logger, jobNamespace string, hc *activemonitorv1.HealthCheck) func() {
	return func() {
		log.Info("Creating and Submitting Job Workflow...")
		jobName, err := r.createSubmitWorkflow(ctx, log, hc)
		if err != nil {
			log.Error(err, "Error creating or submitting job workflow")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error creating or submitting job workflow")
		}
		err = r.watchWorkflowReschedule(ctx, ctrl.Request{}, log, jobNamespace, jobName, hc)
		if err != nil {
			log.Error(err, "Error watching or rescheduling job workflow")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error watching or rescheduling job workflow")
		}
	}
}

func (r *HealthCheckReconcilerV1) createSubmitWorkflow(ctx context.Context, log logr.Logger,
	hc *activemonitorv1.HealthCheck) (cjName string, err error) {
	job := &v1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind: "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: hc.Spec.CronJobWorkflow.GenerateName,
			Namespace:    hc.Spec.CronJobWorkflow.Resource.Namespace,
		},
		Spec: v1.JobSpec{
			Parallelism:             hc.Spec.CronJobWorkflow.Resource.Parallelism,
			ActiveDeadlineSeconds:   hc.Spec.CronJobWorkflow.Resource.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: hc.Spec.CronJobWorkflow.Resource.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: hc.Spec.CronJobWorkflow.Resource.Template.Spec.RestartPolicy,
				},
			},
		},
	}
	//cronjob := &v1beta1.CronJob{
	//TypeMeta: metav1.TypeMeta{
	//	Kind: "CronJob",
	//},
	//ObjectMeta: metav1.ObjectMeta{
	//	GenerateName: hc.Spec.CronJobWorkflow.GenerateName,
	//	Namespace:    hc.Spec.CronJobWorkflow.Resource.Namespace,
	//},
	//Spec: v1beta1.CronJobSpec{
	//	Schedule: hc.Spec.CronJobWorkflow.Resource.Schedule,
	//	JobTemplate: v1beta1.JobTemplateSpec{
	//		Spec: v1.JobSpec{
	//			Parallelism:             hc.Spec.CronJobWorkflow.Resource.Parallelism,
	//			ActiveDeadlineSeconds:   hc.Spec.CronJobWorkflow.Resource.ActiveDeadlineSeconds,
	//			TTLSecondsAfterFinished: hc.Spec.CronJobWorkflow.Resource.TTLSecondsAfterFinished,
	//			Template: corev1.PodTemplateSpec{
	//				Spec: corev1.PodSpec{
	//					RestartPolicy: hc.Spec.CronJobWorkflow.Resource.Template.Spec.RestartPolicy,
	//				},
	//			},
	//		},
	//	},
	//},
	//}
	r.TimerLock.RLock()
	if hc.Spec.CronJobWorkflow.Resource.Labels == nil {
		job.SetLabels(map[string]string{
			WfInstanceIdLabelKey: WfInstanceId,
		})
	} else {
		job.SetLabels(hc.Spec.CronJobWorkflow.Resource.Labels)
	}
	r.TimerLock.RUnlock()
	// set the owner references for workflow
	ownerReferences := job.GetOwnerReferences()
	trueVar := true
	newRef := metav1.OwnerReference{
		Kind:       hcKind,
		APIVersion: hcVersionV1,
		Name:       hc.GetName(),
		UID:        hc.GetUID(),
		Controller: &trueVar,
	}
	ownerReferences = append(ownerReferences, newRef)
	job.SetOwnerReferences(ownerReferences)
	log.Info("Added new owner reference", "UID", newRef.UID)

	// Attach the containers to the cronjob
	var containers []corev1.Container
	for _, container := range hc.Spec.CronJobWorkflow.Resource.Template.Spec.Containers {
		containers = append(containers, corev1.Container{
			Name:    container.Name,
			Image:   container.Image,
			Command: container.Command,
			Args:    container.Args,
		})
	}
	job.Spec.Template.Spec.Containers = containers

	// finally, attempt to create workflow using the kube client
	err = r.Create(ctx, job)
	if err != nil {
		log.Error(err, "Error creating cronjob")
		return "", err
	}
	generatedName := job.GetName()
	log.Info("Created cronjob", "generatedName", generatedName)
	r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Successfully created cronjob")
	return generatedName, nil
}

func (r *HealthCheckReconcilerV1) watchWorkflowReschedule(ctx context.Context, req ctrl.Request, log logr.Logger, jobNamespace, jobName string, hc *activemonitorv1.HealthCheck) error {
	var now metav1.Time
	then := metav1.Time{Time: time.Now()}
	repeatAfterSec := hc.Spec.RepeatAfterSec
	maxTime := time.Duration(hc.Spec.CronJobWorkflow.Timeout/2) * time.Second
	if maxTime <= 0 {
		maxTime = time.Second
	}
	minTime := time.Duration(hc.Spec.CronJobWorkflow.Timeout/60) * time.Second
	if minTime <= 0 {
		minTime = time.Second
	}
	timeout := time.Duration(hc.Spec.CronJobWorkflow.Timeout) * time.Second
	log.Info("IEB with timeout times are", "maxTime:", maxTime, "minTime:", minTime, "timeout:", timeout)
	for ieTimer, err1 := iebackoff.NewIEBWithTimeout(maxTime, minTime, timeout, 0.5, time.Now()); ; err1 = ieTimer.Next() {
		now = metav1.Time{Time: time.Now()}
		// grab job object by name and check its status; update healthcheck accordingly
		// do this once per second until the workflow reaches a terminal state (success/failure)
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
		log.Info("status of workflow", "status:", status, "ok:", ok)

		if err1 != nil {
			//status, ok = map[string]interface{}{"phase": failStr, "message": failStr}, true
			status, ok = v1.JobStatus{Failed: 1}, true
			log.Error(err1, "iebackoff err message")
			log.Info("status of workflow is updated to Failed", "status:", status)
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Workflow timed out")
		}
		if ok {
			log.Info("Job workflow status", "status", "done", "succeeded", status.Succeeded, "Failed", status.Failed)
			if status.Succeeded == 1 {
				r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Workflow status is Succeeded")
				hc.Status.Status = succStr
				hc.Status.StartedAt = &then
				hc.Status.FinishedAt = &now
				log.Info("Time:", "hc.Status.StartedAt:", hc.Status.StartedAt)
				log.Info("Time:", "hc.Status.FinishedAt:", hc.Status.FinishedAt)
				hc.Status.SuccessCount++
				hc.Status.TotalHealthCheckRuns = hc.Status.SuccessCount + hc.Status.FailedCount
				hc.Status.LastSuccessfulWorkflow = jobName
				metrics.MonitorSuccess.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Inc()
				metrics.MonitorRuntime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(now.Time.Sub(then.Time).Seconds())
				metrics.MonitorStartedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(then.Unix()))
				metrics.MonitorFinishedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(hc.Status.FinishedAt.Unix()))
				if !hc.Spec.RemedyWorkflow.IsEmpty() && hc.Status.RemedyTotalRuns >= 1 {
					// HealthCheck Passed so remedy values are reset
					hc.Status.RemedyTotalRuns = 0
					hc.Status.RemedyFinishedAt = nil
					hc.Status.RemedyStartedAt = nil
					hc.Status.RemedyFailedCount = 0
					hc.Status.RemedySuccessCount = 0
					hc.Status.RemedyLastFailedAt = nil
					hc.Status.RemedyStatus = "HealthCheck Passed so Remedy is reset"
					log.Info("HealthCheck passed so Remedy is reset", "RemedyTotalRuns:", hc.Status.RemedyTotalRuns, "RemedySuccessCount", hc.Status.RemedySuccessCount, "RemedyFailedCount", hc.Status.RemedyFailedCount)
					r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "HealthCheck passed so Remedy is reset")
				}
				break
			} else if status.Failed == 1 {
				r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Workflow status is Failed")
				hc.Status.Status = failStr
				hc.Status.StartedAt = &then
				hc.Status.FinishedAt = &now
				hc.Status.LastFailedAt = &now
				hc.Status.ErrorMessage = status.String()
				hc.Status.FailedCount++
				hc.Status.TotalHealthCheckRuns = hc.Status.SuccessCount + hc.Status.FailedCount
				hc.Status.LastFailedWorkflow = jobName
				metrics.MonitorError.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Inc()
				metrics.MonitorStartedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(then.Unix()))
				metrics.MonitorFinishedTime.With(prometheus.Labels{"healthcheck_name": hc.GetName(), "workflow": healthcheck}).Set(float64(now.Time.Unix()))
				log.Info("Remedy values:", "RemedyTotalRuns:", hc.Status.RemedyTotalRuns)
				if !hc.Spec.RemedyWorkflow.IsEmpty() {
					log.Info("RemedyWorkflow not empty:")
					if hc.Spec.RemedyRunsLimit != 0 && hc.Spec.RemedyResetInterval != 0 {
						log.Info("RemedyRunsLimit and  RemedyResetInterval values are", "RemedyRunsLimit", hc.Spec.RemedyRunsLimit, "RemedyResetInterval", hc.Spec.RemedyResetInterval)
						if hc.Spec.RemedyRunsLimit > hc.Status.RemedyTotalRuns {
							log.Info("RemedyRunsLimit is greater than  RemedyTotalRuns", "RemedyRunsLimit", hc.Spec.RemedyRunsLimit, "RemedyTotalRuns", hc.Status.RemedyTotalRuns)
							//err := r.processRemedyWorkflow(ctx, log, wfNamespace, hc)
							//if err != nil {
							//	log.Error(err, "Error executing RemedyWorkflow")
							//	r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error executing RemedyWorkflow")
							//	return err
							//}
						} else {
							remedylastruntime := int(now.Time.Sub(hc.Status.RemedyFinishedAt.Time).Seconds())
							log.Info("Remedy interval from last time run:", "intervalTime:", remedylastruntime)
							if hc.Spec.RemedyResetInterval >= remedylastruntime {
								log.Info("skipping remedy as the remedy limit criteria is met. Remedy will be run after reset interval")
							} else {
								hc.Status.RemedyTotalRuns = 0
								hc.Status.RemedyFinishedAt = nil
								hc.Status.RemedyStartedAt = nil
								hc.Status.RemedyFailedCount = 0
								hc.Status.RemedySuccessCount = 0
								hc.Status.RemedyLastFailedAt = nil
								hc.Status.RemedyStatus = "RemedyResetInterval elapsed so Remedy is reset"
								log.Info("RemedyResetInterval elapsed so Remedy is reset", "RemedyTotalRuns:", hc.Status.RemedyTotalRuns, "RemedySuccessCount", hc.Status.RemedySuccessCount, "RemedyFailedCount", hc.Status.RemedyFailedCount)
								r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "RemedyResetInterval elapsed so Remedy is reset")
								//err := r.processRemedyWorkflow(ctx, log, wfNamespace, hc)
								//if err != nil {
								//	log.Error(err, "Error  executing RemedyWorkflow")
								//	r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error executing RemedyWorkflow")
								//	return err
								//}
							}
						}
					} else {
						log.Info("RemedyRunsLimit and RemedyResetInterval are not set")
						//err := r.processRemedyWorkflow(ctx, log, wfNamespace, hc)
						//if err != nil {
						//	log.Error(err, "Error executing RemedyWorkflow")
						//	r.Recorder.Event(hc, v1.EventTypeWarning, "Warning", "Error executing RemedyWorkflow")
						//	return err
						//}
					}
				}
				break
			}
		}
	}

	// since the workflow has taken an unknown duration of time to complete, it's possible that its parent
	// healthcheck may no longer exist; ensure that it still does before attempting to update it and reschedule
	// see: https://book.kubebuilder.io/reference/using-finalizers.html
	if hc.ObjectMeta.DeletionTimestamp.IsZero() {
		// since the underlying workflow has completed, we update the healthcheck accordingly
		err := r.updateHealthCheckStatus(ctx, log, hc)
		if err != nil {
			log.Error(err, "Error updating healthcheck resource")
			r.Recorder.Event(hc, corev1.EventTypeWarning, "Warning", "Error updating healthcheck resource")
			if r.GetTimerByName(req.NamespacedName.Name) != nil {
				log.Info("Cancelling rescheduled workflow for this healthcheck due to deletion")
				r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Cancelling workflow for this healthcheck due to deletion")
				r.GetTimerByName(hc.GetName()).Stop()
			}
			return err
		}

		// reschedule next run of workflow
		helper := r.createSubmitWorkflowHelper(ctx, log, jobNamespace, hc)
		r.TimerLock.Lock()
		r.RepeatTimersByName[hc.GetName()] = time.AfterFunc(time.Duration(repeatAfterSec)*time.Second, helper)
		r.TimerLock.Unlock()
		log.Info("Rescheduled job workflow for next run", "namespace", jobNamespace, "name", jobName)
		r.Recorder.Event(hc, corev1.EventTypeNormal, "Normal", "Rescheduled job workflow for next run")
	}
	return nil
}

func (r *HealthCheckReconcilerV1) updateHealthCheckStatus(ctx context.Context, log logr.Logger, hc *activemonitorv1.HealthCheck) error {
	if err := r.Status().Update(ctx, hc); err != nil {
		log.Error(err, "HealthCheckV1 status could not be updated.")
		r.Recorder.Event(hc, "Warning", "Failed", fmt.Sprintf("HealthCheck %s/%s status could not be updated. %v", hc.Namespace, hc.Name, err))
		return err
	}
	return nil
}

func (r *HealthCheckReconcilerV1) ContainsEqualFoldSubstring(str, substr string) bool {
	x := strings.ToLower(str)
	y := strings.ToLower(substr)
	if strings.Contains(x, y) {
		return true
	}
	return false
}

func (r *HealthCheckReconcilerV1) IsStorageError(err error) bool {
	if r.ContainsEqualFoldSubstring(err.Error(), "StorageError: invalid object") {
		return true
	}
	return false
}

func (r *HealthCheckReconcilerV1) GetTimerByName(name string) *time.Timer {
	r.TimerLock.RLock()
	s := r.RepeatTimersByName[name]
	r.TimerLock.RUnlock()
	return s
}

// SetupWithManager as used in main package by kubebuilder v2.0.0.alpha4
func (r *HealthCheckReconcilerV1) SetupWithManager(mgr ctrl.Manager) error {
	r.kubeclient = kubernetes.NewForConfigOrDie(mgr.GetConfig())
	return ctrl.NewControllerManagedBy(mgr).
		For(&activemonitorv1.HealthCheck{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxParallel}).
		Complete(r)
}
