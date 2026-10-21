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

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// keeperWiring is how ClickHouse and Keeper talk: which client ports Keeper
// serves and which one ClickHouse connects to.
//
// Switching Keeper to TLS (or back) takes steps, each rolled out before the
// next, so ClickHouse never loses Keeper: Keeper opens the new port next to
// the old one, ClickHouse moves to it, then Keeper closes the old one.
type keeperWiring struct {
	// secure: Keeper serves TLS on the secure client port, and between its
	// nodes (Raft).
	secure bool
	// plaintext: Keeper serves the plaintext client port.
	plaintext bool
	// port ClickHouse connects to Keeper on; 0 leaves it to the operator's
	// Keeper reference, which resolves to the plaintext port (clusters
	// that never had TLS).
	port int
}

// keeperState is what the operator has rolled out of the CHK and the CHI.
type keeperState struct {
	chkExists, chkRolledOut bool
	chkSecure, chkPlaintext bool
	chiExists, chiRolledOut bool
	// chiPort: the port the CHI wires ClickHouse to (0: Keeper reference,
	// -1: no Keeper).
	chiPort int
}

// planKeeperWiring returns the wiring to apply now and the one TLS calls for.
func planKeeperWiring(c *controller.Context) (step, target keeperWiring, err error) {
	state, err := liveKeeperState(c)
	if err != nil {
		return keeperWiring{}, keeperWiring{}, err
	}
	step, target = nextKeeperWiring(tlsEnabled(c), state)
	return step, target, nil
}

// nextKeeperWiring derives the next step towards the target from what is
// rolled out, so each reconcile moves the switch along by itself.
func nextKeeperWiring(tls bool, s keeperState) (step, target keeperWiring) {
	if tls {
		target = keeperWiring{secure: true, port: common.KeeperSecurePort}
	} else {
		target = keeperWiring{plaintext: true}
		// Once wired explicitly, stay so: the Keeper reference would prefer
		// the secure port while Keeper still serves it.
		if s.chiPort > 0 || s.chkSecure {
			target.port = common.KeeperPort
		}
	}

	// ClickHouse keeps its port until Keeper serves the target one.
	step.port = s.chiPort
	switch {
	case s.chiPort < 0 || !s.chiExists:
		// Nothing connects yet: ClickHouse is wired once Keeper is ready.
		step.port = target.port
	case !s.chkRolledOut:
	case tls && s.chkSecure:
		step.port = target.port
	case !tls && s.chkPlaintext:
		step.port = target.port
	}

	// ClickHouse still uses (or may still use) a port: Keeper keeps it.
	settled := s.chiRolledOut && step.port == s.chiPort
	usesSecure := s.chiPort == common.KeeperSecurePort || (!settled && s.chkSecure && s.chiExists)
	usesPlaintext := s.chiExists && s.chiPort >= 0 && (s.chiPort != common.KeeperSecurePort || !settled)
	step.secure = tls || usesSecure
	step.plaintext = !tls || usesPlaintext
	return step, target
}

// liveKeeperState reads what the operator has rolled out.
func liveKeeperState(c *controller.Context) (keeperState, error) {
	s := keeperState{chiPort: -1}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	found, err := getOptional(c, chk, keeperCRName(c.Name()))
	if err != nil {
		return s, err
	}
	if found {
		s.chkExists = true
		s.chkSecure, s.chkPlaintext = chkPorts(chk)
		s.chkRolledOut = chk.Status != nil && chk.Status.GetStatus() == chkv1.StatusCompleted &&
			rolledOut(chk.Spec.TaskID, chk.Status.GetTaskIDsCompleted())
	}
	chi := &chiv1.ClickHouseInstallation{}
	found, err = getOptional(c, chi, c.Name())
	if err != nil {
		return s, err
	}
	if found {
		s.chiExists = true
		s.chiPort = chiKeeperPort(chi)
		s.chiRolledOut = chi.Status != nil && chi.Status.GetStatus() == chiv1.StatusCompleted &&
			rolledOut(chi.Spec.TaskID, chi.Status.GetTaskIDsCompleted())
	}
	return s, nil
}

// chkPorts reports which client ports a CHK serves: the secure one when its
// cluster is secure, the plaintext one unless the cluster is insecure: no.
func chkPorts(chk *chkv1.ClickHouseKeeperInstallation) (secure, plaintext bool) {
	if chk.Spec.Configuration == nil || len(chk.Spec.Configuration.Clusters) == 0 {
		return false, true
	}
	cluster := chk.Spec.Configuration.Clusters[0]
	secure = cluster.Secure != nil && cluster.Secure.Value()
	plaintext = cluster.Insecure == nil || cluster.Insecure.Value()
	return secure, plaintext
}

// chiKeeperPort returns the port a CHI wires ClickHouse to Keeper on: 0 for
// the operator's Keeper reference, -1 without Keeper.
func chiKeeperPort(chi *chiv1.ClickHouseInstallation) int {
	if chi.Spec.Configuration == nil || chi.Spec.Configuration.Zookeeper == nil {
		return -1
	}
	zk := chi.Spec.Configuration.Zookeeper
	if len(zk.Nodes) > 0 && zk.Nodes[0].Port != nil {
		return int(zk.Nodes[0].Port.Value())
	}
	if zk.Keeper != nil {
		return 0
	}
	return -1
}

// keeperZookeeper wires ClickHouse to Keeper. With a port, it lists the
// Keeper hosts' ready-only client Services explicitly, as the operator's
// Keeper reference resolves them, so ClickHouse never connects to a Keeper
// node that is still joining the quorum (Altinity/clickhouse-operator#1982).
func keeperZookeeper(c *controller.Context, port int) *chiv1.ZookeeperConfig {
	zk := &chiv1.ZookeeperConfig{SessionTimeoutMs: 30000, OperationTimeoutMs: 10000}
	if port == 0 {
		zk.Keeper = &chiv1.KeeperRef{Name: keeperCRName(c.Name())}
		return zk
	}
	for _, svc := range keeperClientServiceNames(c.Name()) {
		host := fmt.Sprintf("%s.%s.svc.cluster.local", svc, c.Namespace())
		zk.Nodes = append(zk.Nodes, chiv1.NewZookeeperNode(host, int32(port), port == common.KeeperSecurePort))
	}
	return zk
}

// keeperHostNames returns the operator's per-node Keeper Service names,
// chk-{chk}-{cluster}-{shard}-{replica}, the nodes' Raft hostnames.
func keeperHostNames(instanceName string) []string {
	chk := trimCRName(keeperCRName(instanceName))
	names := make([]string, 0, common.KeeperReplicas)
	for i := 0; i < common.KeeperReplicas; i++ {
		names = append(names, fmt.Sprintf("chk-%s-%s-0-%d", chk, common.CHKClusterName, i))
	}
	return names
}

// keeperClientServiceNames returns the per-node ready-only client Services.
func keeperClientServiceNames(instanceName string) []string {
	hosts := keeperHostNames(instanceName)
	for i := range hosts {
		hosts[i] += "-client"
	}
	return hosts
}
