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
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

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
			},
			// NOTE: We intentionally do NOT watch CHI/CHK here.
			// Watching them causes a tight feedback loop: operator updates
			// (finalizers, status) re-trigger Apply, which updates the object,
			// which triggers the operator again.
			// Instead, Status() polls via c.Get() on each Instance reconcile,
			// and Sync() returns WaitError while provisioning is in progress.
			WatchConfigs: []controller.WatchConfig{},
		},
	}
}

// Validate checks the Instance spec before reconciliation.
func (p *Provider) Validate(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Validating ClickHouse instance", "name", c.Name())

	engine, ok := c.Instance().Spec.Components[common.ComponentEngine]
	if !ok {
		return fmt.Errorf("engine component is required")
	}

	if engine.Resources != nil && engine.Resources.Limits != nil {
		lim := engine.Resources.Limits
		if cpu := lim.Cpu(); cpu != nil && !cpu.IsZero() {
			if cpu.Cmp(resource.MustParse("1")) < 0 {
				return fmt.Errorf("engine CPU limit must be at least 1 core")
			}
		}
		if mem := lim.Memory(); mem != nil && !mem.IsZero() {
			if mem.Cmp(resource.MustParse("1Gi")) < 0 {
				return fmt.Errorf("engine memory limit must be at least 1Gi")
			}
		}
	}

	if c.Instance().GetTopologyType() == common.TopologyReplicated {
		if engine.Replicas != nil && *engine.Replicas < 2 {
			return fmt.Errorf("replicated topology requires at least 2 engine replicas")
		}
	}

	return nil
}

// Sync creates and polls the required resources for the selected topology.
//
// Create-only semantics: once created, the Altinity operator owns the CHI/CHK
// and we must not overwrite its changes on every reconcile. WaitError is
// returned while provisioning is in progress so the runtime requeues after 15s.
func (p *Provider) Sync(c *controller.Context) error {
	l := log.FromContext(c.Context())
	topology := c.Instance().GetTopologyType()
	l.Info("Syncing ClickHouse instance", "name", c.Name(), "topology", topology)

	switch topology {
	case common.TopologyReplicated:
		return p.syncReplicated(c)
	default:
		// standalone (and any unknown topology falls through to standalone)
		return p.syncStandalone(c)
	}
}

// syncStandalone creates or waits on the CHI for a single-node deployment.
func (p *Provider) syncStandalone(c *controller.Context) error {
	l := log.FromContext(c.Context())

	existing := &chiv1.ClickHouseInstallation{}
	if err := c.Get(existing, c.Name()); err != nil {
		chi, buildErr := buildCHI(c, 1)
		if buildErr != nil {
			return fmt.Errorf("build ClickHouseInstallation: %w", buildErr)
		}
		if applyErr := c.Apply(chi); applyErr != nil {
			return fmt.Errorf("create ClickHouseInstallation: %w", applyErr)
		}
		l.Info("ClickHouseInstallation created", "name", c.Name())
		return controller.WaitForDuration("waiting for Altinity operator to provision ClickHouseInstallation", 15*time.Second)
	}

	return waitForCHI(c, existing)
}

// syncReplicated creates or waits on a CHK (Keeper) + CHI pair.
func (p *Provider) syncReplicated(c *controller.Context) error {
	l := log.FromContext(c.Context())
	keeperName := keeperCRName(c.Name())

	// 1. Ensure Keeper exists.
	existingCHK := &chkv1.ClickHouseKeeperInstallation{}
	if err := c.Get(existingCHK, keeperName); err != nil {
		chk := buildCHK(c)
		if applyErr := c.Apply(chk); applyErr != nil {
			return fmt.Errorf("create ClickHouseKeeperInstallation: %w", applyErr)
		}
		l.Info("ClickHouseKeeperInstallation created", "name", keeperName)
		return controller.WaitForDuration("waiting for Keeper to initialize", 15*time.Second)
	}

	// 2. Wait for Keeper to be ready before creating ClickHouse.
	if keeperErr := waitForCHK(c, existingCHK); keeperErr != nil {
		return keeperErr
	}

	// 3. Ensure ClickHouse exists.
	replicas := replicasCount(c)
	existingCHI := &chiv1.ClickHouseInstallation{}
	if err := c.Get(existingCHI, c.Name()); err != nil {
		chi, buildErr := buildCHI(c, replicas)
		if buildErr != nil {
			return fmt.Errorf("build ClickHouseInstallation: %w", buildErr)
		}
		if applyErr := c.Apply(chi); applyErr != nil {
			return fmt.Errorf("create ClickHouseInstallation: %w", applyErr)
		}
		l.Info("ClickHouseInstallation created (replicated)", "name", c.Name(), "replicas", replicas)
		return controller.WaitForDuration("waiting for Altinity operator to provision ClickHouseInstallation", 15*time.Second)
	}

	return waitForCHI(c, existingCHI)
}

