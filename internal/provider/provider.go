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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// Compile-time check.
var _ controller.ProviderInterface = (*Provider)(nil)

// Provider implements controller.ProviderInterface for ClickHouse via the Altinity operator.
type Provider struct {
	controller.BaseProvider
}

// New creates a new Provider instance.
func New() *Provider {
	return &Provider{
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			SchemeFuncs: []func(*runtime.Scheme) error{
				chiv1.AddToScheme,
				chkv1.AddToScheme,
				monitoringv1.AddToScheme,
				cmapi.AddToScheme,
				networkingv1.AddToScheme,
				storagev1.AddToScheme,
			},
			// Safe to watch: server-side apply of an unchanged CHI/CHK is a
			// no-op, so operator status/finalizer writes do not loop back.
			WatchConfigs: []controller.WatchConfig{
				controller.WatchOwned(&chiv1.ClickHouseInstallation{}),
				controller.WatchOwned(&chkv1.ClickHouseKeeperInstallation{}),
				// The credentials Secret, so editing a password rotates it. Only
				// this provider's Secrets: other providers' Instances own Secrets
				// too, and the runtime would reconcile those Instances here.
				controller.WatchOwned(&corev1.Secret{}, predicate.NewPredicateFuncs(func(o client.Object) bool {
					return o.GetLabels()[controller.ProviderLabel] == common.ProviderName
				})),
				controller.WatchOwned(&networkingv1.NetworkPolicy{}),
				// A LoadBalancer address goes into the server certificate.
				controller.WatchExternal(&corev1.Service{},
					handler.EnqueueRequestsFromMapFunc(instanceForClientService),
					loadBalancerAddressChanged()),
			},
		},
	}
}

// Sync provisions the required resources and, while the operator is still
// working, publishes the intermediate phase onto the Instance.
//
// The runtime only persists status after Sync returns nil; on the WaitError
// (requeue) path it short-circuits before computing status. Without this the
// Instance would keep its previous phase for the whole provisioning or update
// window. We therefore publish what Status() reports (Provisioning, Updating
// or Failed) each time we ask the runtime to requeue.
func (p *Provider) Sync(c *controller.Context) error {
	err := p.sync(c)
	if controller.IsWaitError(err) {
		if st, statusErr := p.Status(c); statusErr == nil {
			reportStatus(c, st)
		}
	}
	return err
}

// reportStatus best-effort publishes the status (Provisioning, Updating or
// Failed) to the Instance while Sync is still waiting on the operator. The metadata carried by
// c.Instance() is preserved by the runtime's prepare step, so a status-only
// update targets the live object safely.
//
// Pending maintenance is published here too: the runtime only flushes it after
// a Sync that returns nil, so an approved change that starts a rollout would
// otherwise stay listed as pending until the cluster is Ready again.
func reportStatus(c *controller.Context, st controller.Status) {
	in := c.Instance()
	desired := st.ToV2Alpha1()
	changed := in.Status.Phase != desired.Phase || in.Status.Message != desired.Message
	in.Status.Phase = desired.Phase
	in.Status.Message = desired.Message
	if c.MaintenanceRequested() && setPendingMaintenance(c, in) {
		changed = true
	}
	if changed {
		_ = c.Client().Status().Update(c.Context(), in)
	}
}

// setPendingMaintenance mirrors the runtime's flush of status.pendingMaintenance
// and the MaintenancePending condition, and reports whether either changed.
func setPendingMaintenance(c *controller.Context, in *corev1alpha1.Instance) bool {
	pending := c.GetPendingMaintenance()
	changed := !equality.Semantic.DeepEqual(in.Status.PendingMaintenance, pending)
	in.Status.PendingMaintenance = pending

	condition := metav1.Condition{
		Type:               corev1alpha1.ConditionMaintenancePending,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ReasonAwaitingApproval,
		Message:            fmt.Sprintf("%d action(s) require approval to proceed", len(pending)),
		ObservedGeneration: in.Generation,
	}
	if len(pending) == 0 {
		if meta.FindStatusCondition(in.Status.Conditions, corev1alpha1.ConditionMaintenancePending) == nil {
			return changed
		}
		condition.Status = metav1.ConditionFalse
		condition.Reason = corev1alpha1.ReasonNoActionsPending
		condition.Message = "No disruptive actions are held"
	}
	if meta.SetStatusCondition(&in.Status.Conditions, condition) {
		changed = true
	}
	return changed
}

