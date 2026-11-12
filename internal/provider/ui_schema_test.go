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
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uiFields returns every component of the generated UI schema that writes a
// path.
func uiFields(t *testing.T) []map[string]any {
	t.Helper()
	spec := testProvider(t).Spec
	require.NotNil(t, spec.UISchema)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(spec.UISchema.Raw, &schema))

	var fields []map[string]any
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, ok := n["path"].(string); ok {
				if _, ok := n["uiType"]; ok {
					fields = append(fields, n)
				}
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(schema)
	require.NotEmpty(t, fields)
	return fields
}

// The API validates a CPU quantity as a whole number or a string, so a number
// field may only write whole cores: 0.5 is rejected with a 400 (a Keeper CPU of
// 0.5 used to block creating replicated instances from the UI).
func TestUICPUFieldsWriteValidQuantities(t *testing.T) {
	var cpu []map[string]any
	for _, field := range uiFields(t) {
		if strings.HasSuffix(field["path"].(string), ".cpu") {
			cpu = append(cpu, field)
		}
	}
	require.NotEmpty(t, cpu)

	for _, field := range cpu {
		path := field["path"]
		require.Equal(t, "number", field["uiType"], path)
		params, _ := field["fieldParams"].(map[string]any)
		if params["badgeToApi"] == true {
			continue
		}
		validation, _ := field["validation"].(map[string]any)
		assert.Equal(t, true, validation["int"], "%s must only accept whole cores", path)
		for _, key := range []string{"defaultValue", "step", "min"} {
			v, ok := params[key]
			if !ok {
				v, ok = validation[key]
			}
			if ok {
				n, _ := v.(float64)
				assert.Equal(t, float64(int64(n)), n, "%s %s", path, key)
			}
		}
	}
}

var celPath = regexp.MustCompile(`(?:original\.)?(spec(?:\.\w+)+)`)

// setPath stores value at a dotted path, creating the maps on the way.
func setPath(obj map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := obj[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			obj[p] = next
		}
		obj = next
	}
	obj[parts[len(parts)-1]] = value
}

// An edit dialog evaluates the CEL rules of every section, but only parses the
// fields of its own section: the others reach CEL as the raw strings the form
// read ("10" for "10Gi"), while `original` holds numbers. A rule comparing them
// without double() fails with "no such overload" and keeps Save disabled.
func TestUIEditRulesAcceptStringAndNumberValues(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("spec", cel.DynType),
		cel.Variable("original", cel.DynType),
	)
	require.NoError(t, err)

	var rules []string
	for _, field := range uiFields(t) {
		validation, _ := field["validation"].(map[string]any)
		modes, _ := validation["modes"].(map[string]any)
		edit, _ := modes["edit"].(map[string]any)
		for _, list := range []any{validation["celExpressions"], edit["celExpressions"]} {
			exprs, _ := list.([]any)
			for _, e := range exprs {
				if expr, _ := e.(map[string]any)["celExpr"].(string); strings.Contains(expr, "original.") {
					rules = append(rules, expr)
				}
			}
		}
	}
	require.NotEmpty(t, rules)

	eval := func(expr string, form, original any) (any, error) {
		ast, iss := env.Compile(expr)
		require.NoError(t, iss.Err(), expr)
		prg, err := env.Program(ast)
		require.NoError(t, err)
		vars := map[string]any{"original": map[string]any{}}
		for _, m := range celPath.FindAllStringSubmatch(expr, -1) {
			setPath(vars, m[1], form)
			setPath(vars["original"].(map[string]any), m[1], original)
		}
		out, _, err := prg.Eval(vars)
		if err != nil {
			return nil, err
		}
		return out.Value(), nil
	}

	for _, expr := range rules {
		for _, tc := range []struct {
			form, original any
			want           bool
		}{
			{"10", 10, true},
			{10, 10, true},
			{int64(11), int64(10), true},
			{"9", 10, false},
			{9, 10, false},
		} {
			got, err := eval(expr, tc.form, tc.original)
			if assert.NoError(t, err, "%s with %#v vs %#v", expr, tc.form, tc.original) {
				assert.Equal(t, tc.want, got, "%s with %#v vs %#v", expr, tc.form, tc.original)
			}
		}
	}
}
