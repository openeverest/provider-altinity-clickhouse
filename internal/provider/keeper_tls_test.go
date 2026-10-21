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
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// served reports whether a Keeper with the given ports serves the port
// ClickHouse is wired to (0: the operator's Keeper reference, plaintext).
func served(port int, secure, plaintext bool) bool {
	if port == common.KeeperSecurePort {
		return secure
	}
	return plaintext
}

// migrate rolls nextKeeperWiring out step by step, as the operator would,
// and fails if ClickHouse could use a port Keeper does not serve at any
// point: while Keeper rolls (old or new configuration), or while ClickHouse
// rolls (old or new port).
func migrate(t *testing.T, tls bool, s keeperState) keeperState {
	t.Helper()
	for i := 0; i < 10; i++ {
		step, target := nextKeeperWiring(tls, s)
		if step == target && s.chkExists && s.chkSecure == step.secure && s.chkPlaintext == step.plaintext &&
			(!s.chiExists || s.chiPort == step.port) {
			return s
		}
		// Keeper rolls to the step's ports.
		if s.chiExists && s.chiPort >= 0 {
			require.True(t, served(s.chiPort, s.chkSecure, s.chkPlaintext) && served(s.chiPort, step.secure, step.plaintext),
				"step %d: Keeper %+v drops port %d ClickHouse uses", i, step, s.chiPort)
		}
		s.chkExists, s.chkRolledOut, s.chkSecure, s.chkPlaintext = true, true, step.secure, step.plaintext

		// ClickHouse is (re)wired once Keeper rolled out; replan as Sync does
		// on its next reconcile.
		step, _ = nextKeeperWiring(tls, s)
		require.Equal(t, s.chkSecure, step.secure, "step %d: Keeper must settle before ClickHouse moves", i)
		require.Equal(t, s.chkPlaintext, step.plaintext, "step %d", i)
		if !s.chiExists {
			s.chiExists = true
		}
		if s.chiPort >= 0 && s.chiPort != step.port {
			require.True(t, served(s.chiPort, s.chkSecure, s.chkPlaintext), "step %d: old port %d", i, s.chiPort)
		}
		require.True(t, served(step.port, s.chkSecure, s.chkPlaintext), "step %d: new port %d not served", i, step.port)
		s.chiPort, s.chiRolledOut = step.port, true
	}
	t.Fatalf("no convergence from %+v", s)
	return s
}

func TestKeeperWiringMigrations(t *testing.T) {
	plain := keeperState{chkExists: true, chkRolledOut: true, chkPlaintext: true, chiExists: true, chiRolledOut: true, chiPort: 0}
	tlsOnly := keeperState{chkExists: true, chkRolledOut: true, chkSecure: true, chiExists: true, chiRolledOut: true, chiPort: common.KeeperSecurePort}

	t.Run("new instance with TLS: secure from the start", func(t *testing.T) {
		step, target := nextKeeperWiring(true, keeperState{chiPort: -1})
		assert.Equal(t, keeperWiring{secure: true, port: common.KeeperSecurePort}, step)
		assert.Equal(t, target, step)
	})
	t.Run("new instance without TLS: the Keeper reference as before", func(t *testing.T) {
		step, target := nextKeeperWiring(false, keeperState{chiPort: -1})
		assert.Equal(t, keeperWiring{plaintext: true}, step)
		assert.Equal(t, target, step)
	})
	t.Run("enabling TLS", func(t *testing.T) {
		end := migrate(t, true, plain)
		assert.Equal(t, tlsOnly, end)
	})
	t.Run("disabling TLS", func(t *testing.T) {
		end := migrate(t, false, tlsOnly)
		assert.True(t, end.chkPlaintext)
		assert.False(t, end.chkSecure)
		assert.Equal(t, common.KeeperPort, end.chiPort, "stays wired explicitly")
	})
	t.Run("standalone to replicated with TLS", func(t *testing.T) {
		end := migrate(t, true, keeperState{chiExists: true, chiRolledOut: true, chiPort: -1})
		assert.Equal(t, tlsOnly, end)
	})
	t.Run("disabling halfway through enabling", func(t *testing.T) {
		halfway := keeperState{chkExists: true, chkRolledOut: true, chkSecure: true, chkPlaintext: true,
			chiExists: true, chiRolledOut: false, chiPort: common.KeeperSecurePort}
		end := migrate(t, false, halfway)
		assert.False(t, end.chkSecure)
		assert.Equal(t, common.KeeperPort, end.chiPort)
	})
	t.Run("steps hold while the operator rolls", func(t *testing.T) {
		// Keeper still rolling out its second port: ClickHouse stays put.
		rolling := plain
		rolling.chkRolledOut, rolling.chkSecure = false, true
		step, _ := nextKeeperWiring(true, rolling)
		assert.Equal(t, keeperWiring{secure: true, plaintext: true, port: 0}, step)
		// ClickHouse rolling to the secure port: Keeper keeps the plaintext one.
		moving := keeperState{chkExists: true, chkRolledOut: true, chkSecure: true, chkPlaintext: true,
			chiExists: true, chiRolledOut: false, chiPort: common.KeeperSecurePort}
		step, _ = nextKeeperWiring(true, moving)
		assert.True(t, step.plaintext)
	})
}

