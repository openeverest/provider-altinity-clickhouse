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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// Status reports the Instance phase from the operator's view of the
// ClickHouseInstallation and, in the replicated topology, the
// ClickHouseKeeperInstallation:
//
//   - Provisioning while a cluster that never finished reconciling is created;
//   - Updating while a cluster that did is being changed (version, settings,
//     scaling, an approved maintenance action, a new Keeper), with connection
//     details kept, since it keeps serving clients meanwhile;
//   - Failed, with the operator's error, when the operator aborted a change;
//   - Ready once everything is reconciled.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	chi := &chiv1.ClickHouseInstallation{}
	found, err := getOptional(c, chi, c.Name())
	if err != nil {
		return controller.Status{}, err
	}
	clickhouse := installation{component: "ClickHouse"}
	if found {
		clickhouse = chiInstallation(chi)
	}

	// A cluster the operator has finished reconciling once is serving; every
	// later change is an update of it, Keeper and TLS changes included.
	var conn controller.ConnectionDetails
	if clickhouse.completedBefore {
		conn = buildConnectionDetails(c, chi)
	}
	inProgress := func(message string) controller.Status {
		if clickhouse.completedBefore {
			return controller.Status{Phase: corev1alpha1.InstancePhaseUpdating, Message: message, ConnectionDetails: conn}
		}
		return controller.Provisioning(message)
	}

	if c.Instance().GetTopologyType() == common.TopologyReplicated {
		chk := &chkv1.ClickHouseKeeperInstallation{}
		found, err := getOptional(c, chk, keeperCRName(c.Name()))
		if err != nil {
			return controller.Status{}, err
		}
		keeper := installation{component: "ClickHouse Keeper"}
		if found {
			keeper = chkInstallation(chk)
		}
		if st, done := keeper.status(inProgress); !done {
			return st, nil
		}
	}

	if st, done := clickhouse.status(inProgress); !done {
		return st, nil
	}

	if tlsEnabled(c) {
		issued, err := serverCertificateIssued(c)
		if err != nil {
			return controller.Status{}, err
		}
		if !issued {
			return inProgress("Waiting for the TLS certificate"), nil
		}
	}

	return controller.ReadyWithConnectionDetails(buildConnectionDetails(c, chi)), nil
}

// installation is the operator's view of a ClickHouseInstallation or
// ClickHouseKeeperInstallation, reduced to what the Instance status needs.
type installation struct {
	component string
	exists    bool
	// state is the operator's status.status: InProgress, Completed, Aborted, ...
	state string
	err   string
	// completedBefore reports whether the operator ever finished a reconcile.
	completedBefore bool
	hosts           int
	hostsCompleted  int
}

func chiInstallation(chi *chiv1.ClickHouseInstallation) installation {
	i := installation{component: "ClickHouse", exists: true}
	if s := chi.Status; s != nil {
		i.state = s.GetStatus()
		i.err = s.GetError()
		i.completedBefore = len(s.TaskIDsCompleted) > 0
		i.hosts = s.HostsCount
		i.hostsCompleted = s.HostsCompletedCount
	}
	return i
}

func chkInstallation(chk *chkv1.ClickHouseKeeperInstallation) installation {
	i := installation{component: "ClickHouse Keeper", exists: true}
	if s := chk.Status; s != nil {
		i.state = s.GetStatus()
		i.err = s.GetError()
		i.completedBefore = len(s.TaskIDsCompleted) > 0
		i.hosts = s.HostsCount
		i.hostsCompleted = s.HostsCompletedCount
	}
	return i
}

// status reports the installation's Instance status, or done when the
// operator has reconciled it.
func (i installation) status(inProgress func(string) controller.Status) (st controller.Status, done bool) {
	switch {
	case !i.exists:
		return inProgress("Creating " + i.component), false
	case i.state == chiv1.StatusCompleted:
		return controller.Status{}, true
	case i.state == chiv1.StatusAborted:
		reason := i.err
		if reason == "" {
			reason = "the operator aborted the last change"
		}
		return controller.Failed(fmt.Sprintf("%s: %s", i.component, reason)), false
	}

	verb := "Creating"
	if i.completedBefore {
		verb = "Updating"
	}
	message := fmt.Sprintf("%s %s", verb, i.component)
	if i.hosts > 0 {
		message = fmt.Sprintf("%s: %d of %d hosts ready", message, i.hostsCompleted, i.hosts)
	}
	return inProgress(message), false
}

// getOptional reads the named object, reporting whether it exists.
func getOptional(c *controller.Context, obj client.Object, name string) (bool, error) {
	if err := c.Get(obj, name); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	return true, nil
}
