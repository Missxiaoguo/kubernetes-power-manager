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
	"testing"
	"time"

	"github.com/go-logr/logr"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeClient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func Test_writeStatusErrors(t *testing.T) {
	var object powerv1.PowerCRWithStatusErrors
	var errorList error
	var ctx = context.Background()
	clientMockObj := mock.Mock{}
	clientFuncs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, opts).Error(0)
		},
	}
	clientStatusWriter := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build().Status()

	object = &powerv1.Uncore{}
	assert.Nil(t, writeUpdatedStatusErrsIfRequired(ctx, nil, object, nil), "invalid object should return nil without doing anything")

	object = &powerv1.PowerWorkload{
		ObjectMeta: v1.ObjectMeta{
			UID: "not empty",
		},
	}
	clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", &powerv1.PowerWorkload{
		ObjectMeta: v1.ObjectMeta{
			UID: "not empty",
		},
		Status: powerv1.PowerWorkloadStatus{
			StatusErrors: powerv1.StatusErrors{
				Errors: []string{"err1"},
			},
		},
	}, mock.Anything, mock.Anything).Return(nil)
	errorList = fmt.Errorf("err1")
	assert.Nil(t, writeUpdatedStatusErrsIfRequired(ctx, clientStatusWriter, object, errorList), "API should get updated with object with errors")

	object = &powerv1.PowerWorkload{
		ObjectMeta: v1.ObjectMeta{
			UID: "not empty",
		},
	}

	clientMockObj = mock.Mock{}
	updateErr := fmt.Errorf("update error")
	clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(updateErr)
	assert.ErrorIs(t, writeUpdatedStatusErrsIfRequired(ctx, clientStatusWriter, object, fmt.Errorf("err2")), updateErr, "error updating APi should return that error")

}

