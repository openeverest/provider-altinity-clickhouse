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
	"strings"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	goversion "github.com/hashicorp/go-version"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

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

	var params components.ClickHouseParameters
	_ = c.TryDecodeComponentParameters(engine, &params)
	switch {
	case params.StorageReclaimPolicy == "",
		strings.EqualFold(params.StorageReclaimPolicy, common.StorageReclaimPolicyRetain),
		strings.EqualFold(params.StorageReclaimPolicy, common.StorageReclaimPolicyDelete):
	default:
		return fmt.Errorf("engine storageReclaimPolicy must be one of Retain or Delete")
	}

	if params.TLS != nil {
		switch {
		case params.TLS.Mode == "",
			strings.EqualFold(params.TLS.Mode, common.TLSModeRequired),
			strings.EqualFold(params.TLS.Mode, common.TLSModeOptional):
		default:
			return fmt.Errorf("engine tls.mode must be one of required or optional")
		}
	}

	if c.Instance().GetTopologyType() != common.TopologyReplicated &&
		engine.Replicas != nil && *engine.Replicas > 1 {
		return fmt.Errorf("the %s topology runs a single replica; use the %s topology for more",
			common.TopologyStandalone, common.TopologyReplicated)
	}

	if err := validateConfiguration(params.Configuration); err != nil {
		return err
	}

	if err := validateKeeper(c); err != nil {
		return err
	}

	if err := validateCredentials(c); err != nil {
		return err
	}

	if engine.Service != nil {
		switch engine.Service.ServiceType {
		case "",
			corev1.ServiceTypeClusterIP,
			corev1.ServiceTypeLoadBalancer,
			corev1.ServiceTypeNodePort:
		default:
			return fmt.Errorf("engine service.serviceType must be one of ClusterIP, LoadBalancer or NodePort")
		}
	}

	if err := validateKeeperTransitions(c); err != nil {
		return err
	}
	return validateTransitions(c, engine)
}

// validateCredentials checks the administrative credentials: those seeding a
// new Instance from spec.userSecretRef, and those of the credentials Secret,
// whose password the user may edit to rotate it.
func validateCredentials(c *controller.Context) error {
	if ref := c.Instance().Spec.UserSecretRef; ref != nil {
		src := &corev1.Secret{}
		found, err := getOptional(c, src, ref.Name)
		if err != nil {
			return err
		}
		// A missing Secret is reported by the runtime.
		if found {
			if err := validateUserSecretData(src.Data); err != nil {
				return fmt.Errorf("spec.userSecretRef secret %s: %w", ref.Name, err)
			}
		}
	}
	current := &corev1.Secret{}
	found, err := getOptional(c, current, credentialsSecretName(c.Name()))
	if err != nil || !found {
		return err
	}
	if err := validateUserSecretData(current.Data); err != nil {
		return fmt.Errorf("credentials secret %s: %w", current.Name, err)
	}
	return nil
}

// validateKeeper checks the keeper component, which only the replicated
// topology uses and whose ensemble size is fixed.
func validateKeeper(c *controller.Context) error {
	keeper, ok := c.Instance().Spec.Components[common.ComponentKeeper]
	if !ok {
		return nil
	}
	if c.Instance().GetTopologyType() != common.TopologyReplicated {
		return fmt.Errorf("the %s component is only used by the %s topology",
			common.ComponentKeeper, common.TopologyReplicated)
	}
	if keeper.Replicas != nil && *keeper.Replicas != common.KeeperReplicas {
		return fmt.Errorf("keeper runs %d replicas; its size cannot be changed", common.KeeperReplicas)
	}
	return nil
}