// sync applies and polls the required resources for the selected topology.
//
// CHI/CHK are server-side applied on every reconcile, so spec changes on the
// Instance roll out while fields written by the Altinity operator are left
// alone. WaitError is returned while provisioning is in progress so the
// runtime requeues after 15s.
func (p *Provider) sync(c *controller.Context) error {
	l := log.FromContext(c.Context())
	topology := c.Instance().GetTopologyType()
	l.Info("Syncing ClickHouse instance", "name", c.Name(), "topology", topology)

	// Provision the application user credentials before creating the CHI so the
	// operator can resolve the referenced Secret on first apply.
	if _, err := ensureCredentials(c); err != nil {
		return fmt.Errorf("ensure credentials: %w", err)
	}

	// Provision TLS certificates before creating the CHI so the operator can
	// mount the server certificate secret. Returns a WaitError until issued.
	if tlsEnabled(c) {
		if err := ensureTLS(c); err != nil {
			return err
		}
	}

	var syncErr error
	switch topology {
	case common.TopologyReplicated:
		syncErr = p.syncReplicated(c)
	default:
		// standalone (and any unknown topology falls through to standalone)
		syncErr = p.syncStandalone(c)
	}

	// The PodMonitor is independent of CHI readiness, so reconcile it even while
	// the CHI is still provisioning (syncErr may be a WaitError).
	if err := reconcilePodMonitor(c); err != nil {
		return err
	}
	return syncErr
}

// syncStandalone applies and waits on the CHI for a single-node deployment.
func (p *Provider) syncStandalone(c *controller.Context) error {
	chi, err := buildCHI(c, 1)
	if err != nil {
		return fmt.Errorf("build ClickHouseInstallation: %w", err)
	}
	gate, err := newRolloutGate(c, chi)
	if err != nil {
		return err
	}
	return applyAndWaitForCHI(c, gate, chi)
}

// syncReplicated applies and waits on a CHK (Keeper) + CHI pair.
//
// Keeper TLS switches in steps (see keeperWiring): the CHK and CHI rendered
// here are the next step, and the approval covers the whole switch.
func (p *Provider) syncReplicated(c *controller.Context) error {
	step, target, err := planKeeperWiring(c)
	if err != nil {
		return err
	}
	desiredCHK, err := buildCHKWired(c, step)
	if err != nil {
		return fmt.Errorf("build ClickHouseKeeperInstallation: %w", err)
	}
	desiredCHI, err := buildCHIWired(c, replicasCount(c), step)
	if err != nil {
		return fmt.Errorf("build ClickHouseInstallation: %w", err)
	}
	targetCHK, err := buildCHKWired(c, target)
	if err != nil {
		return fmt.Errorf("build ClickHouseKeeperInstallation: %w", err)
	}
	targetCHI, err := buildCHIWired(c, replicasCount(c), target)
	if err != nil {
		return fmt.Errorf("build ClickHouseInstallation: %w", err)
	}
	gate, err := newRolloutGate(c, targetCHK, targetCHI)
	if err != nil {
		return err
	}

	if _, err := gate.apply(c, desiredCHK, "chk"); err != nil {
		return fmt.Errorf("apply ClickHouseKeeperInstallation: %w", err)
	}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	if err := c.Get(chk, keeperCRName(c.Name())); err != nil {
		return controller.WaitForDuration("waiting for Keeper to initialize", 15*time.Second)
	}
	// ClickHouse needs a Keeper quorum to start, so hold the CHI until it is ready.
	if err := waitForCHK(c, chk); err != nil {
		return err
	}

	if err := applyAndWaitForCHI(c, gate, desiredCHI); err != nil {
		return err
	}
	if step != target {
		return controller.WaitForDuration("switching ClickHouse and Keeper to the new Keeper ports", 5*time.Second)
	}
	return nil
}

// applyAndWaitForCHI server-side applies the CHI, unless the rollout gate holds
// it, and returns a WaitError until the operator reports it Completed.
func applyAndWaitForCHI(c *controller.Context, gate *rolloutGate, chi *chiv1.ClickHouseInstallation) error {
	if _, err := gate.apply(c, chi, "chi"); err != nil {
		return fmt.Errorf("apply ClickHouseInstallation: %w", err)
	}

	current := &chiv1.ClickHouseInstallation{}
	if err := c.Get(current, c.Name()); err != nil {
		return controller.WaitForDuration("waiting for Altinity operator to provision ClickHouseInstallation", 15*time.Second)
	}
	if err := reconcilePlaintextPolicy(c, current); err != nil {
		return err
	}
	if err := waitForCHI(c, current); err != nil {
		return err
	}
	if !tlsEnabled(c) {
		return cleanupTLS(c, current)
	}
	return nil
}

