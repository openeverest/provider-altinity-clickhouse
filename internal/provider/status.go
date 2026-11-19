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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// Reasons of the problems the provider reports. A problem's reason becomes
// the status word users read, so each names one cause.
const (
	// reasonStoppedOutsideOpenEverest: spec.stop is set on the
	// ClickHouseInstallation or ClickHouseKeeperInstallation object. The
	// provider never sets it, so it was set there by hand, and the operator
	// keeps the pods stopped until it is unset.
	reasonStoppedOutsideOpenEverest = "StoppedOutsideOpenEverest"
	// reasonSuspendedOutsideOpenEverest: spec.suspend is set on one of them.
	// The provider never sets it either. The operator then leaves the object
	// alone, so no change of the Instance reaches the cluster.
	reasonSuspendedOutsideOpenEverest = "SuspendedOutsideOpenEverest"
	// reasonHostsFailed: the operator completed a change without some of
	// the hosts, whose StatefulSet it could not reconcile and skipped. It
	// does not try them again until it reconciles the installation again.
	reasonHostsFailed = "HostsFailed"
)

// Reasons explaining why clients cannot connect.
const (
	reasonStopped           = "Stopped"
	reasonNoReplicaReady    = "NoReplicaReady"
	reasonKeeperUnavailable = "KeeperUnavailable"
)

// reasonRollingOutImage is the reason of the Upgrading operation.
const reasonRollingOutImage = "RollingOutImage"

// The operator tags some of the reconciles it aborts with a reason, written
// as "[Reason] message" at the head of status.errors.
var (
	// specRejectedReasons are aborts of a spec the operator refuses: it does
	// not try again until the spec changes.
	specRejectedReasons = []string{
		chiv1.StatusReasonFIPSValidationFailed,
		chiv1.StatusReasonFIPSImagePolicyViolation,
		chiv1.StatusReasonRootCAConflict,
		chiv1.StatusReasonRootCASecretUnresolved,
		chiv1.StatusReasonRemovedSecretRefSyntax,
		chiv1.StatusReasonNoKeeperListener,
	}
	// deferredReasons are aborts that hold back the restart of a host while
	// the other replicas of its shard, or the Keeper quorum, could not take
	// it. The operator tries again once the peer it waits for is Ready.
	deferredReasons = []string{
		chiv1.StatusReasonShardHasNoHealthyPeer,
		chiv1.StatusReasonRaftQuorumUnsafe,
	}
)

// Status reports what the provider observes about the cluster: whether the
// operator runs what the Instance asks for, whether clients can connect, what
// is wrong, which image is rolling out, and how many pods each component will
// have. The runtime derives the Instance's phase and conditions from these
// facts.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	o, err := observeCluster(c)
	if err != nil {
		return controller.Status{}, err
	}
	status := withDesiredReplicas(c, observe(o))

	// The connection details are reported as soon as clients can connect,
	// not only once the cluster has converged: whoever is told the database
	// is available must find where to connect, and the details must follow a
	// change that is still rolling out.
	if o.chi != nil && (status.Converged || status.Available == controller.Available) {
		status.ConnectionDetails = buildConnectionDetails(c, o.chi)
	}
	return status, nil
}

// withDesiredReplicas reports how many pods each component will have once
// the cluster runs what the Instance asks for: for the engine, the shards
// times the replicas of each, with the defaults Sync applies; for Keeper, the
// fixed ensemble of the replicated topology. The counts come from the
// Instance, not from the installations, which may still describe an earlier
// spec.
func withDesiredReplicas(c *controller.Context, status controller.Status) controller.Status {
	l := layout(c)
	components := c.Instance().Spec.Components
	if _, ok := components[common.ComponentEngine]; ok {
		status = status.WithDesiredReplicas(common.ComponentEngine, int32(l.hosts()))
	}
	if _, ok := components[common.ComponentKeeper]; ok && l.keeper {
		status = status.WithDesiredReplicas(common.ComponentKeeper, common.KeeperReplicas)
	}
	return status
}