// validateKeeperTransitions rejects Keeper changes the deployed Keeper cannot
// take: shrinking or moving its volumes, or downgrading it. The Keeper exists
// before ClickHouse does, so it is checked on its own.
func validateKeeperTransitions(c *controller.Context) error {
	if c.Instance().GetTopologyType() != common.TopologyReplicated {
		return nil
	}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	if err := c.Get(chk, keeperCRName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read current ClickHouseKeeperInstallation: %w", err)
	}
	keeper := c.Instance().Spec.Components[common.ComponentKeeper]

	if current := keeperVolumeClaimTemplate(chk); current != nil {
		size, storageClass, err := resolveKeeperStorage(c, keeper)
		if err != nil {
			return err
		}
		if currentSize, ok := current.Spec.Resources.Requests[corev1.ResourceStorage]; ok && size.Cmp(currentSize) < 0 {
			return fmt.Errorf("keeper storage cannot shrink from %s to %s", currentSize.String(), size.String())
		}
		if !equalStorageClass(current.Spec.StorageClassName, storageClass) {
			return fmt.Errorf("keeper storageClass cannot change from %s to %s",
				describeStorageClass(current.Spec.StorageClassName), describeStorageClass(storageClass))
		}
	}

	current := imageVersion(currentKeeperImage(chk))
	if current == nil {
		return nil
	}
	image, err := resolveImage(c, common.ComponentKeeper, keeper)
	if err != nil {
		log.FromContext(c.Context()).Info("Skipping keeper version downgrade check", "reason", err.Error())
		return nil
	}
	if desired := imageVersion(image); desired != nil && compareSegments(desired, current) < 0 {
		return fmt.Errorf("keeper cannot be downgraded from %s to %s", current.Original(), desired.Original())
	}
	return nil
}

func currentKeeperImage(chk *chkv1.ClickHouseKeeperInstallation) string {
	if chk.Spec.Templates == nil {
		return ""
	}
	for _, tmpl := range chk.Spec.Templates.PodTemplates {
		if tmpl.Name != common.KeeperPodTemplateName {
			continue
		}
		for _, container := range tmpl.Spec.Containers {
			if container.Name == "clickhouse-keeper" {
				return container.Image
			}
		}
	}
	return ""
}

// ownedSettings are server settings the operator or the provider manages; a
// user override would break ports, TLS, Keeper wiring or metrics scraping.
var (
	ownedSettings = []string{
		"http_port", "tcp_port", "https_port", "tcp_port_secure", "interserver_http_port",
		"prometheus/port", "prometheus/endpoint",
	}
	ownedSettingPrefixes = []string{"openSSL/", "zookeeper/", "remote_servers/", "macros/", "interserver_http_credentials/"}
)

// validateConfiguration rejects engine configuration that does not parse or
// that overrides a setting the operator or the provider manages.
func validateConfiguration(configuration string) error {
	cfg := strings.TrimSpace(configuration)
	if cfg == "" {
		return nil
	}
	settings := chiv1.NewSettings()
	if err := yaml.Unmarshal([]byte(cfg), settings); err != nil {
		return fmt.Errorf("engine configuration is not valid YAML: %w", err)
	}
	var owned []string
	settings.WalkNames(func(name string) {
		for _, o := range ownedSettings {
			if name == o {
				owned = append(owned, name)
				return
			}
		}
		for _, prefix := range ownedSettingPrefixes {
			if strings.HasPrefix(name, prefix) || name == strings.TrimSuffix(prefix, "/") {
				owned = append(owned, name)
				return
			}
		}
	})
	if len(owned) > 0 {
		return fmt.Errorf("engine configuration must not set %s: managed by the provider",
			strings.Join(owned, ", "))
	}
	return nil
}

// validateTransitions rejects changes the deployed cluster cannot take. The
// runtime hands Validate only the desired Instance, so the current state is
// read from the live ClickHouseInstallation and ClickHouseKeeperInstallation;
// before they exist there is nothing to compare against.
func validateTransitions(c *controller.Context, engine corev1alpha1.ComponentSpec) error {
	chi := &chiv1.ClickHouseInstallation{}
	if err := c.Get(chi, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read current ClickHouseInstallation: %w", err)
	}

	if c.Instance().GetTopologyType() != common.TopologyReplicated {
		replicated, err := isReplicated(c, chi)
		if err != nil {
			return err
		}
		if replicated {
			return fmt.Errorf("the topology cannot be changed from %s to %s",
				common.TopologyReplicated, common.TopologyStandalone)
		}
	}

	if err := validateStorageTransition(chi, engine); err != nil {
		return err
	}
	return validateVersionTransition(c, chi, engine)
}