// waitForCHI checks CHI status and returns a WaitError if not yet Completed.
func waitForCHI(c *controller.Context, chi *chiv1.ClickHouseInstallation) error {
	l := log.FromContext(c.Context())

	if chi.Status == nil {
		return controller.WaitForDuration("waiting for Altinity operator to initialize CHI", 15*time.Second)
	}
	switch chi.Status.GetStatus() {
	case chiv1.StatusCompleted:
		if !rolledOut(chi.Spec.TaskID, chi.Status.GetTaskIDsCompleted()) {
			return controller.WaitForDuration("waiting for Altinity operator to pick up the ClickHouseInstallation change", 15*time.Second)
		}
		l.Info("ClickHouseInstallation is Completed", "name", chi.Name)
		return nil
	case chiv1.StatusAborted:
		// The operator retries aborted reconciles, so keep watching; Sync's
		// wait path reports the failure (a plain error would publish nothing).
		return controller.WaitForDuration("ClickHouseInstallation aborted: "+chi.Status.GetError(), 30*time.Second)
	default:
		l.Info("ClickHouseInstallation still provisioning", "name", chi.Name, "status", chi.Status.GetStatus())
		return controller.WaitForDuration("waiting for Altinity operator to complete CHI provisioning", 15*time.Second)
	}
}

// waitForCHK checks CHK status and returns a WaitError if not yet Completed.
func waitForCHK(c *controller.Context, chk *chkv1.ClickHouseKeeperInstallation) error {
	l := log.FromContext(c.Context())

	if chk.Status == nil {
		return controller.WaitForDuration("waiting for Keeper to initialize", 15*time.Second)
	}
	switch chk.Status.GetStatus() {
	case chkv1.StatusCompleted:
		if !rolledOut(chk.Spec.TaskID, chk.Status.GetTaskIDsCompleted()) {
			return controller.WaitForDuration("waiting for Altinity operator to pick up the Keeper change", 15*time.Second)
		}
		l.Info("ClickHouseKeeperInstallation is Completed", "name", chk.Name)
		return nil
	case chkv1.StatusAborted:
		return controller.WaitForDuration("ClickHouseKeeperInstallation aborted: "+chk.Status.GetError(), 30*time.Second)
	default:
		l.Info("Keeper still provisioning", "name", chk.Name, "status", chk.Status.GetStatus())
		return controller.WaitForDuration("waiting for Keeper to complete provisioning", 15*time.Second)
	}
}

// Cleanup removes the CHI (and CHK for replicated) when the Instance is deleted.
//
// ClickHouse is torn down before Keeper, so replicas never lose their
// coordination service while shutting down. With the Delete storage reclaim
// policy the leftover PVCs are removed once both are gone (see deletePVCs).
func (p *Provider) Cleanup(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up ClickHouse instance", "name", c.Name())

	if err := syncReclaimPolicy(c); err != nil {
		return err
	}

	chi := &chiv1.ClickHouseInstallation{ObjectMeta: c.ObjectMeta(c.Name())}
	if err := c.Delete(chi); err != nil {
		return fmt.Errorf("delete ClickHouseInstallation: %w", err)
	}

	if err := deletePodMonitor(c); err != nil {
		return err
	}

	credentials := &corev1.Secret{ObjectMeta: c.ObjectMeta(credentialsSecretName(c.Name()))}
	if err := c.Delete(credentials); err != nil {
		return fmt.Errorf("delete credentials secret: %w", err)
	}

	state := &corev1.Secret{ObjectMeta: c.ObjectMeta(rolloutStateName(c.Name()))}
	if err := c.Delete(state); err != nil {
		return fmt.Errorf("delete rollout state secret: %w", err)
	}

	// Also when TLS is off now: certificates of an earlier TLS setting may
	// still be around.
	if err := deleteTLSResources(c); err != nil {
		return err
	}

	if err := waitForDeletion(c, &chiv1.ClickHouseInstallation{}, c.Name()); err != nil {
		return err
	}

	if c.Instance().GetTopologyType() == common.TopologyReplicated {
		keeperName := keeperCRName(c.Name())
		chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: c.ObjectMeta(keeperName)}
		if err := c.Delete(chk); err != nil {
			return fmt.Errorf("delete ClickHouseKeeperInstallation: %w", err)
		}
		if err := waitForDeletion(c, &chkv1.ClickHouseKeeperInstallation{}, keeperName); err != nil {
			return err
		}
	}

	if storageReclaimPolicy(c) == common.StorageReclaimPolicyDelete {
		if err := deletePVCs(c); err != nil {
			return err
		}
	}

	l.Info("ClickHouse instance cleaned up", "name", c.Name())
	return nil
}

