package processor

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

type fake struct{ name string }

func (f fake) Name() string                                                        { return f.name }
func (fake) Validate(map[string]any) error                                         { return nil }
func (fake) Run(context.Context, map[string]any, any, map[string]any) (any, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	r := Registry{}
	if err := r.Register(fake{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fake{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fake{"a"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate = %v", err)
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("names = %v", got)
	}
}

func TestCheckKeys(t *testing.T) {
	if err := CheckKeys(map[string]any{"a": 1, "b": 2}, "a", "b", "c"); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	err := CheckKeys(map[string]any{"z": 1, "a": 1, "y": 2}, "a")
	if err == nil || !strings.Contains(err.Error(), "with: unknown field(s) y, z (allowed: a)") {
		t.Errorf("err = %v", err)
	}
}

func TestIsTemplate(t *testing.T) {
	if !IsTemplate("{{ .x }}") || IsTemplate("plain") || IsTemplate(3) || IsTemplate(nil) {
		t.Error("IsTemplate misclassified a value")
	}
}

func TestFloat(t *testing.T) {
	ok := map[string]struct {
		v    any
		want float64
	}{
		"absent":    {nil, 7},
		"float64":   {2.5, 2.5},
		"float32":   {float32(1.5), 1.5},
		"int":       {3, 3},
		"int64":     {int64(4), 4},
		"string":    {"2.5", 2.5},
		"string ws": {" 3 ", 3},
	}
	for name, c := range ok {
		with := map[string]any{}
		if c.v != nil {
			with["k"] = c.v
		}
		got, err := Float(with, "k", 7)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, c.want)
		}
	}
	bad := map[string]any{
		"template": "{{ .x }}",
		"text":     "abc",
		"bool":     true,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"list":     []any{1},
	}
	for name, v := range bad {
		if _, err := Float(map[string]any{"k": v}, "k", 0); err == nil || !strings.Contains(err.Error(), "with.k") {
			t.Errorf("%s: expected error naming with.k, got %v", name, err)
		}
	}
	// nil value behaves like absent.
	if got, err := Float(map[string]any{"k": nil}, "k", 9); err != nil || got != 9 {
		t.Errorf("nil: got %v, %v", got, err)
	}
}

func TestInt(t *testing.T) {
	if got, err := Int(map[string]any{"n": "12"}, "n", 0); err != nil || got != 12 {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := Int(map[string]any{}, "n", 5); err != nil || got != 5 {
		t.Errorf("default: got %v, %v", got, err)
	}
	if _, err := Int(map[string]any{"n": 1.5}, "n", 0); err == nil || !strings.Contains(err.Error(), "integer") {
		t.Errorf("fractional: %v", err)
	}
}

func TestString(t *testing.T) {
	if s, err := String(map[string]any{"m": "iqr"}, "m"); err != nil || s != "iqr" {
		t.Errorf("got %q, %v", s, err)
	}
	if s, err := String(map[string]any{}, "m"); err != nil || s != "" {
		t.Errorf("absent: got %q, %v", s, err)
	}
	if _, err := String(map[string]any{"m": 3}, "m"); err == nil || !strings.Contains(err.Error(), "with.m") {
		t.Errorf("non-string: %v", err)
	}
}

func TestSeconds(t *testing.T) {
	ok := map[string]struct {
		v    any
		want float64
	}{
		"absent":   {nil, 7},
		"number":   {30, 30},
		"float":    {2.5, 2.5},
		"numeric":  {"45", 45},
		"duration": {"5m", 300},
		"hours":    {" 1h30m ", 5400},
		"zero":     {0, 0},
		"zero dur": {"0s", 0},
	}
	for name, tc := range ok {
		with := map[string]any{}
		if tc.v != nil {
			with["gap"] = tc.v
		}
		if got, err := Seconds(with, "gap", 7); err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v (want %v)", name, got, err, tc.want)
		}
	}
	bad := map[string]struct {
		v   any
		err string
	}{
		"template": {"{{ .g }}", "with.gap: unrendered template"},
		"text":     {"soon", `with.gap: expected seconds or a duration such as 30s, got "soon"`},
		"negative": {-1, "with.gap: must be >= 0 seconds, got -1"},
		"neg str":  {"-5", "with.gap: must be >= 0 seconds"},
		"nan":      {math.NaN(), "with.gap: must be a finite number"},
		"bool":     {true, "with.gap: must be a number, got bool"},
	}
	for name, tc := range bad {
		if _, err := Seconds(map[string]any{"gap": tc.v}, "gap", 0); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.err)
		}
	}
}

