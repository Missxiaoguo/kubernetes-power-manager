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
package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/intel/power-optimization-library/pkg/power"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const queuetime = time.Second * 5
const FieldOwnerPowerProfileController = "powerprofile-controller"

// ValidEppValues defines the valid EPP (Energy Performance Preference) values
var ValidEppValues = []string{"performance", "balance_performance", "balance_power", "power"}

// write errors to the status filed, pass nil to clear errors, will only do update resource is valid and not being deleted
// if object already has the correct errors it will not be updated in the API
func writeUpdatedStatusErrsIfRequired(ctx context.Context, statusWriter client.SubResourceWriter, object powerv1.PowerCRWithStatusErrors, objectErrors error) error {
	var err error
	// if invalid or marked for deletion don't do anything
	if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
		return err
	}
	errList := util.UnpackErrsToStrings(objectErrors)
	// no updates are needed
	if equality.Semantic.DeepEqual(*errList, *object.GetStatusErrors()) {
		return err
	}

	// Use Patch for safer status update across multiple agents
	orig, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object does not implement client.Object")
	}
	statusPatch := client.MergeFrom(orig)

	object.SetStatusErrors(errList)
	err = statusWriter.Patch(ctx, object, statusPatch)
	if err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "failed to write status update")
	}
	return err
}

// isValidEpp checks if a certain name corresponds to a valid EPP value.
func isValidEpp(inputName string) bool {
	for _, validEpp := range ValidEppValues {
		if inputName == validEpp {
			return true
		}
	}
	return false
}

// doesNodeMatchPowerProfileSelector checks if a PowerProfile should be applied to a specific node.
func doesNodeMatchPowerProfileSelector(c client.Client, profile *powerv1.PowerProfile, nodeName string, logger *logr.Logger) (bool, error) {
	logger.V(5).Info("Checking if PowerProfile should be applied to node", "profile", profile.Spec.Name, "nodeName", nodeName)
	// If no label selector is specified, apply to all nodes.
	labelSelector := profile.Spec.NodeSelector.LabelSelector
	if len(labelSelector.MatchLabels) == 0 && len(labelSelector.MatchExpressions) == 0 {
		logger.V(5).Info("No label selector specified, applying PowerProfile to all nodes", "profile", profile.Spec.Name, "nodeName", nodeName)
		return true, nil
	}

	// Get the node to check its labels.
	node := &corev1.Node{}
	err := c.Get(context.TODO(), client.ObjectKey{Name: nodeName}, node)
	if err != nil {
		// If we can't get the node, don't apply the profile.
		logger.Error(err, "Failed to get node for selector validation", "nodeName", nodeName)
		return false, err
	}

	// Convert the label selector to a Selector and check if it matches the node.
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		logger.Error(err, "Failed to convert label selector", "selector", labelSelector)
		return false, err
	}

	// Check if the node's labels match the selector.
	res := selector.Matches(labels.Set(node.Labels))
	logger.V(5).Info("Node label check result", "nodeName", nodeName, "selector", selector, "nodeLabels", node.Labels, "result", res)
	return res, nil
}

// validateProfileAvailabilityOnNode validates that a PowerProfile exists in the cluster and is available to be used on the node
func validateProfileAvailabilityOnNode(ctx context.Context, c client.Client, profileName string, nodeName string, powerLibrary power.Host, logger *logr.Logger) (bool, error) {
	if profileName == "" {
		return true, nil
	}

	powerProfile := &powerv1.PowerProfile{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      profileName,
		Namespace: PowerNamespace,
	}, powerProfile)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if powerLibrary == nil {
		return false, fmt.Errorf("power library is nil")
	}

	// PowerProfile exists in the cluster and pool exists indicates profile is available to be used on the node.
	pool := powerLibrary.GetExclusivePool(profileName)
	if pool == nil {
		logger.Error(fmt.Errorf("pool '%s' not found", profileName), "pool not found")
		return false, nil
	}

	// Verify the node matches the PowerProfile node selector.
	match, err := doesNodeMatchPowerProfileSelector(c, powerProfile, nodeName, logger)
	if err != nil {
		logger.Error(err, "error checking if node matches power profile selector")
		return false, err
	}
	return match, nil
}

// formatIntOrStringValue extracts the actual value from IntOrString as a string
// Returns the integer value as string for Int type, or the string value for String type.
func formatIntOrStringValue(value intstr.IntOrString) string {
	if value.Type == intstr.Int {
		return strconv.Itoa(int(value.IntVal))
	}
	return value.StrVal
}

// formatIntOrStringOrEmpty formats an IntOrString pointer, returning empty string if nil
func formatIntOrStringOrEmpty(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	return formatIntOrStringValue(*value)
}

// getPowerNodeState retrieves a PowerNodeState for the given node with common validation and error handling.
// Returns (powerNodeState, nil) if found.
// Returns (nil, nil) if NotFound (caller should treat as no-op).
// Returns (nil, error) for validation errors or other Get errors.
func getPowerNodeState(ctx context.Context, c client.Client, nodeName string, logger *logr.Logger) (*powerv1.PowerNodeState, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("nodeName cannot be empty")
	}

	powerNodeStateName := fmt.Sprintf("%s-power-state", nodeName)

	powerNodeState := &powerv1.PowerNodeState{}
	err := c.Get(ctx, client.ObjectKey{Name: powerNodeStateName, Namespace: PowerNamespace}, powerNodeState)

	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("PowerNodeState not found", "powerNodeState", powerNodeStateName)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get PowerNodeState: %w", err)
	}

	return powerNodeState, nil
}

