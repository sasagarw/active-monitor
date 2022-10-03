/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// Important: Run "make" to regenerate code after modifying this file

// HealthCheckJobSpec defines the desired state of HealthCheck
// Either RepeatAfterSec or Schedule must be defined for the health check to run
type HealthCheckJobSpec struct {
	RepeatAfterSec      int       `json:"repeatAfterSec,omitempty"`
	Description         string    `json:"description,omitempty"`
	Job                 Job       `json:"job"`
	RemedyJob           RemedyJob `json:"remedyjob,omitempty"`
	RemedyRunsLimit     int       `json:"remedyRunsLimit,omitempty"`
	RemedyResetInterval int       `json:"remedyResetInterval,omitempty"`
}

// HealthCheckJobStatus defines the observed state of HealthCheck
type HealthCheckJobStatus struct {
	ErrorMessage         string       `json:"errorMessage,omitempty"`
	RemedyErrorMessage   string       `json:"remedyErrorMessage,omitempty"`
	StartedAt            *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt           *metav1.Time `json:"finishedAt,omitempty"`
	LastFailedAt         *metav1.Time `json:"lastFailedAt,omitempty"`
	RemedyStartedAt      *metav1.Time `json:"remedyTriggeredAt,omitempty"`
	RemedyFinishedAt     *metav1.Time `json:"remedyFinishedAt,omitempty"`
	RemedyLastFailedAt   *metav1.Time `json:"remedyLastFailedAt,omitempty"`
	LastFailedJob        string       `json:"lastFailedJob,omitempty"`
	LastSuccessfulJob    string       `json:"lastSuccessfulJob,omitempty"`
	SuccessCount         int          `json:"successCount,omitempty"`
	FailedCount          int          `json:"failedCount,omitempty"`
	RemedySuccessCount   int          `json:"remedySuccessCount,omitempty"`
	RemedyFailedCount    int          `json:"remedyFailedCount,omitempty"`
	RemedyTotalRuns      int          `json:"remedyTotalRuns,omitempty"`
	TotalHealthCheckRuns int          `json:"totalHealthCheckRuns,omitempty"`
	Status               string       `json:"status,omitempty"`
	RemedyStatus         string       `json:"remedyStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=healthcheckjobs,scope=Namespaced,shortName=hcj;hcjs
// +kubebuilder:printcolumn:name="LATEST STATUS",type=string,JSONPath=`.status.status`
// +kubebuilder:printcolumn:name="SUCCESS CNT  ",type=string,JSONPath=`.status.successCount`
// +kubebuilder:printcolumn:name="FAIL CNT",type=string,JSONPath=`.status.failedCount`
// +kubebuilder:printcolumn:name="REMEDY SUCCESS CNT  ",type=string,JSONPath=`.status.remedySuccessCount`
// +kubebuilder:printcolumn:name="REMEDY FAIL CNT",type=string,JSONPath=`.status.remedyFailedCount`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HealthCheckJob is the Schema for the healthchecks API
type HealthCheckJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HealthCheckJobSpec   `json:"spec,omitempty"`
	Status HealthCheckJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HealthCheckJobList contains a list of HealthCheck
type HealthCheckJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HealthCheckJob `json:"items"`
}

// RemedyJob struct describes a Remedy job
type RemedyJob struct {
	GenerateName string             `json:"generateName,omitempty"`
	Resource     *JobResourceObject `json:"resource,omitempty"`
	Timeout      int                `json:"timeout,omitempty"`
}

func (w RemedyJob) IsEmpty() bool {
	return reflect.DeepEqual(w, RemedyWorkflow{})
}

// Job struct describes a job resource
type Job struct {
	GenerateName string             `json:"generateName,omitempty"`
	Resource     *JobResourceObject `json:"resource,omitempty"`
	Timeout      int                `json:"timeout,omitempty"`
}

// JobResourceObject is the resource object to create on kubernetes
type JobResourceObject struct {
	// Namespace in which to create this object
	// defaults to the service account namespace
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Schedule defines schedule rules to run HealthCheck
	// cron expressions can be found here: https://godoc.org/github.com/robfig/cron
	Schedule                string            `json:"schedule,omitempty"`
	ActiveDeadlineSeconds   *int64            `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32            `json:"ttlSecondsAfterFinished,omitempty"`
	BackoffLimit            *int32            `json:"backoffLimit,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
	Annotations             map[string]string `json:"annotations,omitempty"`
	Template                TemplateSpec      `json:"template"`
}

type TemplateSpec struct {
	Spec Spec `json:"spec"`
}

type Spec struct {
	Containers    []ContainerSpec      `json:"containers"`
	Tolerations   []corev1.Toleration  `json:"tolerations,omitempty"`
	RestartPolicy corev1.RestartPolicy `json:"restartPolicy"`
}

type ContainerSpec struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
}

func init() {
	SchemeBuilder.Register(&HealthCheckJob{}, &HealthCheckJobList{})
}
