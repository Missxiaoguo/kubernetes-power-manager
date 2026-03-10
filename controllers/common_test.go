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

	"github.com/go-logr/logr"
	powerv1 "github.com/openshift-kni/kubernetes-power-manager/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func Test_getPowerNodeState(t *testing.T) {
	ctx := context.Background()
	logger := logr.Discard()

	tcases := []struct {
		testCase        string
		nodeName        string
		setupMockClient func() client.Client
		expectError     bool
		expectedErrMsg  string
		expectNilState  bool
	}{
		{
			testCase:       "Empty nodeName should return error",
			nodeName:       "",
			expectError:    true,
			expectedErrMsg: "nodeName cannot be empty",
			setupMockClient: func() client.Client {
				return nil
			},
		},
		{
			testCase:       "PowerNodeState not found should return nil state",
			nodeName:       "test-node",
			expectError:    false,
			expectNilState: true,
			setupMockClient: func() client.Client {
				clientMockObj := mock.Mock{}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				notFoundErr := apierrors.NewNotFound(schema.GroupResource{Group: "power.openshift.io", Resource: "powernodestates"}, "test-node-power-state")
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(notFoundErr)
				return mockClient
			},
		},
		{
			testCase:       "Error getting PowerNodeState (not NotFound)",
			nodeName:       "test-node",
			expectError:    true,
			expectedErrMsg: "failed to get PowerNodeState",
			setupMockClient: func() client.Client {
				clientMockObj := mock.Mock{}
				getErr := fmt.Errorf("server error")
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(getErr)
				return mockClient
			},
		},
		{
			testCase:       "Successfully get PowerNodeState",
			nodeName:       "test-node",
			expectError:    false,
			expectNilState: false,
			setupMockClient: func() client.Client {
				clientMockObj := mock.Mock{}
				powerNodeState := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name:      "test-node-power-state",
						Namespace: PowerNamespace,
					},
				}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*pns = *powerNodeState
						}
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			mockClient := tc.setupMockClient()
			state, err := getPowerNodeState(ctx, mockClient, tc.nodeName, &logger)

			if tc.expectError {
				assert.Error(t, err, tc.testCase)
				if tc.expectedErrMsg != "" {
					assert.Contains(t, err.Error(), tc.expectedErrMsg, tc.testCase)
				}
			} else {
				assert.Nil(t, err, tc.testCase)
				if tc.expectNilState {
					assert.Nil(t, state, tc.testCase)
				} else {
					assert.NotNil(t, state, tc.testCase)
					assert.Equal(t, "test-node-power-state", state.Name)
				}
			}
		})
	}
}

func Test_applyPowerNodeStateProfilesStatus(t *testing.T) {
	ctx := context.Background()

	tcases := []struct {
		testCase            string
		powerNodeStateName  string
		profiles            []powerv1.PowerNodeProfileStatus
		setupMockClient     func(*powerv1.PowerNodeState) client.Client
		expectError         bool
		verifyPatchedObject func(*testing.T, *powerv1.PowerNodeState)
	}{
		{
			testCase:           "Successfully apply profiles status",
			powerNodeStateName: "test-node-power-state",
			profiles: []powerv1.PowerNodeProfileStatus{
				{Name: "profile-1", Config: "config-1", Errors: []string{}},
			},
			expectError: false,
			setupMockClient: func(capturedPatch *powerv1.PowerNodeState) client.Client {
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
				clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion)
				assert.Equal(t, "PowerNodeState", pns.Kind)
				assert.Equal(t, "test-node-power-state", pns.Name)
				assert.Equal(t, PowerNamespace, pns.Namespace)
				assert.Len(t, pns.Status.PowerProfiles, 1)
				assert.Equal(t, "profile-1", pns.Status.PowerProfiles[0].Name)
			},
		},
		{
			testCase:           "Error patching status",
			powerNodeStateName: "test-node-power-state",
			profiles:           []powerv1.PowerNodeProfileStatus{},
			expectError:        true,
			setupMockClient: func(capturedPatch *powerv1.PowerNodeState) client.Client {
				clientMockObj := mock.Mock{}
				patchErr := fmt.Errorf("patch error")
				clientFuncs := interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, patch, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(patchErr)
				return mockClient
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			capturedPatch := &powerv1.PowerNodeState{}
			mockClient := tc.setupMockClient(capturedPatch)
			err := applyPowerNodeStateProfilesStatus(ctx, mockClient, tc.powerNodeStateName, tc.profiles)

			if tc.expectError {
				assert.Error(t, err, tc.testCase)
			} else {
				assert.Nil(t, err, tc.testCase)
				if tc.verifyPatchedObject != nil {
					tc.verifyPatchedObject(t, capturedPatch)
				}
			}
		})
	}
}

