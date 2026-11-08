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
	"slices"
	"strings"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	goversion "github.com/hashicorp/go-version"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

var _ controller.ReferenceValidator = (*Provider)(nil)

var (
	componentsPath = field.NewPath("spec", "components")
	enginePath     = componentsPath.Key(common.ComponentEngine)
	keeperPath     = componentsPath.Key(common.ComponentKeeper)
)

// minEngineLimits are the smallest CPU and memory limits ClickHouse runs with.
var minEngineLimits = []struct {
	name corev1.ResourceName
	min  resource.Quantity
}{
	{corev1.ResourceCPU, resource.MustParse("1")},
	{corev1.ResourceMemory, resource.MustParse("1Gi")},
}

// The values the engine accepts; the parameters match them in any case.
var (
	storageReclaimPolicies = []string{common.StorageReclaimPolicyRetain, common.StorageReclaimPolicyDelete}
	tlsModes               = []string{common.TLSModeRequired, common.TLSModeOptional}
	serviceTypes           = []corev1.ServiceType{
		corev1.ServiceTypeClusterIP, corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort,
	}
)

// Validate checks the rules about the Instance alone and, on an update, the
// changes the deployed cluster cannot take. The runtime runs it once the
// Instance matches the topologies, components, versions and parameter schemas
// the Provider declares.
func (p *Provider) Validate(in controller.ValidationInput) controller.ValidationResult {
	engine, ok := in.Instance.Spec.Components[common.ComponentEngine]
	if !ok {
		return controller.ValidationResult{Errors: field.ErrorList{field.Required(enginePath, "")}}
	}

	errs := validateEngine(in.Instance, engine)
	errs = append(errs, validateKeeper(in.Instance)...)
	errs = append(errs, validateTransitions(&in.Provider.Spec, in.Instance, in.Old)...)
	return controller.ValidationResult{Errors: errs}
}

// ValidateReferences checks the rules that read other objects: the
// administrative credentials in the Secrets, and the volumes and versions of
// the deployed cluster.
func (p *Provider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	errs, err := validateCredentials(c)
	if err != nil {
		return nil, err
	}
	engineErrs, err := validateDeployedEngine(c)
	if err != nil {
		return nil, err
	}
	keeperErrs, err := validateDeployedKeeper(c)
	if err != nil {
		return nil, err
	}
	return slices.Concat(errs, engineErrs, keeperErrs), nil
}

// validateEngine checks the engine's resources, replicas, parameters and
// service.
func validateEngine(instance *corev1alpha1.Instance, engine corev1alpha1.ComponentSpec) field.ErrorList {
	var errs field.ErrorList
	if engine.Resources != nil {
		for _, limit := range minEngineLimits {
			value, ok := engine.Resources.Limits[limit.name]
			if ok && !value.IsZero() && value.Cmp(limit.min) < 0 {
				errs = append(errs, field.Invalid(enginePath.Child("resources", "limits").Key(string(limit.name)),
					value.String(), "must be at least "+limit.min.String()))
			}
		}
	}
	errs = append(errs, validateRequestsWithinLimits(enginePath, resolveResources(engine))...)

	replicasPath := enginePath.Child("replicas")
	switch replicated := instance.GetTopologyType() == common.TopologyReplicated; {
	case engine.Replicas == nil:
	case replicated && *engine.Replicas < 2:
		errs = append(errs, field.Invalid(replicasPath, *engine.Replicas, "must be at least 2 in the replicated topology"))
	case !replicated && *engine.Replicas > 1:
		errs = append(errs, field.Invalid(replicasPath, *engine.Replicas,
			"the standalone topology runs a single replica; use the replicated topology for more"))
	}

	var params components.ClickHouseParameters
	if engine.Parameters != nil {
		// The runtime has checked them against their schema.
		_ = json.Unmarshal(engine.Parameters.Raw, &params)
	}
	paramsPath := enginePath.Child("parameters")
	if policy := params.StorageReclaimPolicy; policy != "" &&
		!slices.ContainsFunc(storageReclaimPolicies, func(p string) bool { return strings.EqualFold(p, policy) }) {
		errs = append(errs, field.NotSupported(paramsPath.Child("storageReclaimPolicy"), policy, storageReclaimPolicies))
	}
	if params.TLS != nil && params.TLS.Mode != "" &&
		!slices.ContainsFunc(tlsModes, func(m string) bool { return strings.EqualFold(m, params.TLS.Mode) }) {
		errs = append(errs, field.NotSupported(paramsPath.Child("tls", "mode"), params.TLS.Mode, tlsModes))
	}
	if err := validateConfiguration(params.Configuration); err != nil {
		errs = append(errs, field.Invalid(paramsPath.Child("configuration"), field.OmitValueType{}, err.Error()))
	}

	if engine.Service != nil && engine.Service.ServiceType != "" && !slices.Contains(serviceTypes, engine.Service.ServiceType) {
		errs = append(errs, field.NotSupported(enginePath.Child("service", "serviceType"), engine.Service.ServiceType, serviceTypes))
	}
	return errs
}

