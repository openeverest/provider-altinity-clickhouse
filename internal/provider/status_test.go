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
	"fmt"
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const (
	oldImage = "clickhouse/clickhouse-server:25.3"
	newImage = "clickhouse/clickhouse-server:25.8"
)

type operatorState struct {
	state           string
	err             string
	errs            []string
	completedBefore bool
	hosts, ready    int
	failed          int
	// task is the task ID of the spec; the operator finished it when it is
	// in completed or, without one, when the state is not InProgress.
	task, completed string
}

func chiIn(s operatorState) *chiv1.ClickHouseInstallation {
	chi := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	chi.Spec.TaskID = taskIDOf(s.task)
	chi.Spec.Templates = &chiv1.Templates{PodTemplates: []chiv1.PodTemplate{{
		Name: common.PodTemplateName,
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: common.ContainerClickHouse, Image: oldImage}}},
	}}}
	fillStatus(chi.EnsureStatus(), s)
	return chi
}

func chkIn(s operatorState) *chkv1.ClickHouseKeeperInstallation {
	chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db-keeper", Namespace: "ns"}}
	chk.Spec.TaskID = taskIDOf(s.task)
	st := chk.EnsureStatus()
	st.Status, st.Error, st.Errors = s.state, s.err, s.errs
	st.HostsCount, st.HostsCompletedCount, st.HostsFailedCount = s.hosts, s.ready, s.failed
	st.TaskIDsCompleted = completedTasks(s)
	return chk
}

func fillStatus(st *chiv1.Status, s operatorState) {
	st.Status, st.Error, st.Errors = s.state, s.err, s.errs
	st.HostsCount, st.HostsCompletedCount, st.HostsFailedCount = s.hosts, s.ready, s.failed
	st.TaskIDsCompleted = completedTasks(s)
}

func completedTasks(s operatorState) []string {
	var tasks []string
	if s.completed != "" {
		tasks = append(tasks, s.completed)
	}
	if s.completedBefore {
		tasks = append(tasks, "task-1")
	}
	return tasks
}

func taskIDOf(id string) *chtypes.Id {
	if id == "" {
		return nil
	}
	v := chtypes.Id(id)
	return &v
}

// wired wires the CHI to Keeper, as in the replicated topology.
func wired(chi *chiv1.ClickHouseInstallation) *chiv1.ClickHouseInstallation {
	chi.Spec.Configuration = &chiv1.Configuration{Zookeeper: &chiv1.ZookeeperConfig{Keeper: &chiv1.KeeperRef{Name: "db-keeper"}}}
	return chi
}

// withSpec changes the CHI's spec as a test needs it.
func withSpec(chi *chiv1.ClickHouseInstallation, mutate func(*chiv1.ChiSpec)) *chiv1.ClickHouseInstallation {
	mutate(&chi.Spec)
	return chi
}

var (
	completed = operatorState{state: chiv1.StatusCompleted, completedBefore: true, hosts: 2, ready: 2}
	creating  = operatorState{state: chiv1.StatusInProgress, hosts: 2, ready: 1}
	updating  = operatorState{state: chiv1.StatusInProgress, completedBefore: true, hosts: 2, ready: 1}
	aborted   = operatorState{state: chiv1.StatusAborted, err: "Update Service failed", completedBefore: true}
)

// host is the StatefulSet the operator runs for a ClickHouse host.
func host(shard, replica int) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name:      fmt.Sprintf("chi-db-clickhouse-%d-%d", shard, replica),
		Namespace: "ns",
		Labels:    map[string]string{common.LabelCHIName: "db", common.LabelCHIShard: fmt.Sprint(shard)},
	}}
}

type podState int

const (
	// podServing: Ready, and marked ready by the operator.
	podServing podState = iota
	// podUnmarked: Ready, but the operator took its ready mark off.
	podUnmarked
	// podNotReady: marked ready, but not Ready.
	podNotReady
)

