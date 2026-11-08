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
	"errors"
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openeverest/openeverest/v2/provider-runtime/preflight"
	"github.com/openeverest/openeverest/v2/provider-runtime/reconciler"

	"github.com/openeverest/provider-altinity-clickhouse/internal/provider"
)

// main is the entry point for the provider.
func main() {
	var preflightSpec string
	flag.StringVar(&preflightSpec, "preflight-spec", "",
		"Run the upgrade preflight against the target Provider spec at this path and exit. Used by the chart's pre-upgrade hook.")
	var admission bool
	flag.BoolVar(&admission, "admission", true, "Serve the Instance admission webhook, with the serving certificate the chart mounts.")
	flag.Parse()

	l := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	p := provider.New()

	// A non-zero exit fails the hook Job, which aborts the Helm upgrade
	// before any of the release's resources are applied.
	if preflightSpec != "" {
		if err := preflight.Run(ctx, p, preflight.Options{TargetSpecFile: preflightSpec}); err != nil {
			if !errors.Is(err, preflight.ErrUpgradeBlocked) {
				// The controller-runtime logger is not set up on this path.
				fmt.Fprintln(os.Stderr, "upgrade preflight could not run:", err)
			}
			os.Exit(1)
		}
		return
	}

	opts := []reconciler.ReconcilerOption{
		// Serve the health and readiness probes.
		reconciler.WithServer(reconciler.ServerConfig{Port: 8082}),
		// Bind metrics to a non-conflicting port (8080 is taken by OpenEverest v1).
		reconciler.WithMetrics(":9091"),
	}
	if admission {
		opts = append(opts, reconciler.WithAdmission())
	}

	r, err := reconciler.New(ctx, p, opts...)
	if err != nil {
		l.Error(err, "unable to create reconciler")
		os.Exit(1)
	}

	if err := r.Start(ctx); err != nil {
		l.Error(err, "unable to start reconciler")
		os.Exit(1)
	}
}
