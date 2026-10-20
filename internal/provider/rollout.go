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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// rolloutGate decides whether a rendered ClickHouseInstallation or
// ClickHouseKeeperInstallation may be applied now. Changing either rolls the
// pods. A change that follows from the user editing the Instance is applied
// right away. A change the provider makes on its own, with the Instance
// unchanged (typically after a provider upgrade), is a maintenance action:
// it is held until the Instance's maintenance tolerance or approval allows it.
//
// What was last applied, and from which Instance inputs, is recorded per CR
// in a provider-owned Secret (Secrets are cached already; a ConfigMap would
// add a cluster-wide informer). A CR without a record was last applied by a
// provider predating this gate: it is applied and recorded as the baseline.
//
// The inputs are taken from the stored Instance, not from the copy Sync is
// handed: the runtime fills that copy's component versions in from the
// version bundle, so a provider release moving a bundle to a new patch would
// otherwise look like a user edit.
type rolloutGate struct {
	record map[string]string
	inputs string
	token  string
	// approved caches the maintenance decision, so the CHK and the CHI of a
	// replicated instance are one held action rather than two.
	approved *bool
}

// newRolloutGate loads the record and derives one approval token for the
// rendered objects, so a replicated instance needs a single approval.
func newRolloutGate(c *controller.Context, rendered ...client.Object) (*rolloutGate, error) {
	g := &rolloutGate{record: map[string]string{}}

	state := &corev1.Secret{}
	if err := c.Get(state, rolloutStateName(c.Name())); err == nil {
		for k, v := range state.Data {
			// Inputs recorded in an earlier format cannot be compared; see apply.
			if !strings.HasSuffix(k, specSuffix) && !strings.HasSuffix(k, inputsSuffix) {
				continue
			}
			g.record[k] = string(v)
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read rollout state: %w", err)
	}

	inputs, err := inputsHash(c)
	if err != nil {
		return nil, err
	}
	g.inputs = inputs

	release := "change"
	if spec, err := c.ProviderSpec(); err == nil && spec.Release != nil && spec.Release.Version != "" {
		release = spec.Release.Version
	}
	digest := sha256.New()
	for _, obj := range rendered {
		h, err := specHash(obj)
		if err != nil {
			return nil, err
		}
		digest.Write([]byte(h))
	}
	g.token = fmt.Sprintf("provider-%s-%s", release, hex.EncodeToString(digest.Sum(nil))[:8])
	return g, nil
}

// apply applies obj when allowed and records it; it reports whether obj was
// applied. key names the object in the record ("chi" or "chk").
func (g *rolloutGate) apply(c *controller.Context, obj client.Object, key string) (bool, error) {
	spec, err := specHash(obj)
	if err != nil {
		return false, err
	}
	exists, err := c.Exists(obj.DeepCopyObject().(client.Object), obj.GetName())
	if err != nil {
		return false, fmt.Errorf("check %s: %w", obj.GetName(), err)
	}

	// Inputs that differ from the recorded ones mean the user changed the
	// Instance. Without recorded inputs (a record of an earlier format) a
	// changed rendering cannot be attributed to the user, so it is held.
	recordedInputs := g.record[key+inputsSuffix]
	allowed := !exists ||
		g.record[key+specSuffix] == "" ||
		g.record[key+specSuffix] == spec ||
		(recordedInputs != "" && recordedInputs != g.inputs)
	if !allowed {
		if g.approved == nil {
			approved := c.RequestMaintenance(g.token,
				"Apply the provider's updated ClickHouse configuration (rolling restart)",
				controller.MaintenanceRollingRestart)
			g.approved = &approved
		}
		allowed = *g.approved
	}
	if !allowed {
		log.FromContext(c.Context()).Info("Holding provider-initiated change until maintenance is approved",
			"object", obj.GetName(), "token", g.token)
		return false, nil
	}

	if err := c.Apply(obj); err != nil {
		return false, err
	}
	if g.record[key+specSuffix] == spec && recordedInputs == g.inputs {
		return true, nil
	}
	g.record[key+specSuffix] = spec
	g.record[key+inputsSuffix] = g.inputs
	return true, g.save(c)
}

func (g *rolloutGate) save(c *controller.Context) error {
	data := make(map[string][]byte, len(g.record))
	for k, v := range g.record {
		data[k] = []byte(v)
	}
	state := &corev1.Secret{ObjectMeta: c.ObjectMeta(rolloutStateName(c.Name())), Type: corev1.SecretTypeOpaque, Data: data}
	if err := c.Apply(state); err != nil {
		return fmt.Errorf("record rollout state: %w", err)
	}
	return nil
}

func rolloutStateName(instanceName string) string {
	return instanceName + common.RolloutStateSuffix
}

// Record keys: <cr>.spec, and <cr>.inputs-v<N> for the inputs. Bump N
// whenever inputsHash changes what it fingerprints, so records of the earlier
// format are recognised (and their changes held) instead of every Instance
// looking edited after a provider upgrade. v1 was <cr>.inputs (fingerprinting
// the runtime-prepared Instance), v2 <cr>.instance (without credentials).
const (
	specSuffix   = ".spec"
	inputsSuffix = ".inputs-v3"
)

// inputsHash fingerprints the stored Instance fields the CRs are rendered
// from. Maintenance approvals, backups and the deletion policy are left out:
// changing them is not a change to the cluster.
func inputsHash(c *controller.Context) (string, error) {
	stored := &corev1alpha1.Instance{}
	if err := c.Get(stored, c.Name()); err != nil {
		return "", fmt.Errorf("read stored Instance: %w", err)
	}
	spec := stored.Spec
	// The administrative credentials are user input too: rotating the
	// password rolls the pods and must not wait for approval.
	var credentials string
	secret := &corev1.Secret{}
	if found, err := getOptional(c, secret, credentialsSecretName(c.Name())); err != nil {
		return "", err
	} else if found {
		credentials = userCredentialsRevision(secret)
	}
	return hashJSON(struct {
		Version     string `json:"version,omitempty"`
		Topology    any    `json:"topology,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
		Components  any    `json:"components,omitempty"`
		Credentials string `json:"credentials,omitempty"`
	}{spec.Version, spec.Topology, spec.Parameters, spec.Components, credentials})
}

// specHash fingerprints a rendered object's spec.
func specHash(obj client.Object) (string, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", obj.GetName(), err)
	}
	return hashJSON(raw["spec"])
}

func hashJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