// observation is what Status reads about the cluster.
type observation struct {
	// replicated is true when the Instance runs ClickHouse Keeper.
	replicated bool
	// chi and chk are the installations as the API server has them, nil
	// until they exist. chk is read in the replicated topology only.
	chi *chiv1.ClickHouseInstallation
	chk *chkv1.ClickHouseKeeperInstallation
	// tls is true when TLS is on, and certificateIssued once cert-manager
	// has first issued the server certificate.
	tls, certificateIssued bool
	// hosts are the StatefulSets the operator runs for the ClickHouse hosts,
	// one per host.
	hosts []appsv1.StatefulSet
	// pods and keeperPods are the ClickHouse and the Keeper pods.
	pods, keeperPods []corev1.Pod
}

// observeCluster reads the installations, the ClickHouse hosts and the pods.
func observeCluster(c *controller.Context) (observation, error) {
	o := observation{
		replicated: c.Instance().GetTopologyType() == common.TopologyReplicated,
		tls:        tlsEnabled(c),
	}

	chi := &chiv1.ClickHouseInstallation{}
	found, err := getUncached(c, chiv1.SchemeGroupVersion.WithKind(chiv1.ClickHouseInstallationCRDResourceKind), c.Name(), chi)
	if err != nil {
		return o, err
	}
	if found {
		o.chi = chi
	}

	if o.replicated {
		chk := &chkv1.ClickHouseKeeperInstallation{}
		gvk := chkv1.SchemeGroupVersion.WithKind(chkv1.ClickHouseKeeperInstallationCRDResourceKind)
		found, err := getUncached(c, gvk, keeperCRName(c.Name()), chk)
		if err != nil {
			return o, err
		}
		if found {
			o.chk = chk
		}
		keeperPods := &corev1.PodList{}
		if err := c.List(keeperPods, client.MatchingLabels{common.LabelCHKName: keeperCRName(c.Name())}); err != nil {
			return o, fmt.Errorf("list Keeper pods: %w", err)
		}
		o.keeperPods = keeperPods.Items
	}

	if o.tls {
		if o.certificateIssued, err = serverCertificateIssued(c); err != nil {
			return o, err
		}
	}

	hosts := &appsv1.StatefulSetList{}
	if err := c.List(hosts, client.MatchingLabels{common.LabelCHIName: c.Name()}); err != nil {
		return o, fmt.Errorf("list ClickHouse StatefulSets: %w", err)
	}
	o.hosts = hosts.Items
	pods := &corev1.PodList{}
	if err := c.List(pods, client.MatchingLabels{common.LabelCHIName: c.Name()}); err != nil {
		return o, fmt.Errorf("list ClickHouse pods: %w", err)
	}
	o.pods = pods.Items
	return o, nil
}

// getUncached reads the named object from the API server instead of the
// cache into out, reporting whether it exists. Sync has just applied the
// installations, and the cache may still hold them from before that: their
// status would then read as done with the task the provider applied before
// the latest one. The client reads unstructured objects past the cache.
func getUncached(c *controller.Context, gvk schema.GroupVersionKind, name string, out any) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(obj, name); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get %s %q: %w", gvk.Kind, name, err)
	}
	raw, err := obj.MarshalJSON()
	if err != nil {
		return false, fmt.Errorf("encode %s %q: %w", gvk.Kind, name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("decode %s %q: %w", gvk.Kind, name, err)
	}
	return true, nil
}

// observe turns what Status read into the facts the provider reports.
func observe(o observation) controller.Status {
	var status controller.Status
	status.Available, status.AvailableReason, status.AvailableMessage = availability(o)

	installations := o.installations()
	for _, i := range installations {
		status = i.reportProblems(status)
	}

	status.Message = waitingFor(o, installations)
	status.Converged = status.Message == ""

	if message, rolling := imageRollout(o); rolling && !status.Converged {
		status = status.WithOperation(corev1alpha1.ConditionUpgrading, reasonRollingOutImage, message)
	}
	return status
}

// installations returns what the operator reports about the installations
// the Instance runs, Keeper first: ClickHouse is created once Keeper is
// ready.
func (o observation) installations() []installation {
	var list []installation
	if o.replicated {
		list = append(list, chkInstallation(o.chk))
	}
	return append(list, chiInstallation(o.chi))
}

// waitingFor says what keeps the cluster from running what the Instance
// asks for, or "" when nothing does.
func waitingFor(o observation, installations []installation) string {
	for _, i := range installations {
		if wait := i.waitingFor(); wait != "" {
			return wait
		}
	}
	if o.tls && !o.certificateIssued {
		return "Waiting for the TLS certificate"
	}
	return ""
}

