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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
)

var _ = Describe("ClusterPolicy Controller", func() {

	Context("When reconciling dra and xpum", func() {
		defaultNamespace := "foobar-dra-xpum-xpum"
		const resourceName = "test-resource-dra-xpum-xpum"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				ResourceMonitoring:   true,
				UseNFDLabeling:       true,
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "dra-image:v1.2.3",
				},
				XpuManagerSpec: v1alpha.XpuManagerSpec{
					Image:    "xpum-image:v1.2.3",
					LogLevel: 3,
				},
			},
		}

		BeforeEach(func() {
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultNamespace,
					Labels: map[string]string{
						"resource.kubernetes.io/admin-access": "true",
					},
				},
			}

			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("creating the custom resource for the Kind ClusterPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, clusterpolicy)).To(Succeed())
			}

			By("Reconciling the created resource")
			controllerReconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:             defaultNamespace,
					DRAEnable:             true,
					KMMEnable:             true,
					DRAServiceAccountName: "intel-gpu-dra",
					RequeueDelay:          time.Millisecond * 50,
				},
			}

			ret, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			// Re-reconcile to make sure that no-op reconciliation doesn't fail.
			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			By("verifying KMM Module CR was created")
			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resourceName + "-gpu", Namespace: defaultNamespace}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())
			Expect(mod.Spec.DRA).NotTo(BeNil())
			Expect(mod.Spec.DRA.Container.Image).To(Equal("dra-image:v1.2.3"))

			By("verifying XPUM DaemonSet was created")
			daemonSets := apps.DaemonSetList{}
			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(daemonSets.Items).To(HaveLen(1))
			Expect(daemonSets.Items[0].Name).To(Equal(resourceName + "-xpu-manager"))
			Expect(daemonSets.Items[0].Spec.Template.Spec.Containers[0].Image).To(Equal("xpum-image:v1.2.3"))

			nfr := nfdcrd.NodeFeatureRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "intel-gpu-devices"}, &nfr)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Update(ctx, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(daemonSets.Items).To(HaveLen(1))

			configmaps := v1.ConfigMapList{}
			err = k8sClient.List(ctx, &configmaps, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(configmaps.Items).To(HaveLen(1))
			Expect(configmaps.Items[0].Name).To(Equal(resourceName + "-xpumanager-otel-config"))

			clusterpolicy.Spec.HealthinessSpec = &v1alpha.HealthinessSpec{
				CheckIntervalSeconds: 17,
			}
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			// Create XPUM config override
			xpumConfig := deployments.XpuManagerOTelConfig()
			data, err := yaml.Marshal(xpumConfig)
			Expect(err).NotTo(HaveOccurred())

			xpumCm := v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-xpum-cm",
					Namespace: defaultNamespace,
				},
				Data: map[string]string{
					"config.yaml": string(data),
				},
			}
			Expect(k8sClient.Create(ctx, &xpumCm)).To(Succeed())

			clusterpolicy.Spec.XpuManagerSpec.ConfigMapOverride = "test-xpum-cm"
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(daemonSets.Items).To(HaveLen(1))

			By("verifying XPUM config override was applied")
			checked := false
			for _, v := range daemonSets.Items[0].Spec.Template.Spec.Volumes {
				if v.Name == "config" {
					Expect(v.VolumeSource.ConfigMap).NotTo(BeNil())
					Expect(v.VolumeSource.ConfigMap.Name).To(Equal("test-xpum-cm"))
					checked = true
				}
			}
			Expect(checked).To(BeTrue())

			err = k8sClient.Delete(ctx, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			// With the finalizer in place the ClusterPolicy is not immediately removed —
			// it receives a DeletionTimestamp and waits for the controller to finish cleanup.
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpolicy.DeletionTimestamp).NotTo(BeNil())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// After the reconcile the finalizer has been removed and the CR is gone.
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(daemonSets.Items).To(BeEmpty())
		})
	})
})
