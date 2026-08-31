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
}