// installation is what the operator reports about a ClickHouseInstallation
// or ClickHouseKeeperInstallation.
type installation struct {
	// component names it to users, kind is its kind.
	component, kind string
	exists          bool
	deleting        bool
	// stopped and suspended are set when spec.stop or spec.suspend is.
	stopped, suspended bool
	// taskID is the task the provider stamped on the rendering it applied
	// last.
	taskID *chtypes.Id
	// state is the operator's status.status: InProgress, Completed,
	// Aborted or Terminating.
	state string
	// completedTasks lists the tasks the operator finished, successfully or
	// not.
	completedTasks []string
	hosts          int
	// hostsCompleted and hostsFailed count the hosts the current reconcile
	// has finished, and those it could not reconcile.
	hostsCompleted, hostsFailed int
	// lastError is the operator's latest error.
	lastError string
}

func chiInstallation(chi *chiv1.ClickHouseInstallation) installation {
	i := installation{component: "ClickHouse", kind: chiv1.ClickHouseInstallationCRDResourceKind}
	if chi == nil {
		return i
	}
	s := chi.Status
	i.exists = true
	i.deleting = !chi.DeletionTimestamp.IsZero()
	i.stopped, i.suspended = chi.Spec.Stop.Value(), chi.Spec.Suspend.Value()
	i.taskID = chi.Spec.TaskID
	i.state, i.completedTasks = s.GetStatus(), s.GetTaskIDsCompleted()
	i.hosts, i.hostsCompleted, i.hostsFailed = s.GetHostsCount(), s.GetHostsCompletedCount(), s.GetHostsFailedCount()
	i.lastError = latestError(s.GetErrors(), s.GetError())
	return i
}

func chkInstallation(chk *chkv1.ClickHouseKeeperInstallation) installation {
	i := installation{component: "ClickHouse Keeper", kind: chkv1.ClickHouseKeeperInstallationCRDResourceKind}
	if chk == nil {
		return i
	}
	s := chk.Status
	i.exists = true
	i.deleting = !chk.DeletionTimestamp.IsZero()
	i.stopped, i.suspended = chk.Spec.Stop.Value(), chk.Spec.Suspend.Value()
	i.taskID = chk.Spec.TaskID
	i.state, i.completedTasks = s.GetStatus(), s.GetTaskIDsCompleted()
	i.hosts, i.hostsCompleted, i.hostsFailed = s.GetHostsCount(), s.GetHostsCompletedCount(), s.GetHostsFailedCount()
	i.lastError = latestError(s.GetErrors(), s.GetError())
	return i
}

// latestError returns the newest of the operator's errors without the time
// it prefixes them with, else its last error. The operator keeps both after
// a later success.
func latestError(errs []string, last string) string {
	if len(errs) == 0 {
		return strings.TrimSpace(last)
	}
	latest := errs[0]
	if stamp, rest, ok := strings.Cut(latest, " "); ok {
		if _, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			latest = rest
		}
	}
	return strings.TrimSpace(latest)
}

// finished reports whether the operator has finished the task the provider
// applied last, successfully or not.
func (i installation) finished() bool {
	return rolledOut(i.taskID, i.completedTasks)
}

// rolledOut reports whether the operator runs the rendering the provider
// applied last.
func (i installation) rolledOut() bool {
	return i.exists && !i.deleting && i.state == chiv1.StatusCompleted && i.finished()
}

// aborted reports whether the operator gave up on the task the provider
// applied last. An abort of an earlier task says nothing about the latest.
func (i installation) aborted() bool {
	return i.exists && !i.deleting && i.state == chiv1.StatusAborted && i.finished()
}

// abort returns the reason the operator tagged its latest error with, if
// any, and the error.
func (i installation) abort() (reason, message string) {
	message = i.lastError
	if rest, ok := strings.CutPrefix(message, "["); ok {
		if tag, text, ok := strings.Cut(rest, "]"); ok && tag != "" && !strings.ContainsAny(tag, " \t") {
			reason, message = tag, strings.TrimSpace(text)
		}
	}
	return reason, cmp.Or(message, "the operator aborted the last change")
}