func Test_addPowerNodeStatusProfileEntry(t *testing.T) {
	ctx := context.Background()
	logger := logr.Discard()

	defaultProfile := &powerv1.PowerProfile{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-profile",
			Namespace: PowerNamespace,
		},
		Spec: powerv1.PowerProfileSpec{
			Name: "test-profile",
			PStates: powerv1.PStatesConfig{
				Min:      intStrFromInt(2000),
				Max:      intStrFromInt(3000),
				Governor: "powersave",
				Epp:      "balance_performance",
			},
			CStates: powerv1.CStatesConfig{
				Names: map[string]bool{
					"C1":  true,
					"C1E": true,
					"C6":  false,
				},
			},
		},
	}

	// buildMockClient creates a fake client whose SubResourcePatch captures the
	// patched object into capturedPatch and returns patchErr.
	buildMockClient := func(ctx context.Context, capturedPatch *powerv1.PowerNodeState, patchErr error) client.Client {
		clientMockObj := mock.Mock{}
		clientFuncs := interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if pns, ok := obj.(*powerv1.PowerNodeState); ok {
					*capturedPatch = *pns
				}
				return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, patch, opts).Error(0)
			},
		}
		mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
		clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(patchErr)
		return mockClient
	}

	tcases := []struct {
		testCase                string
		nodeName                string
		profile                 *powerv1.PowerProfile
		profileErr              error
		patchErr                error
		expectError             bool
		expectedErrMsg          string
		verifyPatchedObject     func(*testing.T, *powerv1.PowerNodeState)
		shouldVerifyPatchObject bool
	}{
		{
			testCase:       "Empty nodeName should return error",
			nodeName:       "",
			expectError:    true,
			expectedErrMsg: "nodeName cannot be empty",
		},
		{
			testCase:                "Append new profile with no errors",
			nodeName:                "test-node",
			expectError:             false,
			shouldVerifyPatchObject: true,
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				// Verify TypeMeta and ObjectMeta are set correctly.
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion, "APIVersion should be set for SSA")
				assert.Equal(t, "PowerNodeState", pns.Kind, "Kind should be set for SSA")
				assert.Equal(t, "test-node-power-state", pns.Name)
				assert.Equal(t, PowerNamespace, pns.Namespace)

				// Verify patch payload contains the expected profile.
				assert.Len(t, pns.Status.PowerProfiles, 1, "Should have 1 profile")
				assert.Equal(t, "test-profile", pns.Status.PowerProfiles[0].Name)
				assert.Equal(t, "Min: 2000, Max: 3000, Governor: powersave, EPP: balance_performance, C-States: enabled: C1,C1E; disabled: C6", pns.Status.PowerProfiles[0].Config)
				assert.Empty(t, pns.Status.PowerProfiles[0].Errors, "Should have no errors")
			},
		},
		{
			testCase:                "Apply profile with errors",
			nodeName:                "test-node",
			profileErr:              fmt.Errorf("invalid P-states configuration: max frequency (2000) cannot be lower than the min frequency (3000)"),
			shouldVerifyPatchObject: true,
			expectError:             false,
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				// Verify TypeMeta and ObjectMeta are set for SSA.
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion, "APIVersion should be set for SSA")
				assert.Equal(t, "PowerNodeState", pns.Kind, "Kind should be set for SSA")
				assert.Equal(t, "test-node-power-state", pns.Name)
				assert.Equal(t, PowerNamespace, pns.Namespace)

				// Verify the profile status patch contains the expected errors.
				assert.Len(t, pns.Status.PowerProfiles, 1, "Should have 1 profile")
				assert.Equal(t, "test-profile", pns.Status.PowerProfiles[0].Name)
				assert.Equal(t, "Min: 2000, Max: 3000, Governor: powersave, EPP: balance_performance, C-States: enabled: C1,C1E; disabled: C6", pns.Status.PowerProfiles[0].Config)
				assert.Len(t, pns.Status.PowerProfiles[0].Errors, 1, "Should have 1 error")
				assert.Equal(t, "invalid P-states configuration: max frequency (2000) cannot be lower than the min frequency (3000)", pns.Status.PowerProfiles[0].Errors[0])
			},
		},
		{
			testCase:    "PowerNodeState not found should return error to requeue",
			nodeName:    "test-node",
			patchErr:    apierrors.NewNotFound(schema.GroupResource{Group: "power.openshift.io", Resource: "powernodestates"}, "test-node-power-state"),
			expectError: true,
		},
		{
			testCase:       "Error patching status should return error",
			nodeName:       "test-node",
			patchErr:       fmt.Errorf("patch error"),
			expectError:    true,
			expectedErrMsg: "patch error",
		},
		{
			testCase:                "Profile with CpuScalingPolicy includes policy in config string",
			nodeName:                "test-node",
			expectError:             false,
			shouldVerifyPatchObject: true,
			profile: &powerv1.PowerProfile{
				ObjectMeta: v1.ObjectMeta{
					Name:      "dpdk-profile",
					Namespace: PowerNamespace,
				},
				Spec: powerv1.PowerProfileSpec{
					Name: "dpdk-profile",
					PStates: powerv1.PStatesConfig{
						Min:      intStrFromInt(2000),
						Max:      intStrFromInt(3000),
						Governor: "userspace",
						Epp:      "performance",
					},
					CpuScalingPolicy: &powerv1.CpuScalingPolicy{
						WorkloadType:               "polling-dpdk",
						SamplePeriod:               &v1.Duration{Duration: 10 * time.Millisecond},
						CooldownPeriod:             &v1.Duration{Duration: 30 * time.Millisecond},
						TargetUsage:                intPtr(80),
						AllowedUsageDifference:     intPtr(5),
						AllowedFrequencyDifference: intPtr(25),
						ScalePercentage:            intPtr(50),
						FallbackFreqPercent:        intPtr(0),
					},
				},
			},
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				assert.Len(t, pns.Status.PowerProfiles, 1, "Should have 1 profile")
				assert.Equal(t, "dpdk-profile", pns.Status.PowerProfiles[0].Name)
				expectedConfig := `Min: 2000, Max: 3000, Governor: userspace, EPP: performance, C-States: , CpuScalingPolicy: {"workloadType":"polling-dpdk","samplePeriod":"10ms","cooldownPeriod":"30ms","targetUsage":80,"allowedUsageDifference":5,"allowedFrequencyDifference":25,"scalePercentage":50,"fallbackFreqPercent":0}`
				assert.Equal(t, expectedConfig, pns.Status.PowerProfiles[0].Config)
				assert.Empty(t, pns.Status.PowerProfiles[0].Errors)
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			capturedPatch := &powerv1.PowerNodeState{}
			mockClient := buildMockClient(ctx, capturedPatch, tc.patchErr)
			profile := tc.profile
			if profile == nil {
				profile = defaultProfile
			}
			err := addPowerNodeStatusProfileEntry(ctx, mockClient, tc.nodeName, profile, tc.profileErr, &logger)

			if tc.expectError {
				assert.Error(t, err, tc.testCase)
				if tc.expectedErrMsg != "" {
					assert.Contains(t, err.Error(), tc.expectedErrMsg, tc.testCase)
				}
			} else {
				assert.Nil(t, err, tc.testCase)
				// Verify the patched object if verification is expected
				if tc.shouldVerifyPatchObject && tc.verifyPatchedObject != nil {
					tc.verifyPatchedObject(t, capturedPatch)
				}
			}
		})
	}
}