// chPod is the pod of a ClickHouse host.
func chPod(shard, replica int, state podState, image string) *corev1.Pod {
	mark := common.LabelCHIReadyValue
	if state == podUnmarked {
		mark = "no"
	}
	ready := corev1.ConditionTrue
	if state == podNotReady {
		ready = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("chi-db-clickhouse-%d-%d-0", shard, replica),
			Namespace: "ns",
			Labels: map[string]string{
				common.LabelCHIName:  "db",
				common.LabelCHIShard: fmt.Sprint(shard),
				common.LabelCHIReady: mark,
			},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: common.ContainerClickHouse, Image: image}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}},
	}
}

// keeperPod is the pod of a Keeper node.
func keeperPod(replica int, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("chk-db-keeper-keeper-0-%d-0", replica),
			Namespace: "ns",
			Labels:    map[string]string{common.LabelCHKName: "db-keeper"},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// serving is a single ClickHouse host clients can use.
func servingHost() []client.Object {
	return []client.Object{host(0, 0), chPod(0, 0, podServing, oldImage)}
}

func objects(groups ...[]client.Object) []client.Object {
	var all []client.Object
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

func keeperQuorum() []client.Object {
	return []client.Object{keeperPod(0, true), keeperPod(1, true), keeperPod(2, false)}
}

func TestStatus(t *testing.T) {
	standalone := newTestInstance("db", "ns", "")
	replicated := newReplicatedTestInstance("")
	withTLS := newTLSTestInstance(tlsRequiredParams)

	type want struct {
		converged bool
		message   string
		available controller.Availability
		// reason is the reason of an answer other than Available.
		reason string
		// problem is the reason of the problem reported, if any, and
		// terminal whether it is one only a person can resolve.
		problem  string
		terminal bool
		// upgrading is the message of the Upgrading operation, if any.
		upgrading string
		// conn reports whether connection details are reported.
		conn bool
	}

	for _, tc := range []struct {
		name     string
		instance *corev1alpha1.Instance
		live     []client.Object
		want     want
	}{
		{"nothing created yet", standalone, nil,
			want{message: "Creating ClickHouse"}},
		{"first reconcile, no host yet", standalone, []client.Object{chiIn(creating)},
			want{message: "Creating ClickHouse: 1 of 2 hosts ready"}},
		{"first reconcile, host starting", standalone,
			[]client.Object{chiIn(creating), host(0, 0), chPod(0, 0, podNotReady, oldImage)},
			want{message: "Creating ClickHouse: 1 of 2 hosts ready", available: controller.Unavailable, reason: reasonNoReplicaReady}},
		{"ready", standalone, objects([]client.Object{chiIn(completed)}, servingHost()),
			want{converged: true, available: controller.Available, conn: true}},
		{"ready, the operator took the host out of the Service", standalone,
			[]client.Object{chiIn(completed), host(0, 0), chPod(0, 0, podUnmarked, oldImage)},
			want{converged: true, available: controller.Unavailable, reason: reasonNoReplicaReady, conn: true}},
		{"update of a serving cluster", standalone, objects([]client.Object{chiIn(updating)}, servingHost()),
			want{message: "Updating ClickHouse: 1 of 2 hosts ready", available: controller.Available, conn: true}},
		{"update of a cluster whose host restarts", standalone,
			[]client.Object{chiIn(updating), host(0, 0), chPod(0, 0, podNotReady, oldImage)},
			want{message: "Updating ClickHouse: 1 of 2 hosts ready", available: controller.Unavailable, reason: reasonNoReplicaReady}},
		{"change not picked up yet", standalone,
			objects([]client.Object{chiIn(operatorState{state: chiv1.StatusCompleted, completedBefore: true, task: "openeverest-new"})}, servingHost()),
			want{message: "Waiting for the operator to pick up the change to ClickHouse", available: controller.Available, conn: true}},
		{"latest task completed", standalone,
			objects([]client.Object{chiIn(operatorState{state: chiv1.StatusCompleted, task: "openeverest-new", completed: "openeverest-new"})}, servingHost()),
			want{converged: true, available: controller.Available, conn: true}},

		{"operator aborted", standalone, objects([]client.Object{chiIn(aborted)}, servingHost()),
			want{message: "The operator aborted the change to ClickHouse: Update Service failed",
				available: controller.Available, problem: corev1alpha1.ReasonEngineError, conn: true}},
		{"aborted without an error message", standalone, []client.Object{chiIn(operatorState{state: chiv1.StatusAborted})},
			want{message: "The operator aborted the change to ClickHouse: the operator aborted the last change",
				problem: corev1alpha1.ReasonEngineError}},
		{"aborted, newest error read without its time", standalone,
			[]client.Object{chiIn(operatorState{state: chiv1.StatusAborted, err: "an older error",
				errs: []string{"2026-11-19T10:00:00.123456789Z FAILED to reconcile CR ns/db", "2026-11-18T10:00:00Z an older error"}})},
			want{message: "The operator aborted the change to ClickHouse: FAILED to reconcile CR ns/db",
				problem: corev1alpha1.ReasonEngineError}},
		{"spec refused by the operator", standalone,
			[]client.Object{chiIn(operatorState{state: chiv1.StatusAborted,
				errs: []string{"[" + chiv1.StatusReasonRemovedSecretRefSyntax + "] user admin uses k8s_secret_password"}})},
			want{message: "The operator aborted the change to ClickHouse: user admin uses k8s_secret_password",
				problem: chiv1.StatusReasonRemovedSecretRefSyntax, terminal: true}},
		{"restart deferred until a peer is healthy", standalone,
			objects([]client.Object{chiIn(operatorState{state: chiv1.StatusAborted, completedBefore: true,
				errs: []string{"[" + chiv1.StatusReasonShardHasNoHealthyPeer + "] host 0-1 waits for its peer"}})}, servingHost()),
			want{message: "Waiting for a peer to become ready before the operator goes on with ClickHouse: host 0-1 waits for its peer",
				available: controller.Available, conn: true}},
		{"abort of an earlier task", standalone,
			objects([]client.Object{chiIn(operatorState{state: chiv1.StatusAborted, err: "boom", completedBefore: true, task: "openeverest-new"})}, servingHost()),
			want{message: "Waiting for the operator to pick up the change to ClickHouse", available: controller.Available, conn: true}},

		{"completed without a failed host", standalone,
			objects([]client.Object{chiIn(operatorState{state: chiv1.StatusCompleted, completedBefore: true, hosts: 2, ready: 1, failed: 1,
				err: "FAILED to reconcile StatefulSet for host: 0-1"})}, servingHost()),
			want{message: "The operator completed the change to ClickHouse without 1 of its 2 hosts",
				available: controller.Available, problem: reasonHostsFailed, conn: true}},
		{"replicated: keeper completed without a failed host", replicated,
			objects([]client.Object{chkIn(operatorState{state: chiv1.StatusCompleted, completedBefore: true, hosts: 3, ready: 2, failed: 1}),
				wired(chiIn(completed))}, keeperQuorum(), servingHost()),
			want{message: "The operator completed the change to ClickHouse Keeper without 1 of its 3 hosts",
				available: controller.Available, problem: reasonHostsFailed, conn: true}},

		{"stopped outside OpenEverest", standalone,
			[]client.Object{withSpec(chiIn(completed), func(s *chiv1.ChiSpec) { s.Stop = chtypes.NewStringBool(true) }), host(0, 0)},
			want{message: "ClickHouse is stopped on its ClickHouseInstallation object", available: controller.Unavailable,
				reason: reasonStopped, problem: reasonStoppedOutsideOpenEverest, terminal: true}},
		{"suspended outside OpenEverest", standalone,
			objects([]client.Object{withSpec(chiIn(completed), func(s *chiv1.ChiSpec) { s.Suspend = chtypes.NewStringBool(true) })}, servingHost()),
			want{converged: true, available: controller.Available, problem: reasonSuspendedOutsideOpenEverest, terminal: true, conn: true}},
		{"suspended outside OpenEverest with a change pending", standalone,
			objects([]client.Object{withSpec(chiIn(operatorState{state: chiv1.StatusCompleted, completedBefore: true, task: "openeverest-new"}),
				func(s *chiv1.ChiSpec) { s.Suspend = chtypes.NewStringBool(true) })}, servingHost()),
			want{message: "ClickHouse is suspended on its ClickHouseInstallation object", available: controller.Available,
				problem: reasonSuspendedOutsideOpenEverest, terminal: true, conn: true}},

		{"new image rolling out", newReplicatedTestInstance(""),
			objects(keeperQuorum(), []client.Object{
				chkIn(completed),
				withSpec(wired(chiIn(updating)), func(s *chiv1.ChiSpec) { s.Templates.PodTemplates[0].Spec.Containers[0].Image = newImage }),
				host(0, 0), host(0, 1),
				chPod(0, 0, podServing, newImage), chPod(0, 1, podServing, oldImage),
			}),
			want{message: "Updating ClickHouse: 1 of 2 hosts ready", available: controller.Available, conn: true,
				upgrading: "Rolling out ClickHouse image " + newImage + " (1 of 2 pods updated)"}},

		{"sharded: a shard without a ready replica", newReplicatedTestInstance(""),
			objects(keeperQuorum(), []client.Object{
				chkIn(completed), wired(chiIn(updating)),
				host(0, 0), host(1, 0), host(1, 1),
				chPod(0, 0, podServing, oldImage), chPod(1, 0, podNotReady, oldImage), chPod(1, 1, podUnmarked, oldImage),
			}),
			want{message: "Updating ClickHouse: 1 of 2 hosts ready", available: controller.Unavailable, reason: reasonNoReplicaReady}},
		{"sharded: a ready replica in every shard", newReplicatedTestInstance(""),
			objects(keeperQuorum(), []client.Object{
				chkIn(completed), wired(chiIn(updating)),
				host(0, 0), host(0, 1), host(1, 0), host(1, 1),
				chPod(0, 0, podServing, oldImage), chPod(0, 1, podNotReady, oldImage),
				chPod(1, 0, podNotReady, oldImage), chPod(1, 1, podServing, oldImage),
			}),
			want{message: "Updating ClickHouse: 1 of 2 hosts ready", available: controller.Available, conn: true}},

		{"replicated: nothing created yet", replicated, nil,
			want{message: "Creating ClickHouse Keeper"}},
		{"replicated: keeper being created", replicated,
			objects([]client.Object{chkIn(operatorState{state: chiv1.StatusInProgress, hosts: 3, ready: 1})}, keeperQuorum()),
			want{message: "Creating ClickHouse Keeper: 1 of 3 hosts ready"}},
		{"replicated: keeper ready, clickhouse being created", replicated,
			objects([]client.Object{chkIn(completed), wired(chiIn(creating))}, keeperQuorum()),
			want{message: "Creating ClickHouse: 1 of 2 hosts ready"}},
		{"replicated: keeper rolling on a serving cluster", replicated,
			objects([]client.Object{chkIn(operatorState{state: chiv1.StatusInProgress, completedBefore: true, hosts: 3, ready: 2}),
				wired(chiIn(completed))}, keeperQuorum(), servingHost()),
			want{message: "Updating ClickHouse Keeper: 2 of 3 hosts ready", available: controller.Available, conn: true}},
		{"replicated: keeper added to a standalone cluster", replicated,
			objects([]client.Object{chiIn(completed)}, servingHost()),
			want{message: "Creating ClickHouse Keeper", available: controller.Available, conn: true}},
		{"replicated: keeper aborted", replicated,
			objects([]client.Object{chkIn(aborted), wired(chiIn(completed))}, keeperQuorum(), servingHost()),
			want{message: "The operator aborted the change to ClickHouse Keeper: Update Service failed",
				available: controller.Available, problem: corev1alpha1.ReasonEngineError, conn: true}},
		{"replicated: ready", replicated,
			objects([]client.Object{chkIn(completed), wired(chiIn(completed))}, keeperQuorum(), servingHost()),
			want{converged: true, available: controller.Available, conn: true}},
		{"replicated: no Keeper node ready", replicated,
			objects([]client.Object{chkIn(completed), wired(chiIn(completed)), keeperPod(0, false), keeperPod(1, false)}, servingHost()),
			want{converged: true, available: controller.Unavailable, reason: reasonKeeperUnavailable, conn: true}},

		{"TLS certificate not issued yet", withTLS, objects([]client.Object{chiIn(completed)}, servingHost()),
			want{message: "Waiting for the TLS certificate", available: controller.Available, conn: true}},
		{"TLS certificate issued", withTLS, objects([]client.Object{chiIn(completed), issuedCertificate()}, servingHost()),
			want{converged: true, available: controller.Available, conn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := New().Status(newTestContext(t, tc.instance.DeepCopy(), tc.live...))
			require.NoError(t, err)

			assert.Equal(t, tc.want.converged, st.Converged, "converged")
			assert.Equal(t, tc.want.message, st.Message)
			assert.Equal(t, tc.want.available, st.Available, "available: %s", st.AvailableMessage)
			assert.Equal(t, tc.want.reason, st.AvailableReason)
			if tc.want.problem == "" {
				assert.Empty(t, st.Problems)
			} else if assert.Len(t, st.Problems, 1) {
				assert.Equal(t, tc.want.problem, st.Problems[0].Reason)
				assert.Equal(t, tc.want.terminal, st.Problems[0].Terminal)
				assert.NotEmpty(t, st.Problems[0].Message)
			}
			if tc.want.upgrading == "" {
				assert.Empty(t, st.Operations)
			} else if assert.Len(t, st.Operations, 1) {
				assert.Equal(t, corev1alpha1.ConditionUpgrading, st.Operations[0].Type)
				assert.Equal(t, reasonRollingOutImage, st.Operations[0].Reason)
				assert.Equal(t, tc.want.upgrading, st.Operations[0].Message)
			}
			assert.Equal(t, tc.want.conn, !st.ConnectionDetails.IsEmpty(), "connection details reported")
		})
	}
}

func TestStatusDesiredReplicas(t *testing.T) {
	addKeeper := func(in *corev1alpha1.Instance) *corev1alpha1.Instance {
		return withKeeper(in, func(*corev1alpha1.ComponentSpec) {})
	}
	for _, tc := range []struct {
		name     string
		instance *corev1alpha1.Instance
		want     []controller.ComponentReplicas
	}{
		{"standalone", newTestInstance("db", "ns", ""),
			[]controller.ComponentReplicas{{Component: common.ComponentEngine, Desired: 1}}},
		{"replicated, defaults", addKeeper(newReplicatedTestInstance("")),
			[]controller.ComponentReplicas{
				{Component: common.ComponentEngine, Desired: common.DefaultReplicasCount},
				{Component: common.ComponentKeeper, Desired: common.KeeperReplicas},
			}},
		{"replicated, three shards of two", addKeeper(shardedInstance(3, 2)),
			[]controller.ComponentReplicas{
				{Component: common.ComponentEngine, Desired: 6},
				{Component: common.ComponentKeeper, Desired: common.KeeperReplicas},
			}},
		{"replicated, two shards of one", addKeeper(shardedInstance(2, 1)),
			[]controller.ComponentReplicas{
				{Component: common.ComponentEngine, Desired: 2},
				{Component: common.ComponentKeeper, Desired: common.KeeperReplicas},
			}},
		// Keeper runs with its defaults, but only components of the
		// Instance carry a count.
		{"replicated without a keeper component", shardedInstance(2, 3),
			[]controller.ComponentReplicas{{Component: common.ComponentEngine, Desired: 6}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What the operator runs does not matter: here it still runs a
			// single host.
			st, err := New().Status(newTestContext(t, tc.instance, objects([]client.Object{chiIn(updating)}, servingHost())...))
			require.NoError(t, err)
			assert.Equal(t, tc.want, st.DesiredReplicas)
		})
	}
}

// Status reads the installations past the cache: right after Sync applied a
// change, the cache may still hold the object the operator finished.
func TestStatusReadsInstallationsPastTheCache(t *testing.T) {
	chi := chiIn(operatorState{state: chiv1.StatusCompleted, completedBefore: true, task: "openeverest-new"})
	var typedReads int
	c := newInterceptedTestContext(t, countTypedReads(&typedReads), newTestInstance("db", "ns", ""), chi)

	st, err := New().Status(c)
	require.NoError(t, err)
	assert.False(t, st.Converged)
	assert.Zero(t, typedReads, "the ClickHouseInstallation must be read as unstructured, which the client does not cache")
}

func TestAvailabilityMessages(t *testing.T) {
	sharded := observation{
		chi:        wired(chiIn(completed)),
		hosts:      []appsv1.StatefulSet{*host(0, 0), *host(1, 0), *host(2, 0), *host(10, 0)},
		pods:       []corev1.Pod{*chPod(0, 0, podServing, oldImage), *chPod(1, 0, podNotReady, oldImage), *chPod(10, 0, podNotReady, oldImage)},
		keeperPods: []corev1.Pod{*keeperPod(0, true)},
	}
	_, reason, message := availability(sharded)
	assert.Equal(t, reasonNoReplicaReady, reason)
	assert.Equal(t, "No ClickHouse replica is ready in shards 1, 2, 10", message)

	sharded.pods = append(sharded.pods, *chPod(2, 0, podServing, oldImage), *chPod(10, 0, podServing, oldImage))
	_, _, message = availability(sharded)
	assert.Equal(t, "No ClickHouse replica is ready in shard 1", message)
}

func TestPodServingChanged(t *testing.T) {
	p := podServingChanged()
	pod := chPod(0, 0, podServing, oldImage)

	unmarked := pod.DeepCopy()
	unmarked.Labels[common.LabelCHIReady] = "no"
	notReady := pod.DeepCopy()
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	deleting := pod.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	relabelled := pod.DeepCopy()
	relabelled.Labels["unrelated"] = "x"

	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: unmarked}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: notReady}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: deleting}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: relabelled}))
	assert.True(t, p.Create(event.CreateEvent{Object: pod}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: pod}))
}