// waitingFor says what the installation waits for, or "" once the operator
// runs the rendering the provider applied last.
func (i installation) waitingFor() string {
	switch {
	case !i.exists:
		return "Creating " + i.component
	case i.deleting:
		return fmt.Sprintf("Waiting for the operator to delete %s before it is created again", i.component)
	case i.stopped:
		return fmt.Sprintf("%s is stopped on its %s object", i.component, i.kind)
	case i.rolledOut() && i.hostsFailed > 0:
		return fmt.Sprintf("The operator completed the change to %s without %d of its %d hosts", i.component, i.hostsFailed, i.hosts)
	case i.rolledOut():
		return ""
	case i.suspended:
		return fmt.Sprintf("%s is suspended on its %s object", i.component, i.kind)
	case i.aborted():
		reason, message := i.abort()
		if slices.Contains(deferredReasons, reason) {
			return fmt.Sprintf("Waiting for a peer to become ready before the operator goes on with %s: %s", i.component, message)
		}
		return fmt.Sprintf("The operator aborted the change to %s: %s", i.component, message)
	case !i.finished() && (i.state == chiv1.StatusCompleted || i.state == chiv1.StatusAborted):
		return "Waiting for the operator to pick up the change to " + i.component
	}

	verb := "Creating"
	if len(i.completedTasks) > 0 {
		verb = "Updating"
	}
	message := fmt.Sprintf("%s %s", verb, i.component)
	if i.hosts > 0 {
		message = fmt.Sprintf("%s: %d of %d hosts ready", message, i.hostsCompleted, i.hosts)
	}
	return message
}

// reportProblems adds what is wrong with the installation to status.
func (i installation) reportProblems(status controller.Status) controller.Status {
	switch {
	case !i.exists || i.deleting:
		return status
	case i.stopped:
		return status.WithTerminalProblem(reasonStoppedOutsideOpenEverest, fmt.Sprintf(
			"%s is stopped: spec.stop is set on its %s object, outside OpenEverest. Unset it to start %s again.",
			i.component, i.kind, i.component))
	case i.suspended:
		status = status.WithTerminalProblem(reasonSuspendedOutsideOpenEverest, fmt.Sprintf(
			"The operator does not act on %s: spec.suspend is set on its %s object, outside OpenEverest. Unset it to apply changes again.",
			i.component, i.kind))
	}
	if i.rolledOut() && i.hostsFailed > 0 {
		message := fmt.Sprintf("%s: the operator could not reconcile %d of %d hosts", i.component, i.hostsFailed, i.hosts)
		if i.lastError != "" {
			message += ", its latest error: " + i.lastError
		}
		return status.WithProblem(reasonHostsFailed, message)
	}
	if !i.aborted() {
		return status
	}

	reason, message := i.abort()
	message = fmt.Sprintf("%s: %s", i.component, message)
	switch {
	case slices.Contains(deferredReasons, reason):
		// The operator goes on by itself once the peer is back; a peer that
		// does not come back shows in the pods' readiness.
		return status
	case slices.Contains(specRejectedReasons, reason):
		// Only a change of the spec makes the operator try again.
		return status.WithTerminalProblem(reason, message)
	default:
		return status.WithProblem(cmp.Or(reason, corev1alpha1.ReasonEngineError), message)
	}
}

