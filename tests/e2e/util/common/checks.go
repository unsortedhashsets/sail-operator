//go:build e2e

// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"context"
	"fmt"
	"reflect"

	v1 "github.com/istio-ecosystem/sail-operator/api/v1"
	"github.com/istio-ecosystem/sail-operator/pkg/kube"
	. "github.com/istio-ecosystem/sail-operator/pkg/test/util/ginkgo"
	. "github.com/istio-ecosystem/sail-operator/tests/e2e/util/gomega"
	"github.com/istio-ecosystem/sail-operator/tests/e2e/util/kubectl"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AwaitCondition to be True. A key and a pointer to the object struct must be supplied. Extra arguments to pass to `Eventually` can be optionally supplied.
func AwaitCondition[T ~string](ctx context.Context, condition T, key client.ObjectKey, obj client.Object, k kubectl.Kubectl, cl client.Client, args ...any) {
	kind := reflect.TypeOf(obj).Elem().Name()
	cluster := "cluster"
	if k.ClusterName != "" {
		cluster = k.ClusterName
	}

	Eventually(GetObject, args...).
		WithArguments(ctx, cl, key, obj).
		Should(HaveConditionStatus(condition, metav1.ConditionTrue),
			fmt.Sprintf("%s %q is not %s on %s; unexpected Condition", kind, key.Name, condition, cluster))
	Success(fmt.Sprintf("%s %q is %s on %s", kind, key.Name, condition, cluster))

	if istio, ok := obj.(*v1.Istio); ok && string(condition) == string(v1.IstioConditionReady) {
		logIstiodIPFamilies(ctx, cl, istio)
	}
}

// logIstiodIPFamilies prints the IP families the istiod Service actually received. This is the
// check that proves a dual-stack run really is dual-stack: the first entry of clusterIPs is the
// primary family, so on an IPv6-primary cluster it must be an IPv6 address. Logged for every
// control plane on every run - on a single-stack run it is the baseline to compare against.
func logIstiodIPFamilies(ctx context.Context, cl client.Client, istio *v1.Istio) {
	services := &corev1.ServiceList{}
	// List by label rather than getting "istiod" by name, so revision-based and multi control
	// plane installs (istiod-<revision>) are covered too.
	if err := cl.List(ctx, services, client.InNamespace(istio.Spec.Namespace), client.MatchingLabels{"app": "istiod"}); err != nil {
		kubectl.LogDualStack("WARNING: Istio %q is Ready but its istiod Service could not be listed: %v", istio.Name, err)
		return
	}
	if len(services.Items) == 0 {
		kubectl.LogDualStack("WARNING: Istio %q is Ready but no istiod Service was found in namespace %q", istio.Name, istio.Spec.Namespace)
		return
	}
	for _, svc := range services.Items {
		policy := "<unset>"
		if svc.Spec.IPFamilyPolicy != nil {
			policy = string(*svc.Spec.IPFamilyPolicy)
		}
		kubectl.LogDualStack("Istio %q: Service %s/%s ipFamilyPolicy=%s ipFamilies=%v clusterIPs=%v (first = primary)",
			istio.Name, svc.Namespace, svc.Name, policy, svc.Spec.IPFamilies, svc.Spec.ClusterIPs)
	}
}

// AwaitDeployment to reach the Available state.
func AwaitDeployment(ctx context.Context, name string, k kubectl.Kubectl, cl client.Client) {
	AwaitCondition(ctx, appsv1.DeploymentAvailable, kube.Key(name, ControlPlaneNamespace), &appsv1.Deployment{}, k, cl)
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// CheckPodsReady in the given namespace.
func CheckPodsReady(ctx context.Context, cl client.Client, namespace string) error {
	podList := &corev1.PodList{}
	if err := cl.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("Failed to list pods: %w", err)
	}
	if len(podList.Items) == 0 {
		return fmt.Errorf("No pods found in namespace %q", namespace)
	}

	for _, pod := range podList.Items {
		if !isPodReady(&pod) {
			return fmt.Errorf("Pod %q in namespace %q is not ready", pod.Name, namespace)
		}
	}

	return nil
}

func CheckSamplePodsReady(ctx context.Context, cl client.Client) error {
	return CheckPodsReady(ctx, cl, sampleNamespace)
}

// AwaitCniDaemonSet to be deployed and reach the scheduled number of pods.
func AwaitCniDaemonSet(ctx context.Context, k kubectl.Kubectl, cl client.Client) {
	key := kube.Key("istio-cni-node", IstioCniNamespace)
	Eventually(func() bool {
		daemonset := &appsv1.DaemonSet{}
		if err := cl.Get(ctx, key, daemonset); err != nil {
			return false
		}
		return daemonset.Status.DesiredNumberScheduled > 0 &&
			daemonset.Status.NumberAvailable == daemonset.Status.DesiredNumberScheduled
	}).Should(BeTrue(), fmt.Sprintf("DaemonSet '%s' is not Available in the '%s' namespace on %s cluster", key.Name, key.Namespace, k.ClusterName))
	Success(fmt.Sprintf("DaemonSet '%s' is deployed and running in the '%s' namespace on %s cluster", key.Name, key.Namespace, k.ClusterName))
}
