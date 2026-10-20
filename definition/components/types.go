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

// Package components contains custom spec types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
// Add fields when a component type needs custom configuration beyond
// what the base Instance spec provides.
//
// +k8s:openapi-gen=true
package components

// ClickHouseParameters defines custom parameters for the ClickHouse engine component.
// This struct is converted to an OpenAPI schema and served via the /schema endpoint.
// Provider users specify these fields in the Instance's engine component parameters.
type ClickHouseParameters struct {
	// Configuration holds custom ClickHouse server settings as YAML.
	// Each entry maps to a server-level setting rendered into config.d,
	// e.g. `max_concurrent_queries: 200` or `logger/level: information`.
	Configuration string `json:"configuration,omitempty"`

	// PodMonitor gates creation of a Prometheus PodMonitor for the ClickHouse
	// pods. The native metrics endpoint is always exposed; this only controls
	// whether the operator manages a PodMonitor. One of "enabled" or "disabled".
	PodMonitor string `json:"podMonitor,omitempty"`

	// TLS configures encrypted connections. When enabled, the provider uses
	// cert-manager to issue a server certificate, serves HTTPS (8443) and the
	// secure native protocol (9440), and runs distributed queries between
	// replicas over TLS. See TLSParameters.Mode for the plaintext ports.
	TLS *TLSParameters `json:"tls,omitempty"`

	// StorageReclaimPolicy controls what happens to the data volumes (ClickHouse
	// and, in the replicated topology, Keeper) when the Instance is deleted or a
	// replica is removed. One of "Retain" (default) or "Delete". Retained volumes
	// are re-attached when an Instance with the same name is created in the same
	// namespace.
	StorageReclaimPolicy string `json:"storageReclaimPolicy,omitempty"`
}

// TLSParameters configures TLS for client connections to ClickHouse.
type TLSParameters struct {
	// Enabled turns on TLS. One of "enabled" or "disabled" (default). Requires
	// cert-manager to be installed in the cluster.
	Enabled string `json:"enabled,omitempty"`

	// Mode is "required" (default) or "optional". Required exposes only the
	// secure ports to clients and restricts the plaintext ports to the
	// operator and the Instance's own pods with a NetworkPolicy. Optional
	// keeps the plaintext ports open alongside the secure ones.
	Mode string `json:"mode,omitempty"`

	// IssuerRef optionally references an existing cert-manager Issuer or
	// ClusterIssuer to sign the server certificate. When omitted, the provider
	// creates a self-signed CA chain scoped to this Instance.
	IssuerRef *IssuerRef `json:"issuerRef,omitempty"`
}

// IssuerRef references a cert-manager issuer used to sign the server certificate.
type IssuerRef struct {
	// Name is the name of the Issuer or ClusterIssuer.
	Name string `json:"name"`

	// Kind is the issuer kind, either "Issuer" or "ClusterIssuer". Defaults to "Issuer".
	Kind string `json:"kind,omitempty"`

	// Group is the API group of the issuer. Defaults to "cert-manager.io".
	Group string `json:"group,omitempty"`
}