// syncReclaimPolicy aligns the reclaim policy the operator acts on during
// deletion with the Instance spec. The operator reads it from the volume claim
// templates of the CR being deleted and from the reclaim label of each PVC,
// while the runtime no longer runs Sync once deletion has started: a policy
// change that has not reached the CRs yet — or a CR created before the policy
// existed, which the operator treats as Delete — would otherwise decide the
// fate of the data.
func syncReclaimPolicy(c *controller.Context) error {
	policy := chiv1.PVCReclaimPolicy(storageReclaimPolicy(c))

	chi := &chiv1.ClickHouseInstallation{}
	if err := patchVolumeClaimTemplates(c, chi, c.Name(), policy, func() *chiv1.Templates { return chi.Spec.Templates }); err != nil {
		return err
	}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	if err := patchVolumeClaimTemplates(c, chk, keeperCRName(c.Name()), policy, func() *chiv1.Templates { return chk.Spec.Templates }); err != nil {
		return err
	}

	if err := labelPVCs(c, client.MatchingLabels{common.LabelCHIName: c.Name()}, common.LabelCHIReclaimPolicy, string(policy)); err != nil {
		return err
	}
	return labelPVCs(c, client.MatchingLabels{common.LabelCHKName: keeperCRName(c.Name())}, common.LabelCHKReclaimPolicy, string(policy))
}

// patchVolumeClaimTemplates sets the reclaim policy on every volume claim
// template of the named CR, if it exists. A JSON patch touches only those
// fields, so nothing else of the operator-owned spec is rewritten.
func patchVolumeClaimTemplates(c *controller.Context, obj client.Object, name string, policy chiv1.PVCReclaimPolicy, templates func() *chiv1.Templates) error {
	if err := c.Get(obj, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %T %s: %w", obj, name, err)
	}
	t := templates()
	if t == nil {
		return nil
	}

	var ops []map[string]any
	for i, vct := range t.VolumeClaimTemplates {
		if vct.PVCReclaimPolicy == policy {
			continue
		}
		path := fmt.Sprintf("/spec/templates/volumeClaimTemplates/%d", i)
		ops = append(ops,
			// Guard against the list having changed since it was read.
			map[string]any{"op": "test", "path": path + "/name", "value": vct.Name},
			map[string]any{"op": "add", "path": path + "/reclaimPolicy", "value": string(policy)},
		)
	}
	if len(ops) == 0 {
		return nil
	}
	data, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshal reclaim policy patch: %w", err)
	}
	if err := c.Client().Patch(c.Context(), obj, client.RawPatch(types.JSONPatchType, data)); err != nil {
		return fmt.Errorf("set reclaim policy on %T %s: %w", obj, name, err)
	}
	return nil
}

