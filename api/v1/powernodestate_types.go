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
)

// PowerNodeStateSpec defines the desired state of PowerNodeState
// This is intentionally empty as PowerNodeState is a status-only CRD
type PowerNodeStateSpec struct {
}

// PowerNodeStateStatus defines the observed state of PowerNodeState
// Each field is owned by a specific controller using Server-Side Apply (SSA)
type PowerNodeStateStatus struct {
	// PowerProfiles contains the status of power profiles on this node
	// Owned by: PowerProfile controller
	// +optional
	PowerProfiles []PowerNodeProfileStatus `json:"powerProfiles,omitempty"`

	// CPUPools contains the status of CPU pools on this node
	// Owned by: PowerNodeConfig controller (shared, reserved) and PowerPod controller (exclusive)
	// +optional
	CPUPools CPUPoolsStatus `json:"cpuPools,omitempty"`

	// Uncore contains the status of uncore frequency configuration on this node
	// Owned by: Uncore controller
	// +optional
	Uncore *NodeUncoreStatus `json:"uncore,omitempty"`

	// CustomDevices include alternative devices that represent other resources
	// Owned by: PowerConfig controller
	// +optional
	CustomDevices []string `json:"customDevices,omitempty"`
}

// PowerNodeProfileStatus represents the status of a power profile on this node
type PowerNodeProfileStatus struct {
	// Name is the name of the PowerProfile CR
	Name string `json:"name"`

	// Config is the configuration of the PowerProfile
	Config string `json:"config"`

	// Errors contains any errors encountered while applying this profile on this node
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// CPUPoolsStatus contains the status of all CPU pools on this node
type CPUPoolsStatus struct {
	// Shared contains the status of the shared CPU pool
	// Owned by: PowerNodeConfig controller (shared, reserved)
	// +optional
	Shared *SharedCPUPoolStatus `json:"shared,omitempty"`

	// Reserved contains the status of the reserved CPU pools
	// Owned by: PowerNodeConfig controller (shared, reserved)
	// +optional
	Reserved []ReservedCPUPoolStatus `json:"reserved,omitempty"`

	// Exclusive contains the status of exclusive CPU pools
	// Owned by: PowerPod controller (exclusive)
	// +optional
	Exclusive []ExclusiveCPUPoolStatus `json:"exclusive,omitempty"`
}

// SharedCPUPoolStatus represents the status of the shared CPU pool
type SharedCPUPoolStatus struct {
	// PowerProfile is the name of the PowerProfile applied to this pool
	PowerProfile string `json:"powerProfile"`

	// PowerNodeConfig is the name of the PowerNodeConfig applied to this pool
	PowerNodeConfig string `json:"powerNodeConfig"`

	// CPUIDs are the CPU IDs in this pool
	CPUIDs []uint `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring this pool
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// ReservedCPUPoolStatus represents the status of a reserved CPU pool
type ReservedCPUPoolStatus struct {
	// PowerProfileCPUs are the PowerProfile and CPUIDs in this pool
	PowerProfileCPUs []PowerProfileCPUs `json:"powerProfileCPUs"`

	// PowerNodeConfig is the name of the PowerNodeConfig applied to this pool
	PowerNodeConfig string `json:"powerNodeConfig"`
}

type PowerProfileCPUs struct {
	// PowerProfile is the name of the PowerProfile applied to this pool
	PowerProfile string `json:"powerProfile"`

	// CPUIDs are the CPU IDs in this pool
	CPUIDs []uint `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring this pool
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// ExclusiveCPUPoolStatus represents the status of an exclusive CPU pool
// assigned to a container
type ExclusiveCPUPoolStatus struct {
	ExclusiveCPUInfo []ExclusiveCPUInfo `json:"exclusiveCPUInfo"`
}

// ExclusiveCPUInfo contains information about an exclusive CPU pool
type ExclusiveCPUInfo struct {
	// PowerProfile is the name of the PowerProfile used in these containers
	PowerProfile string `json:"powerProfile"`

	// PowerContainers contains information about the containers using exclusive CPUs and the PowerProfile
	PowerContainers []PowerContainer `json:"powerContainers"`
}

// PowerContainer contains information about a container using exclusive CPUs
type PowerContainer struct {
	// Name is the name of the container
	Name string `json:"name"`

	// ID is the ID of the container
	ID string `json:"id"`

	// Pod is the name of the pod the container is running in
	Pod string `json:"pod"`

	// PodUID is the UID of the pod the container is running in
	PodUID string `json:"podUID"`

	// CPUIDs are the CPU IDs assigned to the container
	CPUIDs []uint `json:"cpuIDs"`

	// Errors contains any errors encountered while configuring the container
	Errors []string `json:"errors,omitempty"`
}

// NodeUncoreStatus represents the status of uncore frequency configuration on a node
type NodeUncoreStatus struct {
	// Name is the name of the uncore frequency configuration
	Name string `json:"name"`

	// Config is the configuration of the uncore frequency configuration
	Config string `json:"config"`

	// Errors contains any errors encountered while configuring uncore frequency
	Errors []string `json:"errors,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pns

// PowerNodeState is the Schema for the powernodestates API
// It provides per-node status for power management configuration
type PowerNodeState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PowerNodeStateSpec   `json:"spec,omitempty"`
	Status PowerNodeStateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PowerNodeStateList contains a list of PowerNodeState
type PowerNodeStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PowerNodeState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PowerNodeState{}, &PowerNodeStateList{})
}