func Test_updatePowerNodeStateWithProfileInfo(t *testing.T) {
	ctx := context.Background()
	logger := logr.Discard()

	profile := &powerv1.PowerProfile{
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

	tcases := []struct {
		testCase                string
		nodeName                string
		profileErr              error
		existingProfiles        []powerv1.PowerNodeProfileStatus
		setupMockClient         func(*powerv1.PowerNodeState) client.Client
		expectError             bool
		expectedErrMsg          string
		verifyPatchedObject     func(*testing.T, *powerv1.PowerNodeState)
		shouldVerifyPatchObject bool
	}{
		{
			testCase:                "Append new profile with no errors",
			nodeName:                "test-node",
			existingProfiles:        []powerv1.PowerNodeProfileStatus{},
			expectError:             false,
			shouldVerifyPatchObject: true,
			setupMockClient: func(capturedPatch *powerv1.PowerNodeState) client.Client {
				clientMockObj := mock.Mock{}
				powerNodeState := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name:      "test-node-power-state",
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStateStatus{
						PowerProfiles: []powerv1.PowerNodeProfileStatus{},
					},
				}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*pns = *powerNodeState
						}
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
					SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						// Capture the patched object
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*capturedPatch = *pns
						}
						return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, patch, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				// Verify TypeMeta is set for SSA
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion, "APIVersion should be set for SSA")
				assert.Equal(t, "PowerNodeState", pns.Kind, "Kind should be set for SSA")

				// Verify ObjectMeta
				assert.Equal(t, "test-node-power-state", pns.Name)
				assert.Equal(t, PowerNamespace, pns.Namespace)

				// Verify new profile was appended
				assert.Len(t, pns.Status.PowerProfiles, 1, "Should have 1 profile")
				assert.Equal(t, "test-profile", pns.Status.PowerProfiles[0].Name)
				assert.Equal(t, "Min: 2000, Max: 3000, Governor: powersave, EPP: balance_performance, C-States: enabled: C1,C1E; disabled: C6", pns.Status.PowerProfiles[0].Config)
				assert.Empty(t, pns.Status.PowerProfiles[0].Errors, "Should have no errors")
			},
		},
		{
			testCase:                "Update existing profile with errors",
			nodeName:                "test-node",
			profileErr:              fmt.Errorf("invalid P-states configuration: max frequency (2000) cannot be lower than the min frequency (3000)"),
			shouldVerifyPatchObject: true,
			existingProfiles: []powerv1.PowerNodeProfileStatus{
				{
					Name:   "test-profile",
					Config: "Min: 1000, Max: 2000, Governor: performance, EPP: performance",
					Errors: []string{},
				},
			},
			expectError: false,
			setupMockClient: func(capturedPatch *powerv1.PowerNodeState) client.Client {
				clientMockObj := mock.Mock{}
				powerNodeState := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name:      "test-node-power-state",
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStateStatus{
						PowerProfiles: []powerv1.PowerNodeProfileStatus{
							{
								Name:   "test-profile",
								Config: "Min: 1000, Max: 2000, Governor: performance, EPP: performance",
								Errors: []string{},
							},
						},
					},
				}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*pns = *powerNodeState
						}
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
					SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						// Capture the patched object
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*capturedPatch = *pns
						}
						return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, patch, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				// Verify TypeMeta is set for SSA
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion, "APIVersion should be set for SSA")
				assert.Equal(t, "PowerNodeState", pns.Kind, "Kind should be set for SSA")

				// Verify profile was updated (not appended)
				assert.Len(t, pns.Status.PowerProfiles, 1, "Should still have 1 profile")
				assert.Equal(t, "test-profile", pns.Status.PowerProfiles[0].Name)
				// Config should be updated
				assert.Equal(t, "Min: 2000, Max: 3000, Governor: powersave, EPP: balance_performance, C-States: enabled: C1,C1E; disabled: C6", pns.Status.PowerProfiles[0].Config)
				// Errors should be updated
				assert.Len(t, pns.Status.PowerProfiles[0].Errors, 1, "Should have 1 error")
				assert.Equal(t, "invalid P-states configuration: max frequency (2000) cannot be lower than the min frequency (3000)", pns.Status.PowerProfiles[0].Errors[0])
			},
		},
		// Note: "Empty nodeName", "PowerNodeState not found", "Error getting PowerNodeState", and
		// "Error patching status" are tested in Test_getPowerNodeState and Test_applyPowerNodeStateProfilesStatus
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			capturedPatch := &powerv1.PowerNodeState{}
			mockClient := tc.setupMockClient(capturedPatch)
			err := updatePowerNodeStateWithProfileInfo(ctx, mockClient, tc.nodeName, profile, tc.profileErr, &logger)

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

