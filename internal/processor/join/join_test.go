package join

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

const baseTS = 1757600000.0

// row builds a Prometheus matrix entry with one sample per minute.
func row(labels map[string]any, vals ...float64) map[string]any {
	ts := make([]float64, len(vals))
	for i := range vals {
		ts[i] = baseTS + float64(60*i)
	}
	return rowAt(labels, ts, vals)
}

// rowAt builds a matrix entry with explicit timestamps.
func rowAt(labels map[string]any, ts, vals []float64) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		b, _ := json.Marshal(v)
		values = append(values, []any{ts[i], string(b)})
	}
	return map[string]any{"metric": labels, "values": values}
}

func matrix(items ...map[string]any) map[string]any {
	list := make([]any, 0, len(items))
	for _, it := range items {
		list = append(list, it)
	}
	return map[string]any{"resultType": "matrix", "result": list}
}

func labels(instance string) map[string]any {
	return map[string]any{"instance": instance, "job": "node"}
}

func withCPUMem() map[string]any {
	return map[string]any{"inputs": []any{"cpu", "mem"}, "join_by": []any{"instance"}}
}

func run(t *testing.T, with map[string]any, data any) series.Multi {
	t.Helper()
	out, err := New().Run(context.Background(), with, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("output must be JSON-compatible: %v", err)
	}
	m, err := series.ParseMulti(out)
	if err != nil {
		t.Fatalf("output must be the multivariate form: %v", err)
	}
	return m
}