func TestInstanceForPod(t *testing.T) {
	pod := chPod(0, 0, podServing, oldImage)
	assert.Empty(t, instanceForPod(t.Context(), pod), "a pod without the provider's labels")

	pod.Labels[controller.ProviderLabel] = common.ProviderName
	pod.Labels[controller.InstanceLabel] = "db"
	requests := instanceForPod(t.Context(), pod)
	require.Len(t, requests, 1)
	assert.Equal(t, "ns", requests[0].Namespace)
	assert.Equal(t, "db", requests[0].Name)

	pod.Labels[controller.ProviderLabel] = "another-provider"
	assert.Empty(t, instanceForPod(t.Context(), pod))
}

// issuedCertificate is the server certificate Secret as cert-manager writes it.
// Its Certificate's Ready condition does not matter: a re-issue turns it
// False while the issued certificate keeps serving.
func issuedCertificate() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: serverSecretName("db"), Namespace: "ns"},
		Data:       map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key"), "ca.crt": []byte("ca")},
	}
}

// Sync does not wait while the operator works or after it aborted: Status
// reports either, which the runtime asks for only after a Sync that did not
// wait.
func TestSyncReturnsWhileOperatorWorks(t *testing.T) {
	for name, state := range map[string]operatorState{"in progress": updating, "aborted": aborted} {
		t.Run(name, func(t *testing.T) {
			var applied []*chiv1.ClickHouseInstallation
			c := newInterceptedTestContext(t, captureCHIApply(t, &applied), newTestInstance("db", "ns", ""),
				withCredentials([]client.Object{chiIn(state)})...)
			require.NoError(t, New().Sync(c))
			assert.Len(t, applied, 1)
		})
	}
}

// ClickHouse needs a Keeper quorum to start: Sync holds the CHI back until
// Keeper has rolled out, without waiting.
func TestSyncHoldsClickHouseUntilKeeperRolledOut(t *testing.T) {
	var kinds []string
	c := newInterceptedTestContext(t, captureAppliedKinds(t, &kinds), newReplicatedTestInstance(""),
		withCredentials([]client.Object{chkIn(operatorState{state: chiv1.StatusInProgress, hosts: 3})})...)
	require.NoError(t, New().Sync(c))
	assert.Equal(t, []string{"ClickHouseKeeperInstallation"}, kinds)
}

// countTypedReads counts the reads of typed ClickHouseInstallations, which
// the client serves from its cache.
func countTypedReads(n *int) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*chiv1.ClickHouseInstallation); ok {
				*n++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}