func Test_removeProfileFromPowerNodeState(t *testing.T) {
	ctx := context.Background()
	logger := logr.Discard()

	tcases := []struct {
		testCase                string
		nodeName                string
		profileName             string
		setupMockClient         func(*powerv1.PowerNodeState) client.Client
		expectError             bool
		expectedErrMsg          string
		verifyPatchedObject     func(*testing.T, *powerv1.PowerNodeState)
		shouldVerifyPatchObject bool
	}{
		{
			testCase:    "Profile not in list should return nil (no change)",
			nodeName:    "test-node",
			profileName: "test-profile",
			expectError: false,
			setupMockClient: func(pns *powerv1.PowerNodeState) client.Client {
				clientMockObj := mock.Mock{}
				powerNodeState := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name:      "test-node-power-state",
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStateStatus{
						PowerProfiles: []powerv1.PowerNodeProfileStatus{
							{
								Name:   "other-profile",
								Config: "Min: 1000, Max: 2000, Governor: performance, EPP: performance",
								Errors: []string{},
							},
						},
					},
				}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*pns = *powerNodeState
						}
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
		},
		{
			testCase:                "Successfully remove profile from list",
			nodeName:                "test-node",
			profileName:             "test-profile",
			expectError:             false,
			shouldVerifyPatchObject: true,
			setupMockClient: func(capturedPatch *powerv1.PowerNodeState) client.Client {
				clientMockObj := mock.Mock{}
				powerNodeState := &powerv1.PowerNodeState{
					ObjectMeta: v1.ObjectMeta{
						Name:      "test-node-power-state",
						Namespace: PowerNamespace,
					},
					Status: powerv1.PowerNodeStateStatus{
						PowerProfiles: []powerv1.PowerNodeProfileStatus{
							{
								Name:   "profile-1",
								Config: "Min: 1000, Max: 2000, Governor: performance, EPP: performance",
								Errors: []string{},
							},
							{
								Name:   "test-profile",
								Config: "Min: 2000, Max: 3000, Governor: powersave, EPP: balance_performance",
								Errors: []string{},
							},
							{
								Name:   "profile-3",
								Config: "Min: 1500, Max: 2500, Governor: performance, EPP: performance",
								Errors: []string{},
							},
						},
					},
				}
				clientFuncs := interceptor.Funcs{
					Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*pns = *powerNodeState
						}
						return clientMockObj.MethodCalled("Get", ctx, client, key, obj, opts).Error(0)
					},
					SubResourcePatch: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						// Capture the patched object
						if pns, ok := obj.(*powerv1.PowerNodeState); ok {
							*capturedPatch = *pns
						}
						return clientMockObj.MethodCalled("SubResourcePatch", ctx, client, subResourceName, obj, patch, opts).Error(0)
					},
				}
				mockClient := fakeClient.NewClientBuilder().WithInterceptorFuncs(clientFuncs).Build()
				clientMockObj.On("Get", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				clientMockObj.On("SubResourcePatch", ctx, mock.Anything, "status", mock.Anything, mock.Anything, mock.Anything).Return(nil)
				return mockClient
			},
			verifyPatchedObject: func(t *testing.T, pns *powerv1.PowerNodeState) {
				// Verify TypeMeta is set for SSA
				assert.Equal(t, "power.openshift.io/v1", pns.APIVersion, "APIVersion should be set for SSA")
				assert.Equal(t, "PowerNodeState", pns.Kind, "Kind should be set for SSA")

				// Verify ObjectMeta
				assert.Equal(t, "test-node-power-state", pns.Name)
				assert.Equal(t, PowerNamespace, pns.Namespace)

				// Verify test-profile was removed, leaving profile-1 and profile-3
				assert.Len(t, pns.Status.PowerProfiles, 2, "Should have 2 profiles after removal")

				// Check that test-profile is not in the list
				profileNames := make([]string, len(pns.Status.PowerProfiles))
				for i, profile := range pns.Status.PowerProfiles {
					profileNames[i] = profile.Name
				}
				assert.NotContains(t, profileNames, "test-profile", "test-profile should be removed")
				assert.Contains(t, profileNames, "profile-1", "profile-1 should remain")
				assert.Contains(t, profileNames, "profile-3", "profile-3 should remain")

				// Verify order is preserved
				assert.Equal(t, "profile-1", pns.Status.PowerProfiles[0].Name)
				assert.Equal(t, "profile-3", pns.Status.PowerProfiles[1].Name)
			},
		},
		// Note: "Empty nodeName", "PowerNodeState not found", "Error getting PowerNodeState", and
		// "Error patching status" are tested in Test_getPowerNodeState and Test_applyPowerNodeStateProfilesStatus
	}

	for _, tc := range tcases {
		t.Run(tc.testCase, func(t *testing.T) {
			capturedPatch := &powerv1.PowerNodeState{}
			mockClient := tc.setupMockClient(capturedPatch)
			err := removeProfileFromPowerNodeState(ctx, mockClient, tc.nodeName, tc.profileName, &logger)

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

// Helper function to create IntOrString from int
func intStrFromInt(val int) *intstr.IntOrString {
	result := intstr.FromInt(val)
	return &result
}