// labelPVCs sets the operator's reclaim label on the selected PVCs.
func labelPVCs(c *controller.Context, selector client.MatchingLabels, key, policy string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(pvcs, selector); err != nil {
		return fmt.Errorf("list PVCs: %w", err)
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Labels[key] == policy {
			continue
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Labels[key] = policy
		if err := c.Client().Patch(c.Context(), pvc, patch); err != nil {
			return fmt.Errorf("label PVC %s: %w", pvc.Name, err)
		}
	}
	return nil
}

// waitForDeletion returns a WaitError while the named object still exists.
func waitForDeletion(c *controller.Context, obj client.Object, name string) error {
	exists, err := c.Exists(obj, name)
	if err != nil {
		return fmt.Errorf("check %T %s: %w", obj, name, err)
	}
	if exists {
		return controller.WaitForDuration(fmt.Sprintf("waiting for %T %s to be deleted", obj, name), 10*time.Second)
	}
	return nil
}

// deletePVCs removes the ClickHouse and Keeper PVCs of the Instance. The
// operator deletes ClickHouse PVCs itself under the Delete policy, but its
// Keeper controller fails to list namespaced objects when watching all
// namespaces (controller-runtime v0.18 multi-namespace cache) and never
// purges Keeper PVCs. Running after both CRs are gone, this also catches PVCs
// created by a reconcile that was still in flight when deletion started.
func deletePVCs(c *controller.Context) error {
	selectors := []client.MatchingLabels{
		{common.LabelCHIName: c.Name()},
		{common.LabelCHKName: keeperCRName(c.Name())},
	}
	for _, selector := range selectors {
		pvcs := &corev1.PersistentVolumeClaimList{}
		if err := c.List(pvcs, selector); err != nil {
			return fmt.Errorf("list PVCs: %w", err)
		}
		for i := range pvcs.Items {
			if err := c.Delete(&pvcs.Items[i]); err != nil {
				return fmt.Errorf("delete PVC %s: %w", pvcs.Items[i].Name, err)
			}
		}
	}
	return nil
}

// =============================================================================
// Builders
// =============================================================================

// buildCHI constructs a ClickHouseInstallation for the given replica count,
// wired to Keeper as the current step of the Keeper TLS switch calls for.
func buildCHI(c *controller.Context, replicasCount int) (*chiv1.ClickHouseInstallation, error) {
	step, _, err := planKeeperWiring(c)
	if err != nil {
		return nil, err
	}
	return buildCHIWired(c, replicasCount, step)
}

// buildCHIWired constructs a ClickHouseInstallation wired to Keeper as given.
func buildCHIWired(c *controller.Context, replicasCount int, keeper keeperWiring) (*chiv1.ClickHouseInstallation, error) {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	image, err := resolveImage(c, common.ComponentEngine, engine)
	if err != nil {
		return nil, err
	}
	resources := resolveResources(engine)
	storageSize, storageClass := resolveStorage(engine)

	settings, err := resolveEngineSettings(c, engine)
	if err != nil {
		return nil, err
	}

	container := corev1.Container{
		Name:  "clickhouse",
		Image: image,
		// Named port for the native Prometheus endpoint, targeted by the PodMonitor.
		Ports: []corev1.ContainerPort{
			{Name: common.MetricsPortName, ContainerPort: common.MetricsPort, Protocol: corev1.ProtocolTCP},
		},
		Resources: resources,
	}

	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize}},
	}
	if storageClass != nil {
		pvcSpec.StorageClassName = storageClass
	}

	cluster := &chiv1.Cluster{
		Name: common.CHIClusterName,
		Layout: &chiv1.ChiClusterLayout{
			ShardsCount:   1,
			ReplicasCount: replicasCount,
		},
		Templates: &chiv1.TemplatesList{
			PodTemplate:             common.PodTemplateName,
			DataVolumeClaimTemplate: common.DataVolumeClaimTemplateName,
		},
	}

	podLabels := c.PodLabels(common.ComponentEngine)
	secretName := credentialsSecretName(c.Name())
	username, podAnnotations, err := credentialsForCHI(c, secretName)
	if err != nil {
		return nil, err
	}
	applyInterserverAuth(settings, cluster, secretName)
	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
	applySchedulingPolicy(&podSpec, engine.SchedulingPolicy, podLabels)

	spec := chiv1.ChiSpec{
		Configuration: &chiv1.Configuration{
			Clusters: []*chiv1.Cluster{cluster},
			Users:    buildUserSettings(secretName, username),
		},
		Templates: &chiv1.Templates{
			PodTemplates: []chiv1.PodTemplate{{
				Name: common.PodTemplateName,
				// The operator copies these onto the pod template only, never onto the
				// StatefulSet selector, so existing clusters roll once and keep their selectors.
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
				Spec:       podSpec,
			}},
			VolumeClaimTemplates: []chiv1.VolumeClaimTemplate{{
				Name:              common.DataVolumeClaimTemplateName,
				StorageManagement: storageManagement(c),
				Spec:              pvcSpec,
			}},
		},
	}

	if settings != nil {
		spec.Configuration.Settings = settings
	}

	// Wire TLS: mount the server certificate, open the secure ports and run
	// distributed queries over them.
	mode := tlsMode(c)
	switch {
	case mode != "":
		applyTLSToCHISpec(&spec, settings, cluster, c.Name())
	case keeper.port == common.KeeperSecurePort:
		// TLS is being turned off: until ClickHouse is back on Keeper's
		// plaintext port, it still needs the CA to verify Keeper.
		spec.Configuration.Files = buildTLSFiles(serverSecretName(c.Name()))
	}

	// Wire the root Service exposure (ClusterIP / LoadBalancer / NodePort).
	configureExpose(&spec, engine.Service, mode)

	// Wire Keeper for replicated topology (see keeperZookeeper).
	if replicasCount > 1 {
		spec.Configuration.Zookeeper = keeperZookeeper(c, keeper.port)
	}

	return &chiv1.ClickHouseInstallation{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       spec,
	}, nil
}

// buildCHK constructs the 3-node (Raft quorum) ClickHouseKeeperInstallation
// from the keeper component, which is optional: defaults size it otherwise.
// Its ports follow the current step of the Keeper TLS switch.
func buildCHK(c *controller.Context) (*chkv1.ClickHouseKeeperInstallation, error) {
	step, _, err := planKeeperWiring(c)
	if err != nil {
		return nil, err
	}
	return buildCHKWired(c, step)
}