// isReplicated reports whether the deployed cluster runs the replicated
// topology: ClickHouse is wired to Keeper, or a Keeper exists for it.
func isReplicated(c *controller.Context, chi *chiv1.ClickHouseInstallation) (bool, error) {
	if chi.Spec.Configuration != nil && chi.Spec.Configuration.Zookeeper != nil {
		return true, nil
	}
	exists, err := c.Exists(&chkv1.ClickHouseKeeperInstallation{}, keeperCRName(c.Name()))
	if err != nil {
		return false, fmt.Errorf("read current ClickHouseKeeperInstallation: %w", err)
	}
	return exists, nil
}

// validateStorageTransition rejects shrinking the data volumes or moving them
// to another StorageClass: neither can be applied to existing PVCs.
func validateStorageTransition(chi *chiv1.ClickHouseInstallation, engine corev1alpha1.ComponentSpec) error {
	current := dataVolumeClaimTemplate(chi)
	if current == nil {
		return nil
	}
	size, storageClass := resolveStorage(engine)
	if currentSize, ok := current.Spec.Resources.Requests[corev1.ResourceStorage]; ok && size.Cmp(currentSize) < 0 {
		return fmt.Errorf("engine storage cannot shrink from %s to %s", currentSize.String(), size.String())
	}
	if !equalStorageClass(current.Spec.StorageClassName, storageClass) {
		return fmt.Errorf("engine storageClass cannot change from %s to %s",
			describeStorageClass(current.Spec.StorageClassName), describeStorageClass(storageClass))
	}
	return nil
}

func dataVolumeClaimTemplate(chi *chiv1.ClickHouseInstallation) *chiv1.VolumeClaimTemplate {
	if chi.Spec.Templates == nil {
		return nil
	}
	for i := range chi.Spec.Templates.VolumeClaimTemplates {
		if chi.Spec.Templates.VolumeClaimTemplates[i].Name == common.DataVolumeClaimTemplateName {
			return &chi.Spec.Templates.VolumeClaimTemplates[i]
		}
	}
	return nil
}

func equalStorageClass(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func describeStorageClass(sc *string) string {
	if sc == nil {
		return "the cluster default"
	}
	return fmt.Sprintf("%q", *sc)
}

// validateVersionTransition rejects downgrading ClickHouse below the version
// that is running: newer releases may have written data or metadata older
// ones cannot read. Images whose tags are not versions are not compared.
func validateVersionTransition(c *controller.Context, chi *chiv1.ClickHouseInstallation, engine corev1alpha1.ComponentSpec) error {
	current := imageVersion(currentEngineImage(chi))
	if current == nil {
		return nil
	}
	image, err := resolveImage(c, common.ComponentEngine, engine)
	if err != nil {
		// Version resolution problems surface from Sync with full context.
		log.FromContext(c.Context()).Info("Skipping version downgrade check", "reason", err.Error())
		return nil
	}
	desired := imageVersion(image)
	if desired != nil && compareSegments(desired, current) < 0 {
		return fmt.Errorf("ClickHouse cannot be downgraded from %s to %s", current.Original(), desired.Original())
	}
	return nil
}

func currentEngineImage(chi *chiv1.ClickHouseInstallation) string {
	if chi.Spec.Templates == nil {
		return ""
	}
	for _, tmpl := range chi.Spec.Templates.PodTemplates {
		if tmpl.Name != common.PodTemplateName {
			continue
		}
		for _, container := range tmpl.Spec.Containers {
			if container.Name == "clickhouse" {
				return container.Image
			}
		}
	}
	return ""
}

// imageVersion parses the version in an image tag such as
// clickhouse/clickhouse-server:25.3.5 or :25.3.5.42-alpine; nil when the tag is
// not a version (e.g. latest) or the image has no tag.
func imageVersion(image string) *goversion.Version {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return nil
	}
	v, err := goversion.NewVersion(ref[i+1:])
	if err != nil {
		return nil
	}
	return v
}

// compareSegments compares the release segments both versions have, ignoring
// pre-release and build suffixes such as -alpine: 25.3.5 and 25.3.5.42 name
// the same release at different precision.
func compareSegments(a, b *goversion.Version) int {
	as, bs := a.Segments64(), b.Segments64()
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			if as[i] < bs[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