func TestName(t *testing.T) {
	if New().Name() != "join" {
		t.Errorf("Name() = %q", New().Name())
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with map[string]any
		err  string
	}{
		{"ok", withCPUMem(), ""},
		{"bare strings", map[string]any{"inputs": "cpu", "join_by": "instance"}, ""},
		{"templated inputs deferred", map[string]any{"inputs": []any{"{{ .a }}", "b"}, "join_by": "{{ .l }}"}, ""},
		{"inputs missing", map[string]any{"join_by": "instance"}, "with.inputs: required (the call names to join, e.g. [cpu, mem])"},
		{"join_by missing", map[string]any{"inputs": []any{"cpu"}}, "with.join_by: required (the labels to join the inputs by, e.g. [instance])"},
		{"inputs empty", map[string]any{"inputs": []any{}, "join_by": "instance"}, "with.inputs: must not be empty"},
		{"join_by empty", map[string]any{"inputs": "cpu", "join_by": []any{}}, "with.join_by: must not be empty"},
		{"duplicate input", map[string]any{"inputs": []any{"cpu", "cpu"}, "join_by": "instance"}, `with.inputs: duplicate "cpu"`},
		{"duplicate label", map[string]any{"inputs": "cpu", "join_by": []any{"a", "a"}}, `with.join_by: duplicate "a"`},
		{"empty element", map[string]any{"inputs": []any{"cpu", " "}, "join_by": "instance"}, "with.inputs[1]: must not be empty"},
		{"non-string element", map[string]any{"inputs": []any{"cpu", 1}, "join_by": "instance"}, "with.inputs[1]: must be a string, got number"},
		{"not a list", map[string]any{"inputs": map[string]any{}, "join_by": "instance"}, "with.inputs: must be a list of strings, got object"},
		{"unknown key", map[string]any{"inputs": "cpu", "join_by": "instance", "method": "x"}, "with: unknown field(s) method (allowed: inputs, join_by)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := New().Validate(tc.with)
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestRunUnrendered(t *testing.T) {
	data := map[string]any{"cpu": matrix(), "mem": matrix()}
	for _, tc := range []struct {
		with map[string]any
		err  string
	}{
		{map[string]any{"inputs": []any{"{{ .a }}", "mem"}, "join_by": "instance"}, `with.inputs[0]: unrendered template`},
		{map[string]any{"inputs": []any{"cpu", "mem"}, "join_by": "{{ .l }}"}, `with.join_by[0]: unrendered template`},
	} {
		if _, err := New().Run(context.Background(), tc.with, data, nil); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("with %v: error = %v, want %q", tc.with, err, tc.err)
		}
	}
}

func TestRunJoins(t *testing.T) {
	cpu := matrix(row(labels("vm-2"), 1, 2, 3), row(labels("vm-1"), 4, 5, 6))
	mem := matrix(
		row(labels("vm-1"), 7, 8, 9),
		row(labels("vm-2"), 10, 11, 12),
		row(labels("vm-3"), 13, 14),
		row(map[string]any{"job": "node"}, 1, 2, 3),
	)
	out, err := New().Run(context.Background(), withCPUMem(), map[string]any{"cpu": cpu, "mem": mem, "unused": "ignored"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := out.(map[string]any)
	if !reflect.DeepEqual(raw["dims"], []any{"cpu", "mem"}) {
		t.Errorf("dims = %v", raw["dims"])
	}
	list := raw["series"].([]any)
	if len(list) != 4 {
		t.Fatalf("series = %v", list)
	}
	// The unjoined series comes first with its own labels; then the join
	// keys in sorted order.
	nolabel := list[0].(map[string]any)
	if nolabel["reason"] != "input mem: missing join_by label(s) instance" || !reflect.DeepEqual(nolabel["labels"], map[string]any{"job": "node"}) {
		t.Errorf("unjoined = %v", nolabel)
	}
	if !reflect.DeepEqual(nolabel["input_points"], map[string]any{"mem": 3}) || len(nolabel["points"].([]any)) != 0 {
		t.Errorf("unjoined diagnostics = %v", nolabel)
	}
	vm1 := list[1].(map[string]any)
	if !reflect.DeepEqual(vm1["labels"], map[string]any{"instance": "vm-1"}) {
		t.Errorf("labels must carry only the join_by labels: %v", vm1["labels"])
	}
	if !reflect.DeepEqual(vm1["points"], []any{[]any{baseTS, 4.0, 7.0}, []any{baseTS + 60, 5.0, 8.0}, []any{baseTS + 120, 6.0, 9.0}}) {
		t.Errorf("vm-1 points = %v", vm1["points"])
	}
	if !reflect.DeepEqual(vm1["input_points"], map[string]any{"cpu": 3, "mem": 3}) || vm1["reason"] != nil {
		t.Errorf("vm-1 = %v", vm1)
	}
	if vm2 := list[2].(map[string]any); vm2["labels"].(map[string]any)["instance"] != "vm-2" || vm2["points"].([]any)[0].([]any)[1] != 1.0 {
		t.Errorf("vm-2 = %v", vm2)
	}
	vm3 := list[3].(map[string]any)
	if vm3["reason"] != "missing in input(s) cpu" || len(vm3["points"].([]any)) != 0 || !reflect.DeepEqual(vm3["input_points"], map[string]any{"cpu": 0, "mem": 2}) {
		t.Errorf("vm-3 = %v", vm3)
	}
	// The output is the form the next processor parses.
	m := run(t, withCPUMem(), map[string]any{"cpu": cpu, "mem": mem})
	if len(m.Series) != 4 || m.Series[3].Reason == "" || len(m.Series[1].Points) != 3 {
		t.Errorf("parsed = %+v", m)
	}
}

func TestRunJoinsByTimestamp(t *testing.T) {
	cpu := []float64{1, 2, 3, 4, 5}
	mem := []float64{6, 7, 8, 9, 10}
	ts := []float64{baseTS, baseTS + 60, baseTS + 120, baseTS + 180 + 30, baseTS + 240 + 30} // last two off the cpu grid
	data := map[string]any{
		"cpu": matrix(row(labels("vm-1"), cpu...)),
		"mem": matrix(rowAt(labels("vm-1"), ts, mem)),
	}
	m := run(t, withCPUMem(), data)
	if len(m.Series) != 1 || len(m.Series[0].Points) != 3 {
		t.Fatalf("series = %+v", m.Series)
	}
	if p := m.Series[0].Points[2]; p.T != baseTS+120 || !reflect.DeepEqual(p.V, []float64{3, 8}) {
		t.Errorf("point = %+v", p)
	}
	// The first input drives the order of the samples.
	reversed := []float64{baseTS + 120, baseTS + 60, baseTS}
	data = map[string]any{
		"cpu": matrix(rowAt(labels("vm-1"), reversed, []float64{3, 2, 1})),
		"mem": matrix(row(labels("vm-1"), 6, 7, 8)),
	}
	m = run(t, withCPUMem(), data)
	if got := m.Series[0].Points; len(got) != 3 || got[0].T != baseTS+120 || got[0].V[1] != 8 {
		t.Errorf("order must follow the first input: %+v", got)
	}
}

func TestRunSingleInputAndMultiLabelKey(t *testing.T) {
	// One input is a plain matrix → multivariate conversion.
	m := run(t, map[string]any{"inputs": "cpu", "join_by": "instance"}, map[string]any{"cpu": matrix(row(labels("vm-1"), 1, 2))})
	if !reflect.DeepEqual(m.Dims, []string{"cpu"}) || len(m.Series) != 1 || !reflect.DeepEqual(m.Series[0].Points[1].V, []float64{2}) {
		t.Errorf("single input = %+v", m)
	}
	// A composite key keeps every join_by label.
	a := matrix(row(map[string]any{"ns": "prod", "pod": "x", "extra": "1"}, 1), row(map[string]any{"ns": "dev", "pod": "x"}, 2))
	b := matrix(row(map[string]any{"ns": "prod", "pod": "x", "extra": "2"}, 3), row(map[string]any{"ns": "dev", "pod": "x"}, 4))
	m = run(t, map[string]any{"inputs": []any{"a", "b"}, "join_by": []any{"ns", "pod"}}, map[string]any{"a": a, "b": b})
	if len(m.Series) != 2 || !reflect.DeepEqual(m.Series[0].Labels, map[string]string{"ns": "dev", "pod": "x"}) || !reflect.DeepEqual(m.Series[1].Points[0].V, []float64{1, 3}) {
		t.Errorf("composite key = %+v", m.Series)
	}
}

func TestRunErrors(t *testing.T) {
	cpu := matrix(row(labels("vm-1"), 1, 2, 3))
	dupCPU := matrix(row(labels("vm-1"), 1, 2, 3), row(map[string]any{"instance": "vm-1", "cpu": "0"}, 1, 2, 3))
	vector := map[string]any{"resultType": "vector", "result": []any{}}
	for _, tc := range []struct {
		name string
		data any
		err  string
	}{
		{"not an object", []any{1}, "expected an object of call results {cpu: matrix, mem: matrix} (a calls: tool, or a preceding fn: jq step that builds it), got array"},
		{"single-call form", matrix(), "input cpu: no such call result (available: result, resultType)"},
		{"missing input", map[string]any{"cpu": cpu}, "input mem: no such call result (available: cpu)"},
		{"not a matrix", map[string]any{"cpu": cpu, "mem": vector}, `input mem: expected resultType "matrix"`},
		{"bad sample", map[string]any{"cpu": cpu, "mem": map[string]any{"resultType": "matrix", "result": []any{map[string]any{"values": []any{[]any{1, "x"}}}}}}, "input mem: result[0].values[0]: value"},
		{"duplicate key", map[string]any{"cpu": dupCPU, "mem": cpu}, `input cpu: result[0] and result[1] share the join key {instance="vm-1"}; aggregate the query to one series per object (e.g. sum by (instance) in PromQL)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Run(context.Background(), withCPUMem(), tc.data, nil)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("error = %v, want %q", err, tc.err)
			}
		})
	}
}

func TestRunEmptyInputs(t *testing.T) {
	m := run(t, withCPUMem(), map[string]any{"cpu": matrix(), "mem": matrix()})
	if !reflect.DeepEqual(m.Dims, []string{"cpu", "mem"}) || len(m.Series) != 0 {
		t.Errorf("empty = %+v", m)
	}
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := map[string]any{"cpu": matrix(row(labels("vm-1"), 1)), "mem": matrix(row(labels("vm-1"), 1))}
	if _, err := New().Run(ctx, withCPUMem(), data, nil); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %v", err)
	}
}