// buildCHKWired constructs the ClickHouseKeeperInstallation serving the
// client ports given. With TLS, Keeper serves the Instance's server
// certificate, also between its nodes.
func buildCHKWired(c *controller.Context, wiring keeperWiring) (*chkv1.ClickHouseKeeperInstallation, error) {
	keeper := c.Instance().Spec.Components[common.ComponentKeeper]
	image, err := resolveImage(c, common.ComponentKeeper, keeper)
	if err != nil {
		return nil, err
	}
	resources := resolveKeeperResources(keeper)
	storageSize, storageClass, err := resolveKeeperStorage(c, keeper)
	if err != nil {
		return nil, err
	}

	container := corev1.Container{
		Name:      "clickhouse-keeper",
		Image:     image,
		Resources: resources,
	}

	podSpec := corev1.PodSpec{Containers: []corev1.Container{container}}
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	applyKeeperSchedulingPolicy(&podSpec, engine.SchedulingPolicy, map[string]string{common.LabelCHKName: keeperCRName(c.Name())})

	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize}},
	}
	if storageClass != nil {
		pvcSpec.StorageClassName = storageClass
	}

	chk := &chkv1.ClickHouseKeeperInstallation{
		ObjectMeta: c.ObjectMeta(keeperCRName(c.Name())),
		Spec: chkv1.ChkSpec{
			Configuration: &chkv1.Configuration{
				Clusters: []*chkv1.Cluster{
					{
						Name: common.CHKClusterName,
						Layout: &chkv1.ChkClusterLayout{
							ReplicasCount: common.KeeperReplicas,
						},
						Templates: &chiv1.TemplatesList{
							PodTemplate:             common.KeeperPodTemplateName,
							DataVolumeClaimTemplate: common.KeeperDataVolumeClaimTemplateName,
						},
					},
				},
			},
			Templates: &chiv1.Templates{
				PodTemplates: []chiv1.PodTemplate{
					{
						Name: common.KeeperPodTemplateName,
						// Counted into status.components; the operator copies these onto
						// the pod template only, never onto the StatefulSet selector.
						ObjectMeta: metav1.ObjectMeta{Labels: c.PodLabels(common.ComponentKeeper)},
						Spec:       podSpec,
					},
				},
				VolumeClaimTemplates: []chiv1.VolumeClaimTemplate{
					{
						Name:              common.KeeperDataVolumeClaimTemplateName,
						StorageManagement: storageManagement(c),
						Spec:              pvcSpec,
					},
				},
			},
		},
	}
	cluster := chk.Spec.Configuration.Clusters[0]
	if wiring.secure {
		cluster.Secure = chtypes.NewStringBool(true)
		chk.Spec.Configuration.Files = buildTLSFiles(serverSecretName(c.Name()))
	}
	if !wiring.plaintext {
		cluster.Insecure = chtypes.NewStringBool(false)
	}
	return chk, nil
}

// =============================================================================
// Helpers
// =============================================================================

// keeperCRName returns the CHK resource name for a given instance.
func keeperCRName(instanceName string) string {
	return instanceName + "-keeper"
}

// configureExpose wires the root ClickHouse Service exposure onto the CHI spec
// based on the engine component's Service configuration. It adds a
// ServiceTemplate (referenced from spec.defaults) that overrides the Service
// type (ClusterIP, LoadBalancer, NodePort), annotations, and — for
// LoadBalancer — the allowed source ranges. When service is nil or the type is
// unset, the operator's default (headless ClusterIP) applies, and a template
// is added only to set the ports for TLS.
//
// tlsMode selects the ports: plaintext without TLS, plaintext and secure with
// "optional", and only the secure ones with "required".
func configureExpose(spec *chiv1.ChiSpec, service *corev1alpha1.Service, tlsMode string) {
	exposed := service != nil && service.ServiceType != ""
	if !exposed && tlsMode == "" {
		return
	}

	var ports []corev1.ServicePort
	if tlsMode != common.TLSModeRequired {
		ports = append(ports,
			corev1.ServicePort{
				Name:       chiv1.ChDefaultHTTPPortName,
				Protocol:   corev1.ProtocolTCP,
				Port:       chiv1.ChDefaultHTTPPortNumber,
				TargetPort: intstr.FromString(chiv1.ChDefaultHTTPPortName),
			},
			corev1.ServicePort{
				Name:       chiv1.ChDefaultTCPPortName,
				Protocol:   corev1.ProtocolTCP,
				Port:       chiv1.ChDefaultTCPPortNumber,
				TargetPort: intstr.FromString(chiv1.ChDefaultTCPPortName),
			},
		)
	}
	if tlsMode != "" {
		ports = append(ports,
			corev1.ServicePort{
				Name:       chiv1.ChDefaultHTTPSPortName,
				Protocol:   corev1.ProtocolTCP,
				Port:       chiv1.ChDefaultHTTPSPortNumber,
				TargetPort: intstr.FromString(chiv1.ChDefaultHTTPSPortName),
			},
			corev1.ServicePort{
				Name:       chiv1.ChDefaultTLSPortName,
				Protocol:   corev1.ProtocolTCP,
				Port:       chiv1.ChDefaultTLSPortNumber,
				TargetPort: intstr.FromString(chiv1.ChDefaultTLSPortName),
			},
		)
	}

	tmpl := chiv1.ServiceTemplate{
		Name: common.ServiceTemplateName,
		Spec: corev1.ServiceSpec{Ports: ports},
	}
	if exposed {
		tmpl.ObjectMeta.Annotations = service.Annotations
		tmpl.Spec.Type = service.ServiceType
		if service.ServiceType == corev1.ServiceTypeLoadBalancer && service.LoadBalancerService != nil {
			tmpl.Spec.LoadBalancerSourceRanges = service.LoadBalancerService.SourceRanges.NormalizedSourceRanges()
		}
	} else {
		// The operator's default Service is headless; clusterIP is immutable,
		// so keep it rather than have the Service recreated.
		tmpl.Spec.Type = corev1.ServiceTypeClusterIP
		tmpl.Spec.ClusterIP = corev1.ClusterIPNone
	}

	if spec.Templates == nil {
		spec.Templates = &chiv1.Templates{}
	}
	spec.Templates.ServiceTemplates = append(spec.Templates.ServiceTemplates, tmpl)

	if spec.Defaults == nil {
		spec.Defaults = &chiv1.Defaults{}
	}
	if spec.Defaults.Templates == nil {
		spec.Defaults.Templates = &chiv1.TemplatesList{}
	}
	spec.Defaults.Templates.ServiceTemplate = common.ServiceTemplateName
}