func Test_formatCpuScalingPolicy(t *testing.T) {
	tcases := []struct {
		name        string
		policy      *powerv1.CpuScalingPolicy
		expected    string
		expectError bool
	}{
		{
			name:     "Nil policy returns empty string",
			policy:   nil,
			expected: "",
		},
		{
			name:     "Empty policy returns empty string",
			policy:   &powerv1.CpuScalingPolicy{},
			expected: "",
		},
		{
			name: "Full policy with all fields",
			policy: &powerv1.CpuScalingPolicy{
				WorkloadType:               "polling-dpdk",
				SamplePeriod:               &v1.Duration{Duration: 10 * time.Millisecond},
				CooldownPeriod:             &v1.Duration{Duration: 30 * time.Millisecond},
				TargetUsage:                intPtr(80),
				AllowedUsageDifference:     intPtr(5),
				AllowedFrequencyDifference: intPtr(25),
				ScalePercentage:            intPtr(50),
				FallbackFreqPercent:        intPtr(0),
			},
			expected: `{"workloadType":"polling-dpdk","samplePeriod":"10ms","cooldownPeriod":"30ms","targetUsage":80,"allowedUsageDifference":5,"allowedFrequencyDifference":25,"scalePercentage":50,"fallbackFreqPercent":0}`,
		},
		{
			name: "Policy with partial fields omits nil fields",
			policy: &powerv1.CpuScalingPolicy{
				WorkloadType:   "polling-dpdk",
				SamplePeriod:   &v1.Duration{Duration: 20 * time.Millisecond},
				CooldownPeriod: &v1.Duration{Duration: 50 * time.Millisecond},
				TargetUsage:    intPtr(90),
			},
			expected: `{"workloadType":"polling-dpdk","samplePeriod":"20ms","cooldownPeriod":"50ms","targetUsage":90}`,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := formatCpuScalingPolicy(tc.policy)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func Test_removePowerNodeStatusProfileEntry(t *testing.T) {
	ctx := context.Background()
	logger := logr.Discard()
	scheme := runtime.NewScheme()
	_ = powerv1.AddToScheme(scheme)
	_ = v1.AddMetaToScheme(scheme)

	tcases := []struct {
		testCase            string
		nodeName            string
		profileName         string
		setupMockClient     func() client.Client
		expectError         bool
		expectedErrMsg      string
		capturedObj         *client.Object
		verifyPatchedObject func(*testing.T, client.Object)
	}{
		{
			testCase:    "Empty nodeName should return error",
			nodeName:    "",
			profileName: "test-profile",
			expectError: true,
			setupMockClient: func() client.Client {
				return fakeClient.NewClientBuilder().Build()
			},
			expectedErrMsg: "nodeName cannot be empty",
		},
		{
			testCase:    "PowerNodeState not found should return nil",
			nodeName:    "test-node",
			profileName: "test-profile",
			expectError: false,
			setupMockClient: func() client.Client {
				return fakeClient.NewClientBuilder().WithScheme(scheme).Build()
			},
		},
		{
			testCase:    "Field manager already removed should return nil",
			nodeName:    "test-node",
			profileName: "test-profile",
			expectError: false,
			setupMockClient: func() client.Client {
				pns := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name: "test-node-power-state", Namespace: PowerNamespace,
					},
				}
				return fakeClient.NewClientBuilder().WithScheme(scheme).WithObjects(pns).Build()
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			mockClient := tc.setupMockClient()
			err := removePowerNodeStatusProfileEntry(ctx, mockClient, tc.nodeName, tc.profileName, &logger)

			if tc.expectError {
				assert.Error(t, err, tc.testCase)
				if tc.expectedErrMsg != "" {
					assert.Contains(t, err.Error(), tc.expectedErrMsg, tc.testCase)
				}
			} else {
				assert.Nil(t, err, tc.testCase)
			}
		})
	}
}

// Helper function to create IntOrString from int
func intStrFromInt(val int) *intstr.IntOrString {
	result := intstr.FromInt(val)
	return &result
}
