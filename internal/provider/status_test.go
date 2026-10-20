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
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

type operatorState struct {
	state           string
	err             string
	completedBefore bool
	hosts, ready    int
}

func chiIn(s operatorState) *chiv1.ClickHouseInstallation {
	chi := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	st := chi.EnsureStatus()
	st.Status, st.Error = s.state, s.err
	st.HostsCount, st.HostsCompletedCount = s.hosts, s.ready
	if s.completedBefore {
		st.TaskIDsCompleted = []string{"task-1"}
	}
	return chi
}

func chkIn(s operatorState) *chkv1.ClickHouseKeeperInstallation {
	chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db-keeper", Namespace: "ns"}}
	st := chk.EnsureStatus()
	st.Status, st.Error = s.state, s.err
	st.HostsCount, st.HostsCompletedCount = s.hosts, s.ready
	if s.completedBefore {
		st.TaskIDsCompleted = []string{"task-1"}
	}
	return chk
}

var (
	completed = operatorState{state: chiv1.StatusCompleted, completedBefore: true, hosts: 2, ready: 2}
	creating  = operatorState{state: chiv1.StatusInProgress, hosts: 2, ready: 1}
	updating  = operatorState{state: chiv1.StatusInProgress, completedBefore: true, hosts: 2, ready: 1}
	aborted   = operatorState{state: chiv1.StatusAborted, err: "Update Service failed", completedBefore: true}
)

func TestStatus(t *testing.T) {
	standalone := newTestInstance("db", "ns", "")
	replicated := newReplicatedTestInstance("")
	withTLS := newTestInstance("db", "ns", `{"tls":{"enabled":"enabled"}}`)

	for _, tc := range []struct {
		name     string
		instance *corev1alpha1.Instance
		live     []client.Object
		phase    corev1alpha1.InstancePhase
		message  string
		// conn reports whether connection details are kept.
		conn bool
	}{
		{"nothing created yet", standalone, nil,
			corev1alpha1.InstancePhaseProvisioning, "Creating ClickHouse", false},
		{"first reconcile running", standalone, []client.Object{chiIn(creating)},
			corev1alpha1.InstancePhaseProvisioning, "Creating ClickHouse: 1 of 2 hosts ready", false},
		{"ready", standalone, []client.Object{chiIn(completed)},
			corev1alpha1.InstancePhaseReady, "", true},
		{"update of a ready cluster", standalone, []client.Object{chiIn(updating)},
			corev1alpha1.InstancePhaseUpdating, "Updating ClickHouse: 1 of 2 hosts ready", true},
		{"operator aborted", standalone, []client.Object{chiIn(aborted)},
			corev1alpha1.InstancePhaseFailed, "ClickHouse: Update Service failed", false},
		{"aborted without an error message", standalone, []client.Object{chiIn(operatorState{state: chiv1.StatusAborted})},
			corev1alpha1.InstancePhaseFailed, "ClickHouse: the operator aborted the last change", false},

		{"replicated: nothing created yet", replicated, nil,
			corev1alpha1.InstancePhaseProvisioning, "Creating ClickHouse Keeper", false},
		{"replicated: keeper being created", replicated, []client.Object{chkIn(operatorState{state: chiv1.StatusInProgress, hosts: 3, ready: 1})},
			corev1alpha1.InstancePhaseProvisioning, "Creating ClickHouse Keeper: 1 of 3 hosts ready", false},
		{"replicated: keeper ready, clickhouse being created", replicated, []client.Object{chkIn(completed), chiIn(creating)},
			corev1alpha1.InstancePhaseProvisioning, "Creating ClickHouse: 1 of 2 hosts ready", false},
		{"replicated: keeper rolling on a ready cluster", replicated, []client.Object{chkIn(operatorState{state: chiv1.StatusInProgress, completedBefore: true, hosts: 3, ready: 2}), chiIn(completed)},
			corev1alpha1.InstancePhaseUpdating, "Updating ClickHouse Keeper: 2 of 3 hosts ready", true},
		{"replicated: keeper added to a standalone cluster", replicated, []client.Object{chiIn(completed)},
			corev1alpha1.InstancePhaseUpdating, "Creating ClickHouse Keeper", true},
		{"replicated: keeper aborted", replicated, []client.Object{chkIn(aborted), chiIn(completed)},
			corev1alpha1.InstancePhaseFailed, "ClickHouse Keeper: Update Service failed", false},
		{"replicated: ready", replicated, []client.Object{chkIn(completed), chiIn(completed)},
			corev1alpha1.InstancePhaseReady, "", true},

		{"TLS certificate not issued yet", withTLS, []client.Object{chiIn(completed)},
			corev1alpha1.InstancePhaseUpdating, "Waiting for the TLS certificate", true},
		{"TLS certificate issued", withTLS, []client.Object{chiIn(completed), issuedCertificate()},
			corev1alpha1.InstancePhaseReady, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := New().Status(newTestContext(t, tc.instance.DeepCopy(), tc.live...))
			require.NoError(t, err)
			assert.Equal(t, tc.phase, st.Phase)
			assert.Equal(t, tc.message, st.Message)
			assert.Equal(t, tc.conn, !st.ConnectionDetails.IsEmpty(), "connection details kept")
		})
	}
}

func issuedCertificate() *cmapi.Certificate {
	cert := &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: serverCertName("db"), Namespace: "ns"}}
	cert.Status.Conditions = []cmapi.CertificateCondition{{Type: cmapi.CertificateConditionReady, Status: "True"}}
	return cert
}

// An aborted change used to fail Sync with a plain error, which the runtime
// does not turn into status: the Instance kept its previous phase.
func TestSyncPublishesOperatorFailure(t *testing.T) {
	_, c := syncStandalone(t, newTestInstance("db", "ns", ""), chiIn(aborted))

	live := &corev1alpha1.Instance{}
	require.NoError(t, c.Get(live, "db"))
	assert.Equal(t, corev1alpha1.InstancePhaseFailed, live.Status.Phase)
	assert.Equal(t, "ClickHouse: Update Service failed", live.Status.Message)
}

func TestSyncWaitsOnAbortedInstallation(t *testing.T) {
	var applied []*chiv1.ClickHouseInstallation
	c := newInterceptedTestContext(t, captureCHIApply(t, &applied), newTestInstance("db", "ns", ""), chiIn(aborted))

	err := New().Sync(c)
	require.Error(t, err)
	assert.True(t, controller.IsWaitError(err), "an aborted change is retried by the operator, so Sync keeps watching")
}