// replicasCount returns the configured replica count or the default.
func replicasCount(c *controller.Context) int {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Replicas != nil && *engine.Replicas >= 2 {
		return int(*engine.Replicas)
	}
	return common.DefaultReplicasCount
}

// resolveImage returns the container image for a component: an explicit
// image, else the component's version, else the version the Instance's bundle
// pins for it (the runtime only fills in versions of components present in
// spec.components, and only after Validate), else the type's default.
func resolveImage(c *controller.Context, name string, component corev1alpha1.ComponentSpec) (string, error) {
	if component.Image != "" {
		return component.Image, nil
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}
	version := component.Version
	if version == "" {
		if bundleName := controller.EffectiveVersionBundleName(spec, c.Instance()); bundleName != "" {
			if bundle, err := controller.ResolveVersionBundle(spec, bundleName); err == nil {
				version = bundle.Components[name]
			}
		}
	}
	if version != "" {
		if img := controller.GetImageForVersion(spec, name, version); img != "" {
			return img, nil
		}
	}
	if img := controller.GetDefaultImageForComponent(spec, name); img != "" {
		return img, nil
	}
	return "", fmt.Errorf("no image found for the %s component", name)
}

// resolveResources returns the engine's CPU and memory requirements,
// defaulting to 1 CPU and 4Gi (see resourceRequirements).
func resolveResources(engine corev1alpha1.ComponentSpec) corev1.ResourceRequirements {
	return resourceRequirements(engine.Resources, resource.MustParse("1"), resource.MustParse("4Gi"))
}

// resolveStorage returns the storage size and optional storage class.
func resolveStorage(engine corev1alpha1.ComponentSpec) (size resource.Quantity, storageClass *string) {
	size = resource.MustParse("25Gi")
	if engine.Storage == nil {
		return
	}
	if !engine.Storage.Size.IsZero() {
		size = engine.Storage.Size.DeepCopy()
	}
	storageClass = engine.Storage.StorageClass
	return
}

// resolveKeeperResources returns the Keeper CPU and memory requirements,
// defaulting to the small footprint Keeper needs (see resourceRequirements).
func resolveKeeperResources(keeper corev1alpha1.ComponentSpec) corev1.ResourceRequirements {
	return resourceRequirements(keeper.Resources,
		resource.MustParse(common.KeeperDefaultCPU), resource.MustParse(common.KeeperDefaultMemory))
}

// resourceRequirements fills in the CPU and memory requests and limits of a
// component. An unset limit is the default, raised to the request when that
// is higher; an unset request equals the limit, so a component that sets only
// limits gets the Guaranteed QoS class.
func resourceRequirements(declared *corev1.ResourceRequirements, cpu, memory resource.Quantity) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	for name, fallback := range map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory} {
		var limit, request *resource.Quantity
		if declared != nil {
			if q, ok := declared.Limits[name]; ok && !q.IsZero() {
				limit = &q
			}
			if q, ok := declared.Requests[name]; ok && !q.IsZero() {
				request = &q
			}
		}
		effectiveLimit := fallback
		switch {
		case limit != nil:
			effectiveLimit = *limit
		case request != nil && request.Cmp(fallback) > 0:
			effectiveLimit = *request
		}
		effectiveRequest := effectiveLimit
		if request != nil {
			effectiveRequest = *request
		}
		out.Limits[name] = effectiveLimit.DeepCopy()
		out.Requests[name] = effectiveRequest.DeepCopy()
	}
	return out
}

