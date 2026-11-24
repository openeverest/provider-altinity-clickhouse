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

// Package definition contains shared types used across the provider definition.
//
// +k8s:openapi-gen=true
package definition

// Parameters defines instance-wide structured parameters that apply to the
// entire cluster (Instance.spec.parameters).
type Parameters struct {
	// TLS configures encryption in transit: between clients and ClickHouse,
	// between the replicas, and between ClickHouse and Keeper.
	// +optional
	TLS *TLSParameters `json:"tls,omitempty"`
}

// TLSMode controls whether TLS is on and which client connections must use it.
// +enum
type TLSMode string

const (
	// TLSModeDisabled keeps TLS off. The default. An instance running with
	// TLS cannot go back to it.
	TLSModeDisabled TLSMode = "disabled"
	// TLSModePrefer serves the secure ports (HTTPS 8443, native 9440)
	// alongside the plaintext ones, so clients choose.
	TLSModePrefer TLSMode = "preferTLS"
	// TLSModeRequire serves clients over the secure ports only, and limits
	// the plaintext ports to the operator and the instance's own pods with a
	// NetworkPolicy.
	TLSModeRequire TLSMode = "requireTLS"
)

// TLSParameters configures TLS for the instance. With TLS on, distributed
// queries and replication between the replicas, and the connections to and
// between the Keeper nodes, use it too. The server certificate comes from, in
// order of precedence: certificateSecretName, the cert-manager issuer, or
// else a CA the provider creates for the instance with cert-manager.
// The certificate source cannot change while TLS is on; the certificates
// themselves can be renewed at any time, without a restart.
type TLSParameters struct {
	// Mode is disabled, preferTLS or requireTLS.
	// +default="disabled"
	// +optional
	Mode TLSMode `json:"mode,omitempty"`

	// CertificateSecretName names a Secret in the instance namespace with the
	// server certificate, followed by any intermediate CA certificates
	// (tls.crt), its private key (tls.key) and the CA certificate it is
	// verified with (ca.crt). ClickHouse and Keeper both present it, to
	// clients and to each other, so it must be valid for the names of the
	// instance's Services and pods as well as for the names clients connect
	// to. Does not need cert-manager. Cannot be combined with issuer or
	// additionalNames.
	// +optional
	CertificateSecretName string `json:"certificateSecretName,omitempty"`

	// Issuer is the cert-manager issuer that signs the server certificate,
	// instead of the CA the provider creates for the instance. The provider
	// requests it as the Certificate <instance>-server and cert-manager
	// renews it.
	// +optional
	Issuer *IssuerReference `json:"issuer,omitempty"`

	// AdditionalNames are DNS names the server certificate is valid for
	// besides those of the instance's Services and its LoadBalancer address,
	// such as an external name clients connect to. Used with issuer or the
	// provider's CA; cannot be combined with certificateSecretName, whose
	// certificate carries its own names. Changing them reissues the
	// certificate without a restart.
	// +optional
	AdditionalNames []string `json:"additionalNames,omitempty"`
}

// IssuerKind is the kind of a cert-manager issuer.
// +enum
type IssuerKind string

const (
	// IssuerKindIssuer is an Issuer in the instance namespace.
	IssuerKindIssuer IssuerKind = "Issuer"
	// IssuerKindClusterIssuer is a cluster-wide ClusterIssuer.
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerReference refers to a cert-manager issuer.
type IssuerReference struct {
	// Name of the issuer.
	Name string `json:"name"`

	// Kind of the issuer: Issuer or ClusterIssuer, or the kind of an
	// external issuer of group.
	// +default="Issuer"
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`

	// Group is the API group of an external issuer, such as
	// awspca.cert-manager.io.
	// +default="cert-manager.io"
	// +optional
	Group string `json:"group,omitempty"`
}