// validateRequestsWithinLimits rejects a request above its limit, which
// Kubernetes would refuse when creating the pods.
func validateRequestsWithinLimits(path *field.Path, resources corev1.ResourceRequirements) field.ErrorList {
	var errs field.ErrorList
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := resources.Requests[name], resources.Limits[name]
		if request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(path.Child("resources", "requests").Key(string(name)), request.String(),
				"must not exceed the limit "+limit.String()))
		}
	}
	return errs
}

// validateKeeper checks the keeper component, which only the replicated
// topology uses and whose ensemble size is fixed.
func validateKeeper(instance *corev1alpha1.Instance) field.ErrorList {
	keeper, ok := instance.Spec.Components[common.ComponentKeeper]
	if !ok {
		return nil
	}
	if instance.GetTopologyType() != common.TopologyReplicated {
		return field.ErrorList{field.Forbidden(keeperPath, "only the replicated topology runs Keeper")}
	}

	var errs field.ErrorList
	if keeper.Replicas != nil && *keeper.Replicas != common.KeeperReplicas {
		errs = append(errs, field.Invalid(keeperPath.Child("replicas"), *keeper.Replicas,
			fmt.Sprintf("Keeper runs %d replicas; its size cannot be changed", common.KeeperReplicas)))
	}
	return append(errs, validateRequestsWithinLimits(keeperPath, resolveKeeperResources(keeper))...)
}

// ownedSettings are server settings the operator or the provider manages; a
// user override would break ports, TLS, Keeper wiring or metrics scraping.
var (
	ownedSettings = []string{
		"http_port", "tcp_port", "https_port", "tcp_port_secure", "interserver_http_port", "interserver_https_port",
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
		return fmt.Errorf("not valid YAML: %w", err)
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
		return fmt.Errorf("must not set %s: managed by the provider", strings.Join(owned, ", "))
	}
	return nil
}

// validateTransitions rejects, on an update of an Instance whose cluster the
// provider has started creating, the changes the cluster cannot take.
func validateTransitions(spec *corev1alpha1.ProviderSpec, in, old *corev1alpha1.Instance) field.ErrorList {
	if old == nil || old.Status.Phase == "" || old.Status.Phase == corev1alpha1.InstancePhasePending {
		return nil
	}
	wasReplicated := old.GetTopologyType() == common.TopologyReplicated
	if wasReplicated && in.GetTopologyType() != common.TopologyReplicated {
		return field.ErrorList{field.Forbidden(field.NewPath("spec", "topology", "type"), fmt.Sprintf(
			"cannot change from %s to %s", common.TopologyReplicated, common.TopologyStandalone))}
	}

	// Sizes are compared with the deployed volumes, in ValidateReferences: a
	// growth blocked there is stored, and must stay revertible.
	_, class := resolveStorage(in.Spec.Components[common.ComponentEngine])
	_, oldClass := resolveStorage(old.Spec.Components[common.ComponentEngine])
	errs := validateStorageClassTransition(enginePath.Child("storage", "storageClass"), oldClass, class)
	errs = append(errs, validateVersionTransition(spec, common.ComponentEngine, in, old)...)
	if !wasReplicated {
		return errs
	}

	class, oldClass = keeperStorageClass(in), keeperStorageClass(old)
	if keeper := in.Spec.Components[common.ComponentKeeper]; keeper.Storage == nil || keeper.Storage.StorageClass == nil {
		// Sync keeps the class of the deployed Keeper.
		class = oldClass
	}
	errs = append(errs, validateStorageClassTransition(keeperPath.Child("storage", "storageClass"), oldClass, class)...)
	return append(errs, validateVersionTransition(spec, common.ComponentKeeper, in, old)...)
}

