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

// package main implements the provider-altinity-clickhouse provider.
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openeverest/openeverest/v2/provider-runtime/reconciler"

	"github.com/openeverest/provider-altinity-clickhouse/internal/provider"
)

// main is the entry point for the provider.
func main() {
	l := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	p := provider.New()

	r, err := reconciler.New(ctx, p,
		// Enable HTTP server for validation and schema endpoints.
		reconciler.WithServer(reconciler.ServerConfig{
			Port:           8082,
			ValidationPath: "/validate",
		}),
		// Bind metrics to a non-conflicting port (8080 is taken by OpenEverest v1).
		reconciler.WithMetrics(":9091"),
	)
	if err != nil {
		l.Error(err, "unable to create reconciler")
		os.Exit(1)
	}

	if err := r.Start(ctx); err != nil {
		l.Error(err, "unable to start reconciler")
		os.Exit(1)
	}
}