// availability says whether clients can connect. They reach ClickHouse
// through the client Service, which routes to the hosts the operator marks
// ready, and a query over the cluster needs a host of every shard. Writes to
// replicated tables need Keeper too: without a Keeper quorum ClickHouse turns
// them read-only.
func availability(o observation) (controller.Availability, string, string) {
	if o.chi == nil {
		return controller.AvailabilityUnreported, "", ""
	}
	if o.chi.Spec.Stop.Value() {
		return controller.Unavailable, reasonStopped, "ClickHouse is stopped on its ClickHouseInstallation object"
	}
	shards := shardsOf(o.hosts)
	if len(shards) == 0 {
		// The operator has created no host yet. The runtime knows whether
		// the cluster has ever been available.
		return controller.AvailabilityUnreported, "", ""
	}

	// A Keeper node answers its readiness probe only while it follows a
	// leader, so one ready node means the ensemble has a quorum.
	if chiKeeperPort(o.chi) >= 0 && !slices.ContainsFunc(o.keeperPods, podReady) {
		return controller.Unavailable, reasonKeeperUnavailable,
			"No ClickHouse Keeper node is ready: without a Keeper quorum, replicated tables are read-only"
	}

	var down []string
	for _, shard := range shards {
		if !slices.ContainsFunc(o.pods, func(pod corev1.Pod) bool {
			return pod.Labels[common.LabelCHIShard] == shard && serving(pod)
		}) {
			down = append(down, shard)
		}
	}
	switch {
	case len(down) == 0:
		return controller.Available, "", ""
	case len(shards) == 1:
		return controller.Unavailable, reasonNoReplicaReady, "No ClickHouse host is ready"
	case len(down) == 1:
		return controller.Unavailable, reasonNoReplicaReady, "No ClickHouse replica is ready in shard " + down[0]
	default:
		return controller.Unavailable, reasonNoReplicaReady, "No ClickHouse replica is ready in shards " + strings.Join(down, ", ")
	}
}

// shardsOf returns the shards the ClickHouse hosts belong to, in order.
func shardsOf(hosts []appsv1.StatefulSet) []string {
	var shards []string
	for i := range hosts {
		if shard := hosts[i].Labels[common.LabelCHIShard]; shard != "" && hosts[i].DeletionTimestamp.IsZero() &&
			!slices.Contains(shards, shard) {
			shards = append(shards, shard)
		}
	}
	slices.SortFunc(shards, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	})
	return shards
}

// serving reports whether the client Service routes to the ClickHouse pod:
// the operator marks it ready, and the kubelet reports it Ready.
func serving(pod corev1.Pod) bool {
	return pod.Labels[common.LabelCHIReady] == common.LabelCHIReadyValue && podReady(pod)
}

// podReady reports whether the kubelet reports the pod Ready and its deletion
// has not started.
func podReady(pod corev1.Pod) bool {
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// imageRollout says which image is rolling out while Keeper or ClickHouse
// pods run another image than their installation asks for.
func imageRollout(o observation) (string, bool) {
	if o.chk != nil {
		wanted := templateImage(o.chk.Spec.Templates, common.KeeperPodTemplateName, common.ContainerKeeper)
		if message, rolling := podsOnImage("ClickHouse Keeper", wanted, common.ContainerKeeper, o.keeperPods); rolling {
			return message, true
		}
	}
	if o.chi != nil {
		wanted := templateImage(o.chi.Spec.Templates, common.PodTemplateName, common.ContainerClickHouse)
		return podsOnImage("ClickHouse", wanted, common.ContainerClickHouse, o.pods)
	}
	return "", false
}

// templateImage returns the image of a container in a pod template.
func templateImage(templates *chiv1.Templates, template, container string) string {
	if templates == nil {
		return ""
	}
	for _, t := range templates.PodTemplates {
		if t.Name != template {
			continue
		}
		for _, c := range t.Spec.Containers {
			if c.Name == container {
				return c.Image
			}
		}
	}
	return ""
}

// podsOnImage says how far the pods are in moving the container to image,
// and whether any pod still runs another one.
func podsOnImage(component, image, container string, pods []corev1.Pod) (string, bool) {
	if image == "" {
		return "", false
	}
	var total, updated int
	for _, pod := range pods {
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if c.Name != container {
				continue
			}
			total++
			if c.Image == image {
				updated++
			}
		}
	}
	if updated == total {
		return "", false
	}
	return fmt.Sprintf("Rolling out %s image %s (%d of %d pods updated)", component, image, updated, total), true
}

// instanceForPod maps a ClickHouse or Keeper pod to its Instance, by the
// labels the provider sets on the pods of each component.
func instanceForPod(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	name := labels[controller.InstanceLabel]
	if labels[controller.ProviderLabel] != common.ProviderName || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// podServingChanged passes the pod events that can change whether clients
// can connect: a pod that appears or goes, starts being deleted, becomes
// Ready or not, or gains or loses the operator's ready mark. Neither the
// pods' readiness nor the mark shows in the installations' status.
func podServingChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			return okOld && okNew && (podReady(*oldPod) != podReady(*newPod) ||
				oldPod.Labels[common.LabelCHIReady] != newPod.Labels[common.LabelCHIReady])
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
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