func TestKeeperWiringRendering(t *testing.T) {
	c := newTestContext(t, newReplicatedTestInstance(tlsRequiredParams))

	chk, err := buildCHKWired(c, keeperWiring{secure: true})
	require.NoError(t, err)
	cluster := chk.Spec.Configuration.Clusters[0]
	assert.True(t, cluster.Secure.Value())
	assert.False(t, cluster.Insecure.Value(), "the plaintext port closed")
	require.NotNil(t, chk.Spec.Configuration.Files)
	assert.Equal(t, "db-server-tls", chk.Spec.Configuration.Files.Get("server.crt").GetSecretKeyRef().Name)

	chk, err = buildCHKWired(c, keeperWiring{plaintext: true})
	require.NoError(t, err)
	assert.Nil(t, chk.Spec.Configuration.Clusters[0].Secure)
	assert.Nil(t, chk.Spec.Configuration.Clusters[0].Insecure, "unset as before TLS existed")
	assert.Nil(t, chk.Spec.Configuration.Files)

	chi, err := buildCHIWired(c, 2, keeperWiring{secure: true, port: common.KeeperSecurePort})
	require.NoError(t, err)
	zk := chi.Spec.Configuration.Zookeeper
	assert.Nil(t, zk.Keeper)
	require.Len(t, zk.Nodes, 3)
	for i, node := range zk.Nodes {
		assert.Equal(t, fmt.Sprintf("chk-db-keeper-keeper-0-%d-client.ns.svc.cluster.local", i), node.Host)
		assert.Equal(t, int32(common.KeeperSecurePort), node.Port.Value())
		assert.True(t, node.Secure.Value())
	}
	settings := chi.Spec.Configuration.Settings
	assert.Equal(t, "9010", settings.Get("interserver_https_port").ScalarString())
	assert.Equal(t, "_removed_", settings.Get("interserver_http_port").ScalarString())

	chi, err = buildCHIWired(c, 2, keeperWiring{plaintext: true})
	require.NoError(t, err)
	require.NotNil(t, chi.Spec.Configuration.Zookeeper.Keeper, "the operator's Keeper reference")
	assert.Equal(t, "db-keeper", chi.Spec.Configuration.Zookeeper.Keeper.Name)
}

// Turning TLS off moves ClickHouse back to Keeper's plaintext port in a
// later step; until then it still verifies Keeper with the CA.
func TestCHIKeepsKeeperCAWhileOnSecurePort(t *testing.T) {
	c := newTestContext(t, newReplicatedTestInstance(""))
	chi, err := buildCHIWired(c, 2, keeperWiring{secure: true, plaintext: true, port: common.KeeperSecurePort})
	require.NoError(t, err)
	require.NotNil(t, chi.Spec.Configuration.Files)
	assert.True(t, chi.Spec.Configuration.Files.Has("openssl.xml"))
	assert.Nil(t, chi.Spec.Configuration.Settings.Get("https_port"), "the client ports are off")

	chi, err = buildCHIWired(c, 2, keeperWiring{plaintext: true, port: common.KeeperPort})
	require.NoError(t, err)
	assert.Nil(t, chi.Spec.Configuration.Files)
}

func TestLiveKeeperState(t *testing.T) {
	chk, err := buildCHKWired(newTestContext(t, newReplicatedTestInstance("")), keeperWiring{secure: true, plaintext: true})
	require.NoError(t, err)
	chk.EnsureStatus().Status = chkv1.StatusCompleted
	chi := chiIn(completed)
	chi.Spec.Configuration = &chiv1.Configuration{Zookeeper: keeperZookeeper(newTestContext(t, newReplicatedTestInstance("")), common.KeeperSecurePort)}

	s, err := liveKeeperState(newTestContext(t, newReplicatedTestInstance(""), chk, chi))
	require.NoError(t, err)
	assert.Equal(t, keeperState{chkExists: true, chkRolledOut: true, chkSecure: true, chkPlaintext: true,
		chiExists: true, chiRolledOut: true, chiPort: common.KeeperSecurePort}, s)
}

