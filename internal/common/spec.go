// Package common defines shared constants used across the provider.
package common

const (
	// ProviderName is the canonical name of this provider.
	// Must match the Provider CR name registered in OpenEverest.
	ProviderName = "altinity-clickhouse"

	// ComponentEngine is the logical name of the ClickHouse engine component.
	ComponentEngine = "engine"

	// ComponentTypeClickHouse is the component type name, matching versions.yaml.
	ComponentTypeClickHouse = "clickhouse"

	// TopologyStandalone is the single-node topology name.
	TopologyStandalone = "standalone"

	// TopologyReplicated is the replicated topology name (requires ClickHouse Keeper).
	TopologyReplicated = "replicated"

	// CHIClusterName is the cluster name used inside the ClickHouseInstallation CR.
	// Altinity uses this as part of the pod and service naming scheme.
	CHIClusterName = "clickhouse"

	// CHKClusterName is the cluster name used inside the ClickHouseKeeperInstallation CR.
	CHKClusterName = "keeper"

	// KeeperReplicas is the number of Keeper nodes — must be odd for Raft quorum.
	KeeperReplicas = 3

	// DefaultReplicasCount is the default number of ClickHouse replicas for the replicated topology.
	DefaultReplicasCount = 2

	// PodTemplateName is the name of the pod template defined in the CHI spec.
	PodTemplateName = "default"

	// KeeperPodTemplateName is the name of the pod template for Keeper nodes.
	KeeperPodTemplateName = "keeper-default"

	// DataVolumeClaimTemplateName is the name of the volume claim template for data storage.
	DataVolumeClaimTemplateName = "data"

	// ServiceTemplateName is the name of the service template used to expose the
	// root ClickHouse service (the one clients connect to).
	ServiceTemplateName = "svc-template"

	// KeeperDataVolumeClaimTemplateName is the name of the volume claim template for Keeper storage.
	KeeperDataVolumeClaimTemplateName = "keeper-data"
)
