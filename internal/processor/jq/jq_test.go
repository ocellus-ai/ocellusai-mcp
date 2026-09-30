package jq

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	if New().Name() != "jq" {
		t.Errorf("Name() = %q", New().Name())
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		with map[string]any
		err  string
	}{
		{"ok", map[string]any{"expr": ".a"}, ""},
		{"missing", map[string]any{}, "with.expr: required"},
		{"empty", map[string]any{"expr": "  "}, "with.expr: required"},
		{"not a string", map[string]any{"expr": 1}, "with.expr: must be a string"},
		{"template", map[string]any{"expr": ".[] | select(.n > {{ .min }})"}, "must be a literal jq expression"},
		{"syntax", map[string]any{"expr": ".[ | "}, "with.expr: jq parse:"},
		{"unknown key", map[string]any{"expr": ".", "query": "."}, "with: unknown field(s) query (allowed: expr)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := New().Validate(c.with)
			if c.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
		})
	}
}

func TestRun(t *testing.T) {
	p := New()
	data := map[string]any{"pods": []any{
		map[string]any{"pod": "a", "restarts": 1},
		map[string]any{"pod": "b", "restarts": 7},
	}}
	// One result comes back as is; $params is bound to the tool arguments.
	out, err := p.Run(context.Background(), map[string]any{"expr": "[.pods[] | select(.restarts >= $params.min) | .pod]"}, data, map[string]any{"min": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []any{"b"}) {
		t.Errorf("out = %#v", out)
	}
	// Several results are collected, zero results yield null; nil params are allowed.
	out, err = p.Run(context.Background(), map[string]any{"expr": ".pods[].pod"}, data, nil)
	if err != nil || !reflect.DeepEqual(out, []any{"a", "b"}) {
		t.Errorf("out = %#v, err = %v", out, err)
	}
	out, err = p.Run(context.Background(), map[string]any{"expr": "empty"}, data, nil)
	if err != nil || out != nil {
		t.Errorf("out = %#v, err = %v", out, err)
	}
	// A runtime jq error is reported as such.
	_, err = p.Run(context.Background(), map[string]any{"expr": ".pods | .x"}, data, nil)
	if err == nil || !strings.Contains(err.Error(), "jq:") {
		t.Errorf("err = %v", err)
	}
}

func TestProgramCache(t *testing.T) {
	p := New()
	with := map[string]any{"expr": ".a"}
	if err := p.Validate(with); err != nil {
		t.Fatal(err)
	}
	first := p.programs[".a"]
	if first == nil {
		t.Fatal("Validate must compile and cache the expression")
	}
	if _, err := p.Run(context.Background(), with, map[string]any{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if p.programs[".a"] != first || len(p.programs) != 1 {
		t.Errorf("Run must reuse the compiled program: %#v", p.programs)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context stops evaluation of an expression that would otherwise not finish.
	_, err := New().Run(ctx, map[string]any{"expr": "[range(1; 1e12)] | length"}, nil, nil)
	if err == nil {
		t.Error("expected an error from a cancelled context")
	}
}
