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

// +kubebuilder:webhook:path=/validate-power-openshift-io-v1-uncore,mutating=false,failurePolicy=fail,sideEffects=None,groups=power.openshift.io,resources=uncores,verbs=create;update,versions=v1,name=vuncore.kb.io,admissionReviewVersions=v1

var uncorelog = logf.Log.WithName("uncore-webhook")

// uncoreValidator implements admission.CustomValidator for Uncore.
type uncoreValidator struct{}

// SetupUncoreWebhookWithManager registers the validating webhook for Uncore.
func SetupUncoreWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Uncore{}).
		WithValidator(&uncoreValidator{}).
		Complete()
}

var _ webhook.CustomValidator = &uncoreValidator{}

// ValidateCreate implements admission.CustomValidator.
func (v *uncoreValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	uncore, ok := obj.(*Uncore)
	if !ok {
		return nil, fmt.Errorf("expected Uncore, got %T", obj)
	}
	uncorelog.Info("validating create", "name", uncore.Name)
	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator.
func (v *uncoreValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	uncore, ok := newObj.(*Uncore)
	if !ok {
		return nil, fmt.Errorf("expected Uncore, got %T", newObj)
	}
	uncorelog.Info("validating update", "name", uncore.Name)
	return nil, nil
}

// ValidateDelete implements admission.CustomValidator.
func (v *uncoreValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
