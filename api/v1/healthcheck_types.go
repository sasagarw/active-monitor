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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// Important: Run "make" to regenerate code after modifying this file

// HealthCheckSpec defines the desired state of HealthCheck
// Either RepeatAfterSec or Schedule must be defined for the health check to run
type HealthCheckSpec struct {
	RepeatAfterSec      int             `json:"repeatAfterSec,omitempty"`
	Description         string          `json:"description,omitempty"`
	CronJobWorkflow     CronJobWorkflow `json:"cronjobworkflow"`
	RemedyWorkflow      RemedyWorkflow  `json:"remedyworkflow,omitempty"`
	RemedyRunsLimit     int             `json:"remedyRunsLimit,omitempty"`
	RemedyResetInterval int             `json:"remedyResetInterval,omitempty"`
}

// HealthCheckStatus defines the observed state of HealthCheck
type HealthCheckStatus struct {
	ErrorMessage           string       `json:"errorMessage,omitempty"`
	RemedyErrorMessage     string       `json:"remedyErrorMessage,omitempty"`
	StartedAt              *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt             *metav1.Time `json:"finishedAt,omitempty"`
	LastFailedAt           *metav1.Time `json:"lastFailedAt,omitempty"`
	RemedyStartedAt        *metav1.Time `json:"remedyTriggeredAt,omitempty"`
	RemedyFinishedAt       *metav1.Time `json:"remedyFinishedAt,omitempty"`
	RemedyLastFailedAt     *metav1.Time `json:"remedyLastFailedAt,omitempty"`
	LastFailedWorkflow     string       `json:"lastFailedWorkflow,omitempty"`
	LastSuccessfulWorkflow string       `json:"lastSuccessfulWorkflow,omitempty"`
	SuccessCount           int          `json:"successCount,omitempty"`
	FailedCount            int          `json:"failedCount,omitempty"`
	RemedySuccessCount     int          `json:"remedySuccessCount,omitempty"`
	RemedyFailedCount      int          `json:"remedyFailedCount,omitempty"`
	RemedyTotalRuns        int          `json:"remedyTotalRuns,omitempty"`
	TotalHealthCheckRuns   int          `json:"totalHealthCheckRuns,omitempty"`
	Status                 string       `json:"status,omitempty"`
	RemedyStatus           string       `json:"remedyStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=healthchecks,scope=Namespaced,shortName=hc;hcs
// +kubebuilder:printcolumn:name="LATEST STATUS",type=string,JSONPath=`.status.status`
// +kubebuilder:printcolumn:name="SUCCESS CNT  ",type=string,JSONPath=`.status.successCount`
// +kubebuilder:printcolumn:name="FAIL CNT",type=string,JSONPath=`.status.failedCount`
// +kubebuilder:printcolumn:name="REMEDY SUCCESS CNT  ",type=string,JSONPath=`.status.remedySuccessCount`
// +kubebuilder:printcolumn:name="REMEDY FAIL CNT",type=string,JSONPath=`.status.remedyFailedCount`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HealthCheck is the Schema for the healthchecks API
type HealthCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HealthCheckSpec   `json:"spec,omitempty"`
	Status HealthCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HealthCheckList contains a list of HealthCheck
type HealthCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HealthCheck `json:"items"`
}

// Workflow struct describes a Remedy workflow
type RemedyWorkflow struct {
	GenerateName string          `json:"generateName,omitempty"`
	Resource     *ResourceObject `json:"resource,omitempty"`
	Timeout      int             `json:"workflowtimeout,omitempty"`
}

func (w RemedyWorkflow) IsEmpty() bool {
	return reflect.DeepEqual(w, RemedyWorkflow{})
}

// CronJobWorkflow struct describes a cronjob workflow resource
type CronJobWorkflow struct {
	GenerateName string          `json:"generateName,omitempty"`
	Resource     *ResourceObject `json:"resource,omitempty"`
	Timeout      int             `json:"workflowtimeout,omitempty"`
}

// ResourceObject is the resource object to create on kubernetes
type ResourceObject struct {
	// Namespace in which to create this object
	// defaults to the service account namespace
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Schedule defines schedule rules to run HealthCheck
	// cron expressions can be found here: https://godoc.org/github.com/robfig/cron
	Schedule                string            `json:"schedule,omitempty"`
	Parallelism             *int32            `json:"parallelism,omitempty"`
	Completions             *int32            `json:"completions,omitempty"`
	ActiveDeadlineSeconds   *int64            `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32            `json:"ttlSecondsAfterFinished,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
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
	SchemeBuilder.Register(&HealthCheck{}, &HealthCheckList{})
}