// A change applied in steps shares one approval: once a change toward a
// target was applied (approved, or a user's edit), the record carries the
// target's token, and its later steps are not held again.
func TestRolloutGateCarriesApprovalAcrossSteps(t *testing.T) {
	instance := newReplicatedTestInstance("")
	_, first := syncReplicated(t, instance, completedCHK(), completedCHI())
	state := persisted(t, first, rolloutStateName("db"))

	gateWith := func(t *testing.T, target string) (*rolloutGate, *controller.Context, *chkv1.ClickHouseKeeperInstallation) {
		t.Helper()
		changed := state.DeepCopy()
		changed.Data["chk.spec"] = []byte("rendered-by-the-previous-step")
		var kinds []string
		c := newInterceptedTestContext(t, captureAppliedKinds(t, &kinds), instance,
			withCredentials([]client.Object{changed, completedCHK(), completedCHI()})...)
		_, err := ensureCredentials(c) // as Sync does first
		require.NoError(t, err)
		chk, err := buildCHK(c)
		require.NoError(t, err)
		gate, err := newRolloutGate(c, chk)
		require.NoError(t, err)
		if target == "" {
			target = gate.token
		}
		gate.record[targetKey] = target
		return gate, c, chk
	}

	gate, c, chk := gateWith(t, "")
	applied, err := gate.apply(c, chk, "chk")
	require.NoError(t, err)
	assert.True(t, applied, "a later step of the approved change")
	assert.Empty(t, c.GetPendingMaintenance())

	gate, c, chk = gateWith(t, "provider-0.0.9-another-change")
	applied, err = gate.apply(c, chk, "chk")
	require.NoError(t, err)
	assert.False(t, applied, "another change is held")
	assert.Len(t, c.GetPendingMaintenance(), 1)
}

// Applying an unchanged object records no approval: otherwise a held
// change whose other object happens to be unchanged would go through on the
// next reconcile.
func TestRolloutGateRecordsApprovalOnlyWithAChange(t *testing.T) {
	instance := newReplicatedTestInstance("")
	_, first := syncReplicated(t, instance, completedCHK(), completedCHI())
	state := persisted(t, first, rolloutStateName("db"))
	state.Data[targetKey] = []byte("provider-0.0.9-earlier")

	_, c := syncReplicated(t, instance, state, completedCHK(), completedCHI())
	recorded := persisted(t, c, rolloutStateName("db"))
	assert.Equal(t, "provider-0.0.9-earlier", string(recorded.Data[targetKey]))
}

func TestKeeperPolicy(t *testing.T) {
	t.Setenv(common.EnvOperatorNamespace, "clickhouse-provider")
	c := newTestContext(t, newReplicatedTestInstance(tlsRequiredParams))

	require.NoError(t, reconcilePlaintextPolicy(c, withTLSMode(completedCHI(), common.TLSModeRequired)))
	policy := &networkingv1.NetworkPolicy{}
	require.NoError(t, c.Get(policy, "db"+common.KeeperPolicySuffix))
	assert.Equal(t, map[string]string{common.LabelCHKName: "db-keeper"}, policy.Spec.PodSelector.MatchLabels)
	require.Len(t, policy.Spec.Ingress, 3)
	assert.Equal(t, map[string]string{common.LabelCHIName: "db"}, policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels)
	assert.Equal(t, []int32{2181, 2281}, policyPorts(policy.Spec.Ingress[0].Ports))
	assert.Equal(t, []int32{2181, 2281, 9444}, policyPorts(policy.Spec.Ingress[1].Ports))
	assert.Empty(t, policy.Spec.Ingress[2].Ports, "the operator reaches every port")

	require.NoError(t, reconcilePlaintextPolicy(c, withTLSMode(completedCHI(), common.TLSModeOptional)))
	found, err := getOptional(c, &networkingv1.NetworkPolicy{}, "db"+common.KeeperPolicySuffix)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestServerCertificateCoversKeeper(t *testing.T) {
	c := newTestContext(t, newReplicatedTestInstance(tlsRequiredParams))
	require.NoError(t, ensureServerCertificate(c, ensureIssuerRef(t, c)))
	names := serverCertificate(t, c).Spec.DNSNames
	for _, name := range []string{
		"keeper-db-keeper.ns.svc.cluster.local",
		"chk-db-keeper-keeper-0-2.ns.svc.cluster.local",
		"chk-db-keeper-keeper-0-0-client.ns.svc.cluster.local",
	} {
		assert.Contains(t, names, name)
	}
}

// Sync applies one step at a time and requeues itself until the switch is
// done, rather than wait for an unrelated event.
func TestSyncRequeuesUntilKeeperSwitchDone(t *testing.T) {
	instance := newReplicatedTestInstance(tlsRequiredParams)
	// A running cluster wired through the operator's Keeper reference.
	running := completedCHI()
	running.Spec.Configuration = &chiv1.Configuration{Zookeeper: keeperZookeeper(newTestContext(t, instance), 0)}
	var kinds []string
	c := newInterceptedTestContext(t, captureAppliedKinds(t, &kinds), instance,
		withCredentials([]client.Object{completedCHK(), running, issuedCertificate()})...)
	err := New().Sync(c)
	require.True(t, controller.IsWaitError(err), "got %v", err)
	assert.Contains(t, err.Error(), "switching ClickHouse and Keeper")
}
