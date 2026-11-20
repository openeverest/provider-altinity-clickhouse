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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// The engine's native endpoint is published once the cluster has served,
// and kept while later changes roll the pods.
func TestMonitoringSources(t *testing.T) {
	engine := corev1alpha1.MetricsEndpoint{
		Component: common.ComponentEngine,
		PodSelector: map[string]string{
			controller.InstanceLabel:  "db",
			controller.ComponentLabel: common.ComponentEngine,
		},
		Port:   intstr.FromString(common.MetricsPortName),
		Path:   common.MetricsPath,
		Scheme: "http",
	}

	for _, tc := range []struct {
		name string
		objs []client.Object
		want []corev1alpha1.MetricsEndpoint
	}{
		{name: "no cluster yet"},
		{name: "cluster being created", objs: []client.Object{chiIn(creating)}},
		{name: "cluster ready", objs: []client.Object{chiIn(completed)}, want: []corev1alpha1.MetricsEndpoint{engine}},
		{name: "cluster updating", objs: []client.Object{chiIn(updating)}, want: []corev1alpha1.MetricsEndpoint{engine}},
		{name: "operator aborted a change", objs: []client.Object{chiIn(aborted)}, want: []corev1alpha1.MetricsEndpoint{engine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, newTestInstance("db", "ns", ""), tc.objs...)

			got, err := New().MonitoringSources(c)

			require.NoError(t, err)
			require.NotNil(t, got, "nil would clear the published sources")
			assert.Equal(t, tc.want, got.Metrics)
			assert.Empty(t, got.Connections)
			assert.Empty(t, got.Credentials)
		})
	}
}

// An error reading the cluster keeps the published sources.
func TestMonitoringSourcesReadError(t *testing.T) {
	boom := errors.New("boom")
	c := newInterceptedTestContext(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}, newTestInstance("db", "ns", ""))

	_, err := New().MonitoringSources(c)

	require.ErrorIs(t, err, boom)
}

// The runtime publishes an endpoint only for a component type that declares
// a kind and metrics, and only with the Instance label in its pod selector.
func TestProviderDeclaresEngineMetrics(t *testing.T) {
	spec := testProvider(t).Spec
	engineType := spec.Components[common.ComponentEngine].Type

	assert.Equal(t, "clickhouse", spec.ComponentTypes[engineType].Kind)
	require.NotNil(t, spec.Monitoring)
	assert.Equal(t, corev1alpha1.MetricsSourceNative, spec.Monitoring.ComponentTypes[engineType].Metrics)
	assert.Empty(t, spec.Monitoring.Integrations, "the provider renders no operator-native integration")
}

// The PodMonitor earlier releases created is deleted by name, and a cluster
// without the Prometheus Operator CRDs is not an error.
func TestDeleteLegacyPodMonitor(t *testing.T) {
	noMatch := &meta.NoKindMatchError{GroupKind: podMonitorGVK.GroupKind()}
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "deleted"},
		{name: "no PodMonitor CRD", err: noMatch},
		{name: "API error", err: errors.New("boom"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleted []client.Object
			c := newInterceptedTestContext(t, interceptor.Funcs{
				Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
					deleted = append(deleted, obj)
					return tc.err
				},
			}, newTestInstance("db", "ns", `{"podMonitor":"enabled"}`))

			err := deleteLegacyPodMonitor(c)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, deleted, 1)
			assert.Equal(t, podMonitorGVK, deleted[0].GetObjectKind().GroupVersionKind())
			assert.Equal(t, "db-metrics", deleted[0].GetName())
			assert.Equal(t, "ns", deleted[0].GetNamespace())
		})
	}
}
