/*
Copyright 2025 Intel Corporation. All Rights Reserved.

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

package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	apps "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
)

// ClusterPolicyReconciler reconciles a ClusterPolicy object
type ClusterPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

type ControllerOpts struct {
	ReqName                        string
	Namespace                      string
	SecretName                     string
	DPServiceAccountName           string
	DRAServiceAccountName          string
	ModuleLoaderServiceAccountName string
	RequeueDelay                   time.Duration
	DRAEnable                      bool
	KMMEnable                      bool
	OpenShift                      bool
}

type requeueReconcileErr struct {
	error
}

type SubControllerInterface interface {
	Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error)
}

const (
	ownerKey = "owner"

	xpumdVolumeName = "runxpumd"

	resourceModeDRA = "dra"
	resourceModeDP  = "dp"

	trueValue = "true"

	notAvailableStatus = "N/A"

	clusterPolicyFinalizer = "gpu.intel.com/clusterpolicy-protection"

	maxKeptErrors = 10
)

func gpuNodeSelector(cp *v1alpha.ClusterPolicy) map[string]string {
	selector := map[string]string{
		"kubernetes.io/arch": "amd64",
	}

	for k, v := range cp.Spec.NodeSelector {
		selector[k] = v
	}

	if cp.Spec.UseNFDLabeling {
		selector["intel.feature.node.kubernetes.io/gpu"] = trueValue
	}

	return selector
}

func addIfMissing(slice *[]string, s string) {
	if slices.Contains(*slice, s) {
		return
	}

	*slice = append(*slice, s)
}

// Namespace-scoped resources (apps, batch, core workloads, rbac roles/rolebindings, servicemonitors)
// are intentionally omitted here; they are granted via the namespaced Role in config/rbac/namespaced_role.yaml.

// Except for Pods as we need to list and possible delete them in other namespaces for FW update
// +kubebuilder:rbac:groups="",resources=pods,verbs=delete;get;list;watch

// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies/finalizers,verbs=update

// +kubebuilder:rbac:groups=nfd.k8s-sigs.io,resources=nodefeaturerules,verbs=create;get;update;delete;list;watch

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;create;delete;watch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;create;delete;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicybindings,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=deviceclasses,verbs=get;list;create;delete;watch;update

// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=watch;list

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update

// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=create;delete;get;list;watch;use;update

// Main Reconcile function for ClusterPolicy. Individual sub-controllers will be called from here to handle their
// respective resources, and any errors they return will be aggregated into the ClusterPolicy status.
func (r *ClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	cp := &v1alpha.ClusterPolicy{}

	klog.V(2).Info("Reconciling ClusterPolicy: " + req.Name)

	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		klog.V(2).Info("ClusterPolicy removal: " + req.Name)

		cp = nil
	}

	var origCp *v1alpha.ClusterPolicy
	if cp != nil {
		origCp = cp.DeepCopy()
	}

	// Defer status update at the end of reconciliation, to ensure we capture any changes made by sub-controllers.
	defer func() {
		// Update status if changed
		if origCp != nil && cp != nil && !reflect.DeepEqual(origCp.Status, cp.Status) {
			if err := r.Status().Update(ctx, cp); err != nil {
				klog.Error(err, "unable to update ClusterPolicy status")
			}
		}
	}()

	// Create a local copy of the options, in case we ever have parallel reconciles with
	// different request names.
	opts := r.Opts
	opts.ReqName = req.Name

	subControllers := make([]SubControllerInterface, 0, 3)

	subControllers = append(subControllers, &KMMReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})
	subControllers = append(subControllers, &XpuManagerReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})
	subControllers = append(subControllers, &MiscReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})

	// Ensure finalizer is present on live (non-deleted) ClusterPolicy objects.
	if cp != nil && cp.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cp, clusterPolicyFinalizer) {
			controllerutil.AddFinalizer(cp, clusterPolicyFinalizer)

			if err := r.Update(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Handle deletion: run sub-controllers so they can clean up their resources in the
	// correct order, then remove the finalizer once everything is gone.
	if cp != nil && !cp.DeletionTimestamp.IsZero() {
		for _, subController := range subControllers {
			if ret, err := subController.Reconcile(ctx, cp); err != nil {
				if errors.Is(err, requeueReconcileErr{}) {
					klog.Info("Requeueing deletion reconciliation after sub-controller request")

					return ret, nil
				}

				return ctrl.Result{}, err
			}
		}

		// All sub-controllers completed successfully — safe to remove the finalizer.
		// Suppress the deferred status update to avoid a resource-version conflict
		// after the Update call below.
		origCp = nil

		controllerutil.RemoveFinalizer(cp, clusterPolicyFinalizer)

		// The object may have been garbage-collected between the Get and this.
		// NotFound here means the goal is already achieved.
		if err := r.Update(ctx, cp); !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	var retErr error

	// Delegate reconciliation
	for _, subController := range subControllers {
		if ret, err := subController.Reconcile(ctx, cp); err != nil {
			if errors.Is(err, requeueReconcileErr{}) {
				klog.Info("Requeueing reconciliation after sub-controller request")
				return ret, nil
			}

			klog.Error("Return sub-controller error", err)

			retErr = err

			if cp != nil {
				for len(cp.Status.Errors) > maxKeptErrors {
					cp.Status.Errors = cp.Status.Errors[1:]
				}

				cp.Status.Errors = append(cp.Status.Errors, err.Error())
			}
		}
	}

	return ctrl.Result{}, retErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPolicyReconciler) SetupWithManager(mgr ctrl.Manager, opts ControllerOpts) error {
	r.Opts = opts

	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha.ClusterPolicy{}).
		Named("clusterpolicy").
		Owns(&apps.DaemonSet{})

	if opts.KMMEnable {
		b = b.Owns(&kmmv1beta1.Module{})
	}

	return b.Complete(r)
}