// applyPowerNodeStateProfilesStatus applies the given PowerProfiles to the PowerNodeState status using Server-Side Apply.
func applyPowerNodeStateProfilesStatus(ctx context.Context, c client.Client, powerNodeStateName string, profiles []powerv1.PowerNodeProfileStatus) error {
	patchNodeState := &powerv1.PowerNodeState{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "power.openshift.io/v1",
			Kind:       "PowerNodeState",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      powerNodeStateName,
			Namespace: PowerNamespace,
		},
		Status: powerv1.PowerNodeStateStatus{
			PowerProfiles: profiles,
		},
	}

	return c.Status().Patch(ctx, patchNodeState, client.Apply, client.FieldOwner(FieldOwnerPowerProfileController), client.ForceOwnership)
}

// updatePowerNodeStateWithProfileInfo updates the PowerNodeState CR for a given node with the validation
// results of a PowerProfile using Server-Side Apply (SSA). Uses retry.RetryOnConflict to handle concurrent updates safely.
func updatePowerNodeStateWithProfileInfo(ctx context.Context, c client.Client, nodeName string, profile *powerv1.PowerProfile, profileErrors error, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	// Serialize the PowerProfile spec to a readable config string (done once, outside retry loop).
	cstatesString := prettifyCStatesMap(profile.Spec.CStates.Names)
	if cstatesString == "" && profile.Spec.CStates.MaxLatencyUs != nil {
		cstatesString = fmt.Sprintf("maxLatency: %d", *profile.Spec.CStates.MaxLatencyUs)
	}
	config := fmt.Sprintf(
		"Min: %s, Max: %s, Governor: %s, EPP: %s, C-States: %s",
		formatIntOrStringOrEmpty(profile.Spec.PStates.Min), formatIntOrStringOrEmpty(profile.Spec.PStates.Max),
		profile.Spec.PStates.Governor, profile.Spec.PStates.Epp, cstatesString)

	// Convert errors to string slice.
	errList := util.UnpackErrsToStrings(profileErrors)

	// Create the PowerNodeProfileStatus entry for this profile.
	profileStatus := powerv1.PowerNodeProfileStatus{Name: profile.Spec.Name, Config: config, Errors: *errList}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fresh read on each retry attempt.
		powerNodeState, err := getPowerNodeState(ctx, c, nodeName, logger)
		if err != nil {
			return err
		}
		if powerNodeState == nil {
			// PowerNodeState not found, nothing to update.
			return nil
		}

		// Get the existing PowerProfiles list and update/append this profile.
		existingProfiles := make([]powerv1.PowerNodeProfileStatus, len(powerNodeState.Status.PowerProfiles))
		copy(existingProfiles, powerNodeState.Status.PowerProfiles)

		found := false
		for i, existing := range existingProfiles {
			if existing.Name == profile.Spec.Name {
				// Update existing entry (including errors).
				existingProfiles[i] = profileStatus
				found = true
				break
			}
		}
		if !found {
			// Append new entry.
			existingProfiles = append(existingProfiles, profileStatus)
		}

		// Apply the status using Server-Side Apply.
		err = applyPowerNodeStateProfilesStatus(ctx, c, powerNodeState.Name, existingProfiles)
		if err != nil {
			return err
		}

		logger.Info("Updated PowerNodeState with profile validation results",
			"powerNodeState", powerNodeState.Name,
			"profile", profile.Spec.Name,
			"errors", len(*errList))

		return nil
	})
}

// removeProfileFromPowerNodeState removes a PowerProfile entry from the PowerNodeState status.
// Uses retry.RetryOnConflict to handle concurrent updates safely.
func removeProfileFromPowerNodeState(ctx context.Context, c client.Client, nodeName string, profileName string, logger *logr.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("nodeName cannot be empty")
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fresh read on each retry attempt.
		powerNodeState, err := getPowerNodeState(ctx, c, nodeName, logger)
		if err != nil {
			return err
		}
		if powerNodeState == nil {
			// PowerNodeState not found, nothing to clean up.
			return nil
		}

		// Filter out the profile to be removed.
		updatedProfiles := make([]powerv1.PowerNodeProfileStatus, 0)
		for _, existing := range powerNodeState.Status.PowerProfiles {
			if existing.Name != profileName {
				updatedProfiles = append(updatedProfiles, existing)
			}
		}

		// If nothing changed, no need to update.
		if len(updatedProfiles) == len(powerNodeState.Status.PowerProfiles) {
			logger.V(5).Info("Profile not found in PowerNodeState, nothing to remove", "profile", profileName)
			return nil
		}

		// Apply the status using Server-Side Apply.
		err = applyPowerNodeStateProfilesStatus(ctx, c, powerNodeState.Name, updatedProfiles)
		if err != nil {
			return err
		}

		logger.Info("Removed profile from PowerNodeState",
			"powerNodeState", powerNodeState.Name,
			"profile", profileName)

		return nil
	})
}
