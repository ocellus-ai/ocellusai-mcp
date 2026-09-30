package transform

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func fixture(t *testing.T) any {
	t.Helper()
	var v any
	err := json.Unmarshal([]byte(`{"resultType":"vector","result":[
	  {"metric":{"job":"node","instance":"a"},"value":[1,"1"]},
	  {"metric":{"job":"node","instance":"b"},"value":[1,"0"]}]}`), &v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRunSingleOutput(t *testing.T) {
	p, err := Compile(`[.result[] | {i: .metric.instance, v: (.value[1] | tonumber)}]`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), fixture(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"i": "a", "v": 1}, map[string]any{"i": "b", "v": 0}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %#v", out)
	}
}

func TestParamsVariableAndMultipleOutputs(t *testing.T) {
	p, err := Compile(`.[] | select(. >= $params.min)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Run(context.Background(), []any{1, 5, 10}, map[string]any{"min": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []any{5, 10}) {
		t.Errorf("out = %#v", out)
	}
}

func TestZeroOutputsIsNil(t *testing.T) {
	p, _ := Compile(`empty`)
	out, err := p.Run(context.Background(), 1, nil)
	if err != nil || out != nil {
		t.Errorf("out = %#v, err = %v", out, err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := Compile(`.[`); err == nil {
		t.Error("expected parse error")
	}
	if _, err := Compile(`$undefined`); err == nil {
		t.Error("expected compile error for undefined variable")
	}
	p, _ := Compile(`.a.b`)
	if _, err := p.Run(context.Background(), "str", nil); err == nil {
		t.Error("expected runtime error")
	}
}

func TestNormalize(t *testing.T) {
	type S struct {
		A int `json:"a"`
	}
	in := map[string]any{
		"i64": int64(7),
		"u8":  uint8(3),
		"f32": float32(1.5),
		"num": json.Number("12"),
		"ss":  []string{"x"},
		"ms":  map[string]string{"k": "v"},
		"st":  S{A: 1},
	}
	out := Normalize(in).(map[string]any)
	if out["i64"] != 7 || out["u8"] != 3 || out["f32"] != 1.5 || out["num"] != 12 {
		t.Errorf("numbers: %#v", out)
	}
	if !reflect.DeepEqual(out["ss"], []any{"x"}) || !reflect.DeepEqual(out["ms"], map[string]any{"k": "v"}) {
		t.Errorf("collections: %#v", out)
	}
	if !reflect.DeepEqual(out["st"], map[string]any{"a": float64(1)}) {
		t.Errorf("struct: %#v", out["st"])
	}
	p, _ := Compile(`.i64 + .u8`)
	if v, err := p.Run(context.Background(), in, nil); err != nil || v != 10 {
		t.Errorf("gojq over normalized input: %v, %v", v, err)
	}
}
