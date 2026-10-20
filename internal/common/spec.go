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

	// ComponentKeeper is the logical name of the ClickHouse Keeper component
	// (replicated topology only).
	ComponentKeeper = "keeper"

	// KeeperDefaultCPU, KeeperDefaultMemory and KeeperDefaultStorage size each
	// Keeper node when the keeper component does not say otherwise.
	KeeperDefaultCPU     = "500m"
	KeeperDefaultMemory  = "1Gi"
	KeeperDefaultStorage = "10Gi"

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
	// Fixed: changing a Raft ensemble's size is not a rolling operation.
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

	// MetricsPortName is the container/Service port name for the native
	// ClickHouse Prometheus endpoint.
	MetricsPortName = "metrics"

	// MetricsPort is the port the ClickHouse Prometheus endpoint listens on.
	MetricsPort = 9363

	// MetricsPath is the HTTP path of the ClickHouse Prometheus endpoint.
	MetricsPath = "/metrics"

	// LabelCHIName is the label the Altinity operator sets on ClickHouse pods,
	// keyed by CHI (Instance) name. Used to select pods for the PodMonitor.
	LabelCHIName = "clickhouse.altinity.com/chi"

	// LabelCHKName is the label the Altinity operator sets on Keeper pods, keyed
	// by CHK name. Used to spread the Keeper pods of an Instance.
	LabelCHKName = "clickhouse-keeper.altinity.com/chk"

	// LabelCHIReclaimPolicy and LabelCHKReclaimPolicy are the PVC labels the
	// Altinity operator reads a volume's reclaim policy from when deciding
	// whether to drop a replica or purge the PVC.
	LabelCHIReclaimPolicy = "clickhouse.altinity.com/reclaimPolicy"
	LabelCHKReclaimPolicy = "clickhouse-keeper.altinity.com/reclaimPolicy"

	// StorageReclaimPolicyRetain keeps the data volumes when the Instance is
	// deleted or a replica is removed (default).
	StorageReclaimPolicyRetain = "Retain"

	// StorageReclaimPolicyDelete removes the data volumes together with the
	// Instance or the removed replica.
	StorageReclaimPolicyDelete = "Delete"

	// AntiAffinityHostnameWeight and AntiAffinityZoneWeight weigh the default
	// preferred pod anti-affinity terms that spread replicas across nodes and,
	// where nodes carry zone labels, across zones.
	AntiAffinityHostnameWeight = 100
	AntiAffinityZoneWeight     = 50

	// PodMonitorEnabled is the parameter value that turns PodMonitor creation on.
	PodMonitorEnabled = "enabled"

	// PodMonitorDisabled is the parameter value (default) that keeps PodMonitor off.
	PodMonitorDisabled = "disabled"

	// TLSEnabled is the parameter value that turns on TLS.
	TLSEnabled = "enabled"

	// TLSDisabled is the parameter value (default) that keeps TLS off.
	TLSDisabled = "disabled"

	// AppUserName is the application user provisioned by the provider. The
	// Altinity operator does not create a usable external user, so we create one.
	AppUserName = "admin"

	// RolloutStateSuffix is appended to the Instance name to form the Secret
	// recording what the provider last applied (see rolloutGate).
	RolloutStateSuffix = "-provider-state"

	// CredentialsSecretSuffix is appended to the Instance name to form the Secret
	// holding the generated application user credentials.
	CredentialsSecretSuffix = "-credentials"

	// CredentialsKeyUsername is the Secret data key holding the application username.
	CredentialsKeyUsername = "username"

	// CredentialsKeyPassword is the Secret data key holding the plaintext password.
	CredentialsKeyPassword = "password"

	// CredentialsKeyPasswordSHA256 is the Secret data key holding the SHA256 hex
	// digest of the password, referenced into the CHI users configuration so the
	// plaintext never appears in the ClickHouse config.
	CredentialsKeyPasswordSHA256 = "password_sha256_hex"

	// AppUserPasswordBytes is the entropy (in bytes) of the generated password.
	AppUserPasswordBytes = 24

	// CredentialsKeyDefaultPassword and CredentialsKeyDefaultPasswordSHA256
	// hold the generated password of ClickHouse's built-in "default" user.
	CredentialsKeyDefaultPassword       = "default_password"
	CredentialsKeyDefaultPasswordSHA256 = "default_password_sha256_hex"

	// CredentialsKeyInterserverPassword holds the generated password replicas
	// authenticate replication fetches with (interserver_http_credentials).
	CredentialsKeyInterserverPassword = "interserver_password"

	// CredentialsKeyClusterSecret holds the generated shared secret replicas
	// authenticate Distributed and ON CLUSTER queries with.
	CredentialsKeyClusterSecret = "cluster_secret"

	// InterserverUser is the user replicas present on replication fetches.
	InterserverUser = "interserver"

	// MinPasswordLength is the minimum length of a user-supplied password.
	MinPasswordLength = 12

	// AnnotationCredentialsDigest on the ClickHouse pod template changes with
	// any credential, rolling the pods: the operator passes passwords as
	// environment variables, read only at start-up.
	AnnotationCredentialsDigest = "openeverest.io/credentials-digest"

	// HTTPPort is the plaintext ClickHouse HTTP port clients connect to.
	HTTPPort = 8123

	// HTTPSPort is the ClickHouse HTTPS port exposed when TLS is enabled.
	HTTPSPort = 8443

	// TCPSecurePort is the ClickHouse native secure protocol port exposed when TLS is enabled.
	TCPSecurePort = 9440

	// SelfSignedIssuerSuffix is appended to the Instance name to form the
	// provider-managed self-signed cert-manager Issuer.
	SelfSignedIssuerSuffix = "-selfsign"

	// CAIssuerSuffix is appended to the Instance name to form both the CA
	// Certificate and the CA cert-manager Issuer.
	CAIssuerSuffix = "-ca"

	// CACertSecretSuffix is appended to the Instance name to form the Secret
	// holding the provider-managed CA key pair.
	CACertSecretSuffix = "-ca-tls"

	// ServerCertSuffix is appended to the Instance name to form the leaf server
	// Certificate.
	ServerCertSuffix = "-server"

	// ServerCertSecretSuffix is appended to the Instance name to form the Secret
	// holding the issued server certificate, mounted into the ClickHouse pods.
	ServerCertSecretSuffix = "-server-tls"
)