// keeperStorageClass returns the StorageClass a new Keeper gets: its own,
// else the engine's.
func keeperStorageClass(instance *corev1alpha1.Instance) *string {
	if keeper := instance.Spec.Components[common.ComponentKeeper]; keeper.Storage != nil && keeper.Storage.StorageClass != nil {
		return keeper.Storage.StorageClass
	}
	_, class := resolveStorage(instance.Spec.Components[common.ComponentEngine])
	return class
}

// validateStorageClassTransition rejects moving volumes to another
// StorageClass, which existing PVCs cannot follow.
func validateStorageClassTransition(path *field.Path, fromClass, toClass *string) field.ErrorList {
	if equalStorageClass(fromClass, toClass) {
		return nil
	}
	return field.ErrorList{field.Forbidden(path, fmt.Sprintf("cannot change from %s to %s",
		describeStorageClass(fromClass), describeStorageClass(toClass)))}
}

// validateVersionTransition rejects downgrading a component: newer releases
// may have written data or metadata older ones cannot read.
func validateVersionTransition(spec *corev1alpha1.ProviderSpec, name string, in, old *corev1alpha1.Instance) field.ErrorList {
	from, err := componentImage(spec, old, name, old.Spec.Components[name])
	if err != nil {
		return nil
	}
	to, err := componentImage(spec, in, name, in.Spec.Components[name])
	if err != nil {
		return nil
	}
	return validateDowngrade(imageSourcePath(in, name), from, to)
}

// validateDowngrade rejects a desired image whose version is below the
// current one's. Images whose tags are not versions are not compared.
func validateDowngrade(path *field.Path, current, desired string) field.ErrorList {
	from, to := imageVersion(current), imageVersion(desired)
	if from == nil || to == nil || compareSegments(to, from) >= 0 {
		return nil
	}
	return field.ErrorList{field.Forbidden(path,
		fmt.Sprintf("cannot downgrade from %s to %s", from.Original(), to.Original()))}
}

// imageSourcePath is the field the component's image is chosen by.
func imageSourcePath(instance *corev1alpha1.Instance, name string) *field.Path {
	component := instance.Spec.Components[name]
	switch {
	case component.Image != "":
		return componentsPath.Key(name).Child("image")
	case component.Version != "":
		return componentsPath.Key(name).Child("version")
	default:
		return field.NewPath("spec", "version")
	}
}

// validateCredentials checks the administrative credentials: those seeding a
// new Instance from spec.userSecretRef, and those of the credentials Secret,
// whose password the user may edit to rotate it.
func validateCredentials(c *controller.Context) (field.ErrorList, error) {
	refPath := field.NewPath("spec", "userSecretRef")
	var errs field.ErrorList
	if ref := c.Instance().Spec.UserSecretRef; ref != nil {
		src := &corev1.Secret{}
		found, err := getOptional(c, src, ref.Name)
		if err != nil {
			return nil, err
		}
		// A missing Secret is reported by the runtime.
		if found {
			if err := validateUserSecretData(src.Data); err != nil {
				errs = append(errs, field.Invalid(refPath.Child("name"), ref.Name, err.Error()))
			}
		}
	}

	current := &corev1.Secret{}
	found, err := getOptional(c, current, credentialsSecretName(c.Name()))
	if err != nil {
		return nil, err
	}
	if found {
		if err := validateUserSecretData(current.Data); err != nil {
			errs = append(errs, field.Invalid(refPath, field.OmitValueType{},
				fmt.Sprintf("the credentials Secret %s: %v", current.Name, err)))
		}
	}
	return errs, nil
}

