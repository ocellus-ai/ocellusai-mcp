package series

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

const baseTS = 1757600000.0

func row(labels map[string]any, vals ...any) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		values = append(values, []any{baseTS + float64(60*i), v})
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

func TestParseMatrix(t *testing.T) {
	data := matrix(
		row(map[string]any{"pod": "a"}, "1", "2", "3"),
		row(map[string]any{"pod": "b"}, "NaN", "+Inf", 4.0, 5),
		map[string]any{"metric": map[string]any{}},
	)
	got, err := ParseMatrix(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Labels["pod"] != "a" || len(got[0].Points) != 3 || got[0].Points[2].V != 3 || got[0].Points[1].T != baseTS+60 {
		t.Errorf("series[0] = %+v", got[0])
	}
	if len(got[1].Points) != 2 || got[1].Points[0].V != 4 || got[1].Points[1].V != 5 {
		t.Errorf("NaN/Inf should be dropped, numbers accepted: %+v", got[1].Points)
	}
	if len(got[2].Points) != 0 {
		t.Errorf("missing values should give an empty series: %+v", got[2])
	}
	if got, err := ParseMatrix(map[string]any{"resultType": "matrix", "result": nil}); err != nil || len(got) != 0 {
		t.Errorf("nil result: %v %v", got, err)
	}

	bad := map[string]struct {
		data any
		want string
	}{
		"not object":     {[]any{1}, "expected a Prometheus range result {resultType: matrix, result: [...]} (a range query, or a preceding fn: jq step that builds this shape), got array"},
		"vector":         {map[string]any{"resultType": "vector", "result": []any{}}, `expected resultType "matrix" (a range query: request.type: range; other sources are shaped by a preceding fn: jq step), got "vector"`},
		"result scalar":  {map[string]any{"resultType": "matrix", "result": "x"}, "result: expected array"},
		"item not obj":   {map[string]any{"resultType": "matrix", "result": []any{1}}, "result[0]: expected object"},
		"values not arr": {map[string]any{"resultType": "matrix", "result": []any{map[string]any{"values": 1}}}, "result[0].values: expected array"},
		"bad pair":       {map[string]any{"resultType": "matrix", "result": []any{map[string]any{"values": []any{[]any{1}}}}}, "result[0].values[0]: expected [timestamp, value]"},
		"bad value":      {map[string]any{"resultType": "matrix", "result": []any{map[string]any{"values": []any{[]any{1, "abc"}}}}}, `result[0].values[0]: value: "abc" is not a number`},
		"bad timestamp":  {map[string]any{"resultType": "matrix", "result": []any{map[string]any{"values": []any{[]any{true, "1"}}}}}, "timestamp: expected a number, got boolean"},
	}
	for name, c := range bad {
		_, err := ParseMatrix(c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestToMatrixRoundTrip(t *testing.T) {
	in := []Series{
		{Labels: map[string]string{"pod": "a"}, Points: []Point{{T: baseTS, V: 0.25}, {T: baseTS + 60, V: 1e-7}}},
		{Labels: map[string]string{}, Points: nil},
	}
	out := ToMatrix(in)
	if out["resultType"] != "matrix" {
		t.Errorf("resultType = %v", out["resultType"])
	}
	first := out["result"].([]any)[0].(map[string]any)
	if v := first["values"].([]any)[0].([]any); v[0] != baseTS || v[1] != "0.25" {
		t.Errorf("values are [ts, string] like the API: %v", v)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("matrix must be JSON-compatible: %v", err)
	}
	back, err := ParseMatrix(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back[0], in[0]) || len(back[1].Points) != 0 || back[1].Labels == nil {
		t.Errorf("round trip = %+v, want %+v", back, in)
	}
}

func multiForm(dims []any, series ...map[string]any) map[string]any {
	list := make([]any, 0, len(series))
	for _, s := range series {
		list = append(list, s)
	}
	return map[string]any{"dims": dims, "series": list}
}

func TestParseMulti(t *testing.T) {
	data := multiForm([]any{"cpu", "mem"},
		map[string]any{"labels": map[string]any{"instance": "vm-1", "n": 1}, "points": []any{
			[]any{baseTS, 0.3, "0.7"}, []any{baseTS + 60, math.NaN(), 0.7}, []any{baseTS + 120, 0.31, 0.72},
		}, "input_points": map[string]any{"cpu": 3, "mem": 3}},
		map[string]any{"labels": map[string]any{"instance": "vm-2"}, "points": []any{}, "reason": "missing in input(s) mem"},
		map[string]any{},
	)
	got, err := ParseMulti(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Dims, []string{"cpu", "mem"}) || len(got.Series) != 3 {
		t.Fatalf("multi = %+v", got)
	}
	vm1 := got.Series[0]
	if vm1.Labels["instance"] != "vm-1" || vm1.Labels["n"] != "1" || !reflect.DeepEqual(vm1.Dims, got.Dims) {
		t.Errorf("labels/dims = %+v", vm1)
	}
	if len(vm1.Points) != 2 || vm1.Points[0].V[1] != 0.7 || vm1.Points[1].T != baseTS+120 {
		t.Errorf("non-finite point dropped, numeric strings accepted: %+v", vm1.Points)
	}
	if vm2 := got.Series[1]; vm2.Reason != "missing in input(s) mem" || len(vm2.Points) != 0 {
		t.Errorf("reason = %+v", vm2)
	}
	if empty := got.Series[2]; empty.Labels == nil || len(empty.Points) != 0 || empty.Reason != "" {
		t.Errorf("empty entry = %+v", empty)
	}

	bad := map[string]struct {
		data any
		want string
	}{
		"not object":    {[]any{1}, "expected a multivariate series object {dims: [...], series: [...]} (the output of fn: join), got array"},
		"matrix":        {matrix(), "dims: expected a non-empty list of dimension names, got null"},
		"dims empty":    {multiForm([]any{}), "dims: expected a non-empty list"},
		"dim not str":   {multiForm([]any{"cpu", 1}), "dims[1]: expected a non-empty string, got number"},
		"dim dup":       {multiForm([]any{"cpu", "cpu"}), `dims[1]: duplicate dimension "cpu"`},
		"series scalar": {map[string]any{"dims": []any{"a"}, "series": "x"}, "series: expected array, got string"},
		"entry not obj": {map[string]any{"dims": []any{"a"}, "series": []any{1}}, "series[0]: expected object, got number"},
		"labels scalar": {multiForm([]any{"a"}, map[string]any{"labels": 1}), "series[0].labels: expected object, got number"},
		"reason number": {multiForm([]any{"a"}, map[string]any{"reason": 1}), "series[0].reason: expected string, got number"},
		"points scalar": {multiForm([]any{"a"}, map[string]any{"points": 1}), "series[0].points: expected array, got number"},
		"row short":     {multiForm([]any{"a", "b"}, map[string]any{"points": []any{[]any{1, 2}}}), "series[0].points[0]: expected [timestamp, a, b]"},
		"row not arr":   {multiForm([]any{"a"}, map[string]any{"points": []any{1}}), "series[0].points[0]: expected [timestamp, a]"},
		"bad timestamp": {multiForm([]any{"a"}, map[string]any{"points": []any{[]any{"x", 1}}}), `series[0].points[0]: timestamp: "x" is not a number`},
		"bad value":     {multiForm([]any{"a", "b"}, map[string]any{"points": []any{[]any{1, 2, true}}}), "series[0].points[0]: b: expected a number, got boolean"},
	}
	for name, c := range bad {
		_, err := ParseMulti(c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestToMultiRoundTrip(t *testing.T) {
	in := Multi{Dims: []string{"cpu", "mem"}, Series: []MultiSeries{
		{Labels: map[string]string{"instance": "vm-1"}, Dims: []string{"cpu", "mem"}, Points: []MultiPoint{{T: baseTS, V: []float64{0.3, 0.7}}}},
		{Labels: map[string]string{"instance": "vm-2"}, Dims: []string{"cpu", "mem"}, Reason: "missing in input(s) cpu"},
	}}
	out := ToMulti(in, []map[string]any{{"input_points": map[string]any{"cpu": 1, "mem": 1}}})
	if !reflect.DeepEqual(out["dims"], []any{"cpu", "mem"}) {
		t.Errorf("dims = %v", out["dims"])
	}
	list := out["series"].([]any)
	first := list[0].(map[string]any)
	if !reflect.DeepEqual(first["points"], []any{[]any{baseTS, 0.3, 0.7}}) || first["reason"] != nil {
		t.Errorf("first = %v", first)
	}
	if !reflect.DeepEqual(first["input_points"], map[string]any{"cpu": 1, "mem": 1}) {
		t.Errorf("extra keys are merged into the entry: %v", first)
	}
	second := list[1].(map[string]any)
	if second["reason"] != "missing in input(s) cpu" || len(second["points"].([]any)) != 0 {
		t.Errorf("second = %v", second)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("form must be JSON-compatible: %v", err)
	}
	back, err := ParseMulti(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Dims, in.Dims) || !reflect.DeepEqual(back.Series[0].Points, in.Series[0].Points) || back.Series[1].Reason != in.Series[1].Reason {
		t.Errorf("round trip = %+v", back)
	}
}

func TestFormatTime(t *testing.T) {
	if got := FormatTime(baseTS + 17*60); got != "2025-09-11T14:30:20Z" {
		t.Errorf("FormatTime = %q", got)
	}
	if got := FormatTime(baseTS + 0.5); got != "2025-09-11T14:13:20Z" {
		t.Errorf("fractional seconds are floored: %q", got)
	}
}