func TestBool(t *testing.T) {
	for _, v := range []any{true, "true", " True "} {
		if got, err := Bool(map[string]any{"b": v}, "b", false); err != nil || !got {
			t.Errorf("%v: got %v, %v", v, got, err)
		}
	}
	if got, err := Bool(map[string]any{}, "b", true); err != nil || !got {
		t.Errorf("default: got %v, %v", got, err)
	}
	for name, v := range map[string]any{"template": "{{ .x }}", "text": "yes please", "number": 1} {
		if _, err := Bool(map[string]any{"b": v}, "b", false); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseDimFloats(t *testing.T) {
	dims := []string{"cpu", "mem"}
	ok := map[string]struct {
		v        any
		cpu, mem float64
	}{
		"absent":         {nil, 0, 0},
		"number":         {5, 5, 5},
		"numeric string": {"2.5", 2.5, 2.5},
		"object":         {map[string]any{"cpu": 5, "mem": "3"}, 5, 3},
		"partial object": {map[string]any{"mem": 3}, 0, 3},
		"zero":           {0, 0, 0},
	}
	for name, tc := range ok {
		with := map[string]any{}
		if tc.v != nil {
			with["min_delta"] = tc.v
		}
		d, err := ParseDimFloats(with, "min_delta", false)
		if err != nil || d.Of("cpu") != tc.cpu || d.Of("mem") != tc.mem {
			t.Errorf("%s: got cpu %v mem %v err %v, want %v %v", name, d.Of("cpu"), d.Of("mem"), err, tc.cpu, tc.mem)
		}
		if err := d.Check("min_delta", dims); err != nil {
			t.Errorf("%s: check: %v", name, err)
		}
	}
	bad := map[string]struct {
		v   any
		err string
	}{
		"negative":        {-1, "with.min_delta: must be >= 0, got -1"},
		"negative member": {map[string]any{"cpu": -2}, "with.min_delta.cpu: must be >= 0, got -2"},
		"text member":     {map[string]any{"cpu": "lots"}, `with.min_delta.cpu: "lots" is not a number`},
		"list":            {[]any{1, 2}, "with.min_delta: must be a number or an object of numbers per dimension, got array"},
		"bool":            {true, "with.min_delta: must be a number, got bool"},
		"unrendered":      {"{{ .d }}", "with.min_delta: unrendered template"},
	}
	for name, tc := range bad {
		if _, err := ParseDimFloats(map[string]any{"min_delta": tc.v}, "min_delta", false); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.err)
		}
	}
	// Templates are skipped at load time, as a whole value and as a member.
	for name, v := range map[string]any{"whole": "{{ .d }}", "member": map[string]any{"cpu": "{{ .d }}", "mem": 3}} {
		d, err := ParseDimFloats(map[string]any{"min_delta": v}, "min_delta", true)
		if err != nil {
			t.Errorf("%s: load: %v", name, err)
		}
		if d.Of("cpu") != 0 {
			t.Errorf("%s: a template must not produce a value at load time", name)
		}
	}
	d, _ := ParseDimFloats(map[string]any{"min_delta": map[string]any{"cpu": 5, "disk": 10, "net": 1}}, "min_delta", false)
	if err := d.Check("min_delta", dims); err == nil || err.Error() != "with.min_delta: unknown dimension(s) disk, net (dims: cpu, mem)" {
		t.Errorf("check: %v", err)
	}
}

func TestSpanOf(t *testing.T) {
	ok := map[string]struct {
		v     any
		want  Span
		at60s int
	}{
		"absent":   {nil, Span{}, 0},
		"points":   {288, Span{Points: 288}, 288},
		"numeric":  {"12", Span{Points: 12}, 12},
		"duration": {"1d", Span{Seconds: 86400}, 1440},
		"short":    {"10s", Span{Seconds: 10}, 1},
		"zero":     {0, Span{}, 0},
	}
	for name, tc := range ok {
		with := map[string]any{}
		if tc.v != nil {
			with["window"] = tc.v
		}
		got, err := SpanOf(with, "window")
		if err != nil || got != tc.want || got.Resolve(60) != tc.at60s || got.IsSet() != (tc.at60s > 0) {
			t.Errorf("%s: got %+v (%d points at 60s), %v; want %+v, %d", name, got, got.Resolve(60), err, tc.want, tc.at60s)
		}
	}
	for name, v := range map[string]any{"negative": -1, "fraction": 2.5, "text": "long", "template": "{{ .w }}"} {
		if _, err := SpanOf(map[string]any{"window": v}, "window"); err == nil || !strings.Contains(err.Error(), "with.window: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