// validateDeployedEngine checks the engine against the running
// ClickHouseInstallation: its volumes do not shrink and grow only on a
// StorageClass that allows it, and ClickHouse is not downgraded, also when a
// provider release changes the version an Instance gets by default.
func validateDeployedEngine(c *controller.Context) (field.ErrorList, error) {
	chi := &chiv1.ClickHouseInstallation{}
	found, err := getOptional(c, chi, c.Name())
	if err != nil || !found {
		return nil, err
	}
	engine := c.Instance().Spec.Components[common.ComponentEngine]

	var errs field.ErrorList
	if current := dataVolumeClaimTemplate(chi); current != nil {
		size, _ := resolveStorage(engine)
		errs, err = validateResize(c, enginePath.Child("storage", "size"), current, size,
			client.MatchingLabels{common.LabelCHIName: c.Name()})
		if err != nil {
			return nil, err
		}
	}
	// A version that does not resolve is reported by Sync.
	if image, err := resolveImage(c, common.ComponentEngine, engine); err == nil {
		errs = append(errs, validateDowngrade(imageSourcePath(c.Instance(), common.ComponentEngine), currentEngineImage(chi), image)...)
	}
	return errs, nil
}

// validateDeployedKeeper checks the keeper component against the running
// ClickHouseKeeperInstallation, which exists before ClickHouse does: its
// volumes keep their StorageClass, do not shrink and grow only on a
// StorageClass that allows it, and Keeper is not downgraded.
func validateDeployedKeeper(c *controller.Context) (field.ErrorList, error) {
	if c.Instance().GetTopologyType() != common.TopologyReplicated {
		return nil, nil
	}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	found, err := getOptional(c, chk, keeperCRName(c.Name()))
	if err != nil || !found {
		return nil, err
	}
	keeper := c.Instance().Spec.Components[common.ComponentKeeper]

	var errs field.ErrorList
	if current := keeperVolumeClaimTemplate(chk); current != nil {
		size, class, err := resolveKeeperStorage(c, keeper)
		if err != nil {
			return nil, err
		}
		// A Keeper created before the keeper component existed is on the
		// cluster default, whatever the previous Instance says.
		if !equalStorageClass(current.Spec.StorageClassName, class) {
			errs = append(errs, field.Forbidden(keeperPath.Child("storage", "storageClass"), fmt.Sprintf(
				"cannot change from %s to %s", describeStorageClass(current.Spec.StorageClassName), describeStorageClass(class))))
		}
		resizeErrs, err := validateResize(c, keeperPath.Child("storage", "size"), current, size,
			client.MatchingLabels{common.LabelCHKName: keeperCRName(c.Name())})
		if err != nil {
			return nil, err
		}
		errs = append(errs, resizeErrs...)
	}
	if image, err := resolveImage(c, common.ComponentKeeper, keeper); err == nil {
		errs = append(errs, validateDowngrade(imageSourcePath(c.Instance(), common.ComponentKeeper), currentKeeperImage(chk), image)...)
	}
	return errs, nil
}

// validateResize compares volumes with the deployed volume claim template. It
// rejects shrinking them, and growing them when their StorageClass does not
// allow volume expansion: the operator's resize of the PVCs would be refused,
// leaving the change stuck.
func validateResize(
	c *controller.Context,
	path *field.Path,
	current *chiv1.VolumeClaimTemplate,
	size resource.Quantity,
	selector client.MatchingLabels,
) (field.ErrorList, error) {
	currentSize, ok := current.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || size.Cmp(currentSize) == 0 {
		return nil, nil
	}
	if size.Cmp(currentSize) < 0 {
		return field.ErrorList{field.Forbidden(path,
			fmt.Sprintf("cannot shrink from %s to %s", currentSize.String(), size.String()))}, nil
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(pvcs, selector); err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	checked := map[string]bool{}
	for _, pvc := range pvcs.Items {
		if pvc.Spec.StorageClassName == nil || checked[*pvc.Spec.StorageClassName] {
			continue
		}
		name := *pvc.Spec.StorageClassName
		checked[name] = true
		// Cluster-scoped: read without the Instance's namespace.
		class := &storagev1.StorageClass{}
		if err := c.Client().Get(c.Context(), client.ObjectKey{Name: name}, class); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read StorageClass %s: %w", name, err)
		}
		if class.AllowVolumeExpansion == nil || !*class.AllowVolumeExpansion {
			return field.ErrorList{field.Forbidden(path, fmt.Sprintf(
				"cannot grow: the %s StorageClass does not allow volume expansion", name))}, nil
		}
	}
	return nil, nil
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