// waitForCHI checks CHI status and returns a WaitError if not yet Completed.
func waitForCHI(c *controller.Context, chi *chiv1.ClickHouseInstallation) error {
	l := log.FromContext(c.Context())

	if chi.Status == nil {
		return controller.WaitForDuration("waiting for Altinity operator to initialize CHI", 15*time.Second)
	}
	switch chi.Status.GetStatus() {
	case chiv1.StatusCompleted:
		l.Info("ClickHouseInstallation is Completed", "name", chi.Name)
		return nil
	case chiv1.StatusAborted:
		return fmt.Errorf("ClickHouseInstallation aborted: %s", chi.Status.GetError())
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
		l.Info("ClickHouseKeeperInstallation is Completed", "name", chk.Name)
		return nil
	case chkv1.StatusAborted:
		return fmt.Errorf("ClickHouseKeeperInstallation aborted: %s", chk.Status.GetError())
	default:
		l.Info("Keeper still provisioning", "name", chk.Name, "status", chk.Status.GetStatus())
		return controller.WaitForDuration("waiting for Keeper to complete provisioning", 15*time.Second)
	}
}

// Status reports the current status of the ClickHouse instance.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	topology := c.Instance().GetTopologyType()

	if topology == common.TopologyReplicated {
		// For replicated, check Keeper first, then CHI.
		chk := &chkv1.ClickHouseKeeperInstallation{}
		if err := c.Get(chk, keeperCRName(c.Name())); err != nil {
			return controller.Provisioning("Waiting for ClickHouseKeeperInstallation"), nil
		}
		if chk.Status == nil || chk.Status.GetStatus() != chkv1.StatusCompleted {
			return controller.Provisioning("Waiting for Keeper to become ready"), nil
		}
	}

	chi := &chiv1.ClickHouseInstallation{}
	if err := c.Get(chi, c.Name()); err != nil {
		return controller.Provisioning("Waiting for ClickHouseInstallation"), nil
	}
	if chi.Status == nil {
		return controller.Provisioning("Waiting for operator to initialize"), nil
	}

	switch chi.Status.GetStatus() {
	case chiv1.StatusCompleted:
		return controller.ReadyWithConnectionDetails(buildConnectionDetails(c, chi)), nil
	case chiv1.StatusAborted:
		errMsg := chi.Status.GetError()
		if errMsg == "" {
			errMsg = "ClickHouseInstallation aborted"
		}
		return controller.Failed(errMsg), nil
	default:
		return controller.Provisioning(fmt.Sprintf("Cluster is being created (%s)", chi.Status.GetStatus())), nil
	}
}

// Cleanup removes the CHI (and CHK for replicated) when the Instance is deleted.
func (p *Provider) Cleanup(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up ClickHouse instance", "name", c.Name())

	chi := &chiv1.ClickHouseInstallation{ObjectMeta: c.ObjectMeta(c.Name())}
	if err := c.Delete(chi); err != nil {
		return fmt.Errorf("delete ClickHouseInstallation: %w", err)
	}

	if c.Instance().GetTopologyType() == common.TopologyReplicated {
		chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: c.ObjectMeta(keeperCRName(c.Name()))}
		if err := c.Delete(chk); err != nil {
			return fmt.Errorf("delete ClickHouseKeeperInstallation: %w", err)
		}
	}

	l.Info("ClickHouse instance cleaned up", "name", c.Name())
	return nil
}

// =============================================================================
// Builders
// =============================================================================

// buildCHI constructs a ClickHouseInstallation for the given replica count.
func buildCHI(c *controller.Context, replicasCount int) (*chiv1.ClickHouseInstallation, error) {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	image, err := resolveImage(c, engine)
	if err != nil {
		return nil, err
	}
	cpu, memory := resolveResources(engine)
	storageSize, storageClass := resolveStorage(engine)

	container := corev1.Container{
		Name:  "clickhouse",
		Image: image,
		Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
			Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
		},
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

	spec := chiv1.ChiSpec{
		Configuration: &chiv1.Configuration{
			Clusters: []*chiv1.Cluster{cluster},
		},
		Templates: &chiv1.Templates{
			PodTemplates:         []chiv1.PodTemplate{{Name: common.PodTemplateName, Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}},
			VolumeClaimTemplates: []chiv1.VolumeClaimTemplate{{Name: common.DataVolumeClaimTemplateName, Spec: pvcSpec}},
		},
	}

	// Wire Keeper for replicated topology using explicit node listing.
	// The Altinity operator creates per-replica headless services following:
	//   chk-<keeper-name>-<cluster>-0-<replica-index>.<namespace>.svc
	// We enumerate them for the ZooKeeper config so older operator versions
	// (which may not support the keeper: reference field) work correctly.
	if replicasCount > 1 {
		nodes := keeperZookeeperNodes(keeperCRName(c.Name()), c.Namespace(), common.KeeperReplicas)
		spec.Configuration.Zookeeper = &chiv1.ZookeeperConfig{
			Nodes:              nodes,
			SessionTimeoutMs:   30000,
			OperationTimeoutMs: 10000,
		}
	}

	return &chiv1.ClickHouseInstallation{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       spec,
	}, nil
}

