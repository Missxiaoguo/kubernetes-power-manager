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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PowerProfileSpec defines the desired state of PowerProfile
type PowerProfileSpec struct {
	// Important: Run "make" to regenerate code after modifying this file

	// The name of the PowerProfile
	Name string `json:"name"`

	Shared bool `json:"shared,omitempty"`

	// NodeSelector specifies which nodes this PowerProfile should be applied to
	// If empty, the profile will be applied to all the nodes.
	// +optional
	NodeSelector NodeSelector `json:"nodeSelector,omitempty"`

	// P-states configuration
	PStates PStatesConfig `json:"pstates,omitempty"`

	// C-states configuration
	CStates CStatesConfig `json:"cstates,omitempty"`

	// CpuCapacity defines the number or percentage of CPUs that can be allocated to this profile.
	// If not specified, it defaults to 100% of the available CPUs.
	// Accepted values are:
	// - A number (e.g., 5)
	// - A percentage (e.g., "10%")
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:Pattern=`^([1-9][0-9]?|100)%?$`
	// +kubebuilder:default="100%"
	CpuCapacity intstr.IntOrString `json:"cpuCapacity,omitempty"`
}

type NodeSelector struct {
	// LabelSelector is a label selector that specifies which nodes this PowerProfile should be
	// applied to.
	// +optional
	LabelSelector metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// PStatesConfig defines the CPU P-states configuration
type PStatesConfig struct {
	// Max frequency cores can run at
	Max int `json:"max,omitempty"`

	// Min frequency cores can run at
	Min int `json:"min,omitempty"`

	// The priority value associated with this Power Profile
	Epp string `json:"epp,omitempty"`

	// Governor to be used
	//+kubebuilder:default=powersave
	Governor string `json:"governor,omitempty"`
}

// CStatesConfig defines the CPU C-states configuration.
// +kubebuilder:validation:XValidation:rule="!(has(self.names) && has(self.maxLatencyUs))",message="Specify either 'names' or 'maxLatencyUs' for C-state configuration, but not both"
type CStatesConfig struct {
	// Names defines explicit C-state configuration.
	// The map key represents the C-state name (e.g., "C1", "C1E", "C6" etc.).
	// The map value represents whether the C-state should be enabled (true) or disabled (false).
	// This field is mutually exclusive with 'maxLatencyUs' — only one of 'names' or 'maxLatencyUs' may be set.
	Names map[string]bool `json:"names,omitempty"`

	// MaxLatencyUs defines the maximum latency threshold in microseconds.
	// C-states with latency higher than this threshold will be disabled.
	// This field is mutually exclusive with 'names' — only one of 'names' or 'maxLatencyUs' may be set.
	// +kubebuilder:validation:Minimum=0
	MaxLatencyUs *int `json:"maxLatencyUs,omitempty"`
}

// PowerProfileStatus defines the observed state of PowerProfile
type PowerProfileStatus struct {
	// The ID given to the power profile
	ID           int `json:"id,omitempty"`
	StatusErrors `json:",inline,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PowerProfile is the Schema for the powerprofiles API
type PowerProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PowerProfileSpec   `json:"spec,omitempty"`
	Status PowerProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PowerProfileList contains a list of PowerProfile
type PowerProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PowerProfile `json:"items"`
}

func (prfl *PowerProfile) SetStatusErrors(errs *[]string) {
	prfl.Status.Errors = *errs
}
func (prfl *PowerProfile) GetStatusErrors() *[]string {
	return &prfl.Status.Errors
}

func init() {
	SchemeBuilder.Register(&PowerProfile{}, &PowerProfileList{})
}
