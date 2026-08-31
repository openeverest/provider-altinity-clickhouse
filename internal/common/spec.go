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

	// PodMonitorEnabled is the parameter value that turns PodMonitor creation on.
	PodMonitorEnabled = "enabled"

	// PodMonitorDisabled is the parameter value (default) that keeps PodMonitor off.
	PodMonitorDisabled = "disabled"

	// AppUserName is the application user provisioned by the provider. The
	// Altinity operator does not create a usable external user, so we create one.
	AppUserName = "admin"

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