// buildCHK constructs a ClickHouseKeeperInstallation with 3 replicas (Raft quorum).
func buildCHK(c *controller.Context) *chkv1.ClickHouseKeeperInstallation {
	// Keeper nodes are small — fixed resources, not user-configurable.
	keeperCPU := resource.MustParse("500m")
	keeperMem := resource.MustParse("1Gi")
	keeperStorage := resource.MustParse("10Gi")

	container := corev1.Container{
		Name:  "clickhouse-keeper",
		Image: "clickhouse/clickhouse-keeper:25.3.5",
		Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceCPU: keeperCPU, corev1.ResourceMemory: keeperMem},
			Requests: corev1.ResourceList{corev1.ResourceCPU: keeperCPU, corev1.ResourceMemory: keeperMem},
		},
	}

	return &chkv1.ClickHouseKeeperInstallation{
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
						Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
					},
				},
				VolumeClaimTemplates: []chiv1.VolumeClaimTemplate{
					{
						Name: common.KeeperDataVolumeClaimTemplateName,
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceStorage: keeperStorage},
							},
						},
					},
				},
			},
		},
	}
}

// =============================================================================
// Helpers
// =============================================================================

// keeperCRName returns the CHK resource name for a given instance.
func keeperCRName(instanceName string) string {
	return instanceName + "-keeper"
}

// keeperZookeeperNodes builds the explicit ZooKeeper node list for Keeper.
// Altinity creates per-replica headless services:
//
//	chk-<keeper-name>-<cluster>-0-<replica>.<namespace>.svc
//
// Port 2181 is the standard ZooKeeper/Keeper client port.
func keeperZookeeperNodes(keeperName, namespace string, replicas int) []chiv1.ZookeeperNode {
	nodes := make([]chiv1.ZookeeperNode, replicas)
	for i := 0; i < replicas; i++ {
		// Service name pattern: chk-<keeper-name>-<cluster>-<shard>-<replica>
		svc := fmt.Sprintf("chk-%s-%s-0-%d.%s.svc", keeperName, common.CHKClusterName, i, namespace)
		nodes[i] = chiv1.NewZookeeperNode(svc, 2181)
	}
	return nodes
}

// replicasCount returns the configured replica count or the default.
func replicasCount(c *controller.Context) int {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	if engine.Replicas != nil && *engine.Replicas >= 2 {
		return int(*engine.Replicas)
	}
	return common.DefaultReplicasCount
}

// resolveImage returns the container image for the engine component.
func resolveImage(c *controller.Context, engine corev1alpha1.ComponentSpec) (string, error) {
	if engine.Image != "" {
		return engine.Image, nil
	}
	spec, err := c.ProviderSpec()
	if err != nil {
		return "", fmt.Errorf("get provider spec: %w", err)
	}
	if engine.Version != "" {
		if img := controller.GetImageForVersion(spec, common.ComponentEngine, engine.Version); img != "" {
			return img, nil
		}
	}
	if img := controller.GetDefaultImageForComponent(spec, common.ComponentEngine); img != "" {
		return img, nil
	}
	return "", fmt.Errorf("no image found for engine component")
}

// resolveResources returns CPU and memory quantities with defaults applied.
func resolveResources(engine corev1alpha1.ComponentSpec) (cpu, memory resource.Quantity) {
	cpu = resource.MustParse("1")
	memory = resource.MustParse("4Gi")
	if engine.Resources == nil || engine.Resources.Limits == nil {
		return
	}
	if v := engine.Resources.Limits.Cpu(); v != nil && !v.IsZero() {
		cpu = v.DeepCopy()
	}
	if v := engine.Resources.Limits.Memory(); v != nil && !v.IsZero() {
		memory = v.DeepCopy()
	}
	return
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

// buildConnectionDetails extracts connection info from a ready CHI.
func buildConnectionDetails(c *controller.Context, chi *chiv1.ClickHouseInstallation) controller.ConnectionDetails {
	svcName := fmt.Sprintf("clickhouse-%s", c.Name())
	host := chi.Status.GetEndpoint()
	if host == "" {
		host = fmt.Sprintf("%s.%s.svc", svcName, c.Namespace())
	}
	return controller.ConnectionDetails{
		Type:     "clickhouse",
		Provider: common.ProviderName,
		Host:     host,
		Port:     "8123",
		URI:      fmt.Sprintf("http://default:@%s:8123/", host),
	}
}
