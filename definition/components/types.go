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
}