// resolveKeeperStorage returns the Keeper volume size and StorageClass. An
// unset class keeps the one the deployed Keeper uses — volumes cannot move to
// another class, and Keepers created before the keeper component existed
// always used the cluster default — and otherwise follows the engine's.
func resolveKeeperStorage(c *controller.Context, keeper corev1alpha1.ComponentSpec) (resource.Quantity, *string, error) {
	size := resource.MustParse(common.KeeperDefaultStorage)
	if keeper.Storage != nil && !keeper.Storage.Size.IsZero() {
		size = keeper.Storage.Size.DeepCopy()
	}
	if keeper.Storage != nil && keeper.Storage.StorageClass != nil {
		return size, keeper.Storage.StorageClass, nil
	}

	current := &chkv1.ClickHouseKeeperInstallation{}
	if err := c.Get(current, keeperCRName(c.Name())); err == nil {
		if vct := keeperVolumeClaimTemplate(current); vct != nil {
			return size, vct.Spec.StorageClassName, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return size, nil, fmt.Errorf("read current ClickHouseKeeperInstallation: %w", err)
	}

	_, engineClass := resolveStorage(c.Instance().Spec.Components[common.ComponentEngine])
	return size, engineClass, nil
}

func keeperVolumeClaimTemplate(chk *chkv1.ClickHouseKeeperInstallation) *chiv1.VolumeClaimTemplate {
	if chk.Spec.Templates == nil {
		return nil
	}
	for i := range chk.Spec.Templates.VolumeClaimTemplates {
		if chk.Spec.Templates.VolumeClaimTemplates[i].Name == common.KeeperDataVolumeClaimTemplateName {
			return &chk.Spec.Templates.VolumeClaimTemplates[i]
		}
	}
	return nil
}

// storageReclaimPolicy returns the engine's storage reclaim policy, defaulting
// to Retain so deleting an Instance never silently destroys its data.
func storageReclaimPolicy(c *controller.Context) string {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	var params components.ClickHouseParameters
	_ = c.TryDecodeComponentParameters(engine, &params)
	if strings.EqualFold(params.StorageReclaimPolicy, common.StorageReclaimPolicyDelete) {
		return common.StorageReclaimPolicyDelete
	}
	return common.StorageReclaimPolicyRetain
}

// storageManagement sets the reclaim policy on a volume claim template. The
// operator sets no owner references on the PVCs; it deletes them on Instance
// deletion or replica removal only when the policy is Delete. The same policy
// applies to Keeper, because retained ClickHouse data whose replication
// metadata is gone from Keeper comes back read-only.
func storageManagement(c *controller.Context) chiv1.StorageManagement {
	return chiv1.StorageManagement{PVCReclaimPolicy: chiv1.PVCReclaimPolicy(storageReclaimPolicy(c))}
}

// resolveEngineSettings builds the ClickHouse server settings rendered into
// config.d. It always enables the native Prometheus endpoint and layers the
// user-supplied configuration on top (user values win on conflict).
func resolveEngineSettings(c *controller.Context, engine corev1alpha1.ComponentSpec) (*chiv1.Settings, error) {
	var params components.ClickHouseParameters
	_ = c.TryDecodeComponentParameters(engine, &params)

	settings := chiv1.NewSettings()
	if cfg := strings.TrimSpace(params.Configuration); cfg != "" {
		if err := yaml.Unmarshal([]byte(cfg), settings); err != nil {
			return nil, fmt.Errorf("parse engine configuration: %w", err)
		}
	}

	settings.SetIfNotExists("prometheus/endpoint", chiv1.NewSettingScalar(common.MetricsPath))
	settings.SetIfNotExists("prometheus/port", chiv1.NewSettingScalar(strconv.Itoa(common.MetricsPort)))
	settings.SetIfNotExists("prometheus/metrics", chiv1.NewSettingScalar("true"))
	settings.SetIfNotExists("prometheus/events", chiv1.NewSettingScalar("true"))
	settings.SetIfNotExists("prometheus/asynchronous_metrics", chiv1.NewSettingScalar("true"))
	settings.SetIfNotExists("prometheus/status_info", chiv1.NewSettingScalar("true"))

	return settings, nil
}

// credentialsForCHI returns the administrative username and the pod
// annotations that roll ClickHouse when a credential changes. Before the
// credentials Secret exists (Validate on a new Instance) the defaults apply.
func credentialsForCHI(c *controller.Context, secretName string) (string, map[string]string, error) {
	secret := &corev1.Secret{}
	found, err := getOptional(c, secret, secretName)
	if err != nil || !found {
		return common.AppUserName, nil, err
	}
	username := common.AppUserName
	if v := string(secret.Data[common.CredentialsKeyUsername]); v != "" {
		username = v
	}
	return username, map[string]string{common.AnnotationCredentialsRevision: credentialsRevision(secret)}, nil
}

// readCredentials reads the generated application user credentials. On any error
// it falls back to the app username with an empty password so Status still
// reports connection details rather than failing the reconcile.
func readCredentials(c *controller.Context) (username, password string) {
	username = common.AppUserName
	secret := &corev1.Secret{}
	if err := c.Get(secret, credentialsSecretName(c.Name())); err != nil {
		return username, ""
	}
	if v, ok := secret.Data[common.CredentialsKeyUsername]; ok && len(v) > 0 {
		username = string(v)
	}
	return username, string(secret.Data[common.CredentialsKeyPassword])
}
