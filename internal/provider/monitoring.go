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

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// MonitoringSources implements controller.MonitoringSourcesProvider.
// ClickHouse serves OpenMetrics itself (metrics: Native in
// definition/monitoring.yaml), so every engine pod always exposes the
// endpoint and no exporter is switched on or off for a bound class. It is
// published once the operator has finished reconciling the cluster at least
// once, and stays published while later changes roll the pods, like the
// connection details. Monitoring extensions turn it into scrape objects.
func (p *Provider) MonitoringSources(c *controller.Context) (*corev1alpha1.MonitoringSources, error) {
	chi := &chiv1.ClickHouseInstallation{}
	found, err := getOptional(c, chi, c.Name())
	if err != nil {
		return nil, err
	}
	sources := &corev1alpha1.MonitoringSources{}
	if !found || !chiInstallation(chi).completedBefore {
		return sources, nil
	}
	sources.Metrics = []corev1alpha1.MetricsEndpoint{{
		Component: common.ComponentEngine,
		PodSelector: map[string]string{
			controller.InstanceLabel:  c.Name(),
			controller.ComponentLabel: common.ComponentEngine,
		},
		Port:   intstr.FromString(common.MetricsPortName),
		Path:   common.MetricsPath,
		Scheme: "http",
	}}
	return sources, nil
}

var _ controller.MonitoringSourcesProvider = (*Provider)(nil)

// podMonitorGVK is the Prometheus Operator PodMonitor, deleted only as
// metadata so the provider needs neither its types nor its CRD.
var podMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitor"}

// deleteLegacyPodMonitor removes the PodMonitor that releases before the
// monitoring contract created for the engine's podMonitor parameter; a
// monitoring extension now creates the scrape objects from the published
// sources. Clusters without the Prometheus Operator CRDs are skipped.
// TODO: drop once no supported release created PodMonitors.
func deleteLegacyPodMonitor(c *controller.Context) error {
	pm := &metav1.PartialObjectMetadata{ObjectMeta: c.ObjectMeta(legacyPodMonitorName(c.Name()))}
	pm.SetGroupVersionKind(podMonitorGVK)
	if err := c.Delete(pm); err != nil && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete legacy PodMonitor: %w", err)
	}
	return nil
}

// legacyPodMonitorName is the name of the PodMonitor earlier releases created.
func legacyPodMonitorName(instanceName string) string {
	return instanceName + "-metrics"
}
