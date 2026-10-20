// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"fmt"
	"os"
	"strings"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// serviceAccountNamespaceFile holds the namespace of the pod the provider runs in.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// reconcilePlaintextPolicy creates the NetworkPolicy closing the plaintext
// ports when the applied CHI requires TLS, and removes it otherwise. It
// follows the applied CHI rather than the Instance, so a mode change held for
// maintenance takes effect together with the rest of it.
func reconcilePlaintextPolicy(c *controller.Context, chi *chiv1.ClickHouseInstallation) error {
	name := c.Name() + common.PlaintextPolicySuffix
	if !appliedTLSRequired(chi) {
		found, err := getOptional(c, &networkingv1.NetworkPolicy{}, name)
		if err != nil || !found {
			return err
		}
		return c.Delete(&networkingv1.NetworkPolicy{ObjectMeta: c.ObjectMeta(name)})
	}
	policy, err := plaintextPolicy(c)
	if err != nil {
		return err
	}
	if err := c.Apply(policy); err != nil {
		return fmt.Errorf("apply plaintext NetworkPolicy: %w", err)
	}
	return nil
}

// appliedTLSRequired reports whether chi was applied in the required TLS
// mode: its client Service carries the secure ports only. (Not an annotation:
// the operator copies CHI annotations onto the pods, restarting them.)
func appliedTLSRequired(chi *chiv1.ClickHouseInstallation) bool {
	if chi.Spec.Templates == nil {
		return false
	}
	for _, tmpl := range chi.Spec.Templates.ServiceTemplates {
		if tmpl.Name != common.ServiceTemplateName {
			continue
		}
		secure, plaintext := false, false
		for _, port := range tmpl.Spec.Ports {
			switch port.Port {
			case common.HTTPSPort, common.TCPSecurePort:
				secure = true
			case common.HTTPPort, common.TCPPort:
				plaintext = true
			}
		}
		return secure && !plaintext
	}
	return false
}

// plaintextPolicy lets anyone reach the secure ports and the metrics endpoint
// of the ClickHouse pods, and limits the plaintext ports (HTTP, native and
// interserver) to the operator, which only speaks plain HTTP, and the
// Instance's own pods, which replicate over the interserver port.
func plaintextPolicy(c *controller.Context) (*networkingv1.NetworkPolicy, error) {
	operator, err := operatorPeer()
	if err != nil {
		return nil, err
	}
	pods := metav1.LabelSelector{MatchLabels: map[string]string{common.LabelCHIName: c.Name()}}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: c.ObjectMeta(c.Name() + common.PlaintextPolicySuffix),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: pods,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: tcpPorts(common.HTTPSPort, common.TCPSecurePort, common.MetricsPort)},
				{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &pods}, operator},
					Ports: tcpPorts(common.HTTPPort, common.TCPPort, common.InterserverHTTPPort),
				},
			},
		},
	}, nil
}

// operatorPeer selects the Altinity operator pods: CLICKHOUSE_OPERATOR_NAMESPACE
// (the provider's own namespace by default) and CLICKHOUSE_OPERATOR_POD_LABELS.
func operatorPeer() (networkingv1.NetworkPolicyPeer, error) {
	namespace := os.Getenv(common.EnvOperatorNamespace)
	if namespace == "" {
		data, err := os.ReadFile(serviceAccountNamespaceFile)
		if err != nil {
			return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("operator namespace unknown: set %s", common.EnvOperatorNamespace)
		}
		namespace = strings.TrimSpace(string(data))
	}
	selector := os.Getenv(common.EnvOperatorPodLabels)
	if selector == "" {
		selector = common.DefaultOperatorPodLabels
	}
	podLabels, err := labels.ConvertSelectorToLabelsMap(selector)
	if err != nil {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("parse %s: %w", common.EnvOperatorPodLabels, err)
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: podLabels},
	}, nil
}

func tcpPorts(ports ...int) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	out := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		port := intstr.FromInt32(int32(p))
		out = append(out, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return out
}
