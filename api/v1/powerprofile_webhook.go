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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-power-openshift-io-v1-powerprofile,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.openshift.io,resources=powerprofiles,verbs=delete,versions=v1,name=vpowerprofile.kb.io,admissionReviewVersions=v1

var powerprofilelog = logf.Log.WithName("powerprofile-webhook")

// powerProfileValidator implements admission.CustomValidator for PowerProfile.
type powerProfileValidator struct{}

// SetupPowerProfileWebhookWithManager registers the validating webhook for PowerProfile.
func SetupPowerProfileWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&PowerProfile{}).
		WithValidator(&powerProfileValidator{}).
		Complete()
}

var _ webhook.CustomValidator = &powerProfileValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete implements admission.CustomValidator.
func (v *powerProfileValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	profile, ok := obj.(*PowerProfile)
	if !ok {
		return nil, fmt.Errorf("expected PowerProfile, got %T", obj)
	}
	powerprofilelog.Info("validating delete", "name", profile.Name)
	return nil, nil
}
