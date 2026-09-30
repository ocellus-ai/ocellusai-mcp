package template

import (
	"reflect"
	"strings"
	"testing"
)

func TestPromLabel(t *testing.T) {
	got := PromLabel("a\"b\\c\nd")
	want := `"a\"b\\c\nd"`
	if got != want {
		t.Errorf("PromLabel = %s, want %s", got, want)
	}
	if PromLabel(42) != `"42"` {
		t.Errorf("PromLabel(42) = %s", PromLabel(42))
	}
}

func TestPromRegex(t *testing.T) {
	if got := PromRegex("api.v1*"); got != `api\.v1\*` {
		t.Errorf("PromRegex = %s", got)
	}
}

func TestPromDuration(t *testing.T) {
	if got, err := PromDuration("90s"); err != nil || got != "1m30s" {
		t.Errorf("PromDuration(90s) = %q, %v", got, err)
	}
	if _, err := PromDuration("2d"); err != nil {
		t.Errorf("2d should be valid: %v", err)
	}
	if _, err := PromDuration("soon"); err == nil {
		t.Error("expected error for invalid duration")
	}
	if got, err := PromDurationSeconds("5m"); err != nil || got != 300 {
		t.Errorf("PromDurationSeconds(5m) = %d, %v", got, err)
	}
}

func TestExecuteWithParamAndSprig(t *testing.T) {
	tpl, err := Parse("t", `{{ param "x" | upper }}-{{ .y }}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tpl.Execute(map[string]any{"y": 1}, Params{"x": "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "A-1" {
		t.Errorf("out = %q", out)
	}
	if _, err := tpl.Execute(map[string]any{"y": 1}, Params{}, nil); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("expected undeclared param error, got %v", err)
	}
}

func TestMissingKeyIsError(t *testing.T) {
	tpl, err := Parse("t", `{{ .nope }}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.Execute(map[string]any{"y": 1}, nil, nil); err == nil {
		t.Error("expected missing key error")
	}
}

func TestParseError(t *testing.T) {
	if _, err := Parse("t", `{{ .a `); err == nil {
		t.Error("expected syntax error")
	}
	if _, err := Parse("t", `{{ nosuchfunc . }}`); err == nil {
		t.Error("expected unknown function error")
	}
}

func TestTree(t *testing.T) {
	tree, err := CompileTree("request", map[string]any{
		"type":  "instant",
		"query": `up{ns={{ promLabel .ns }}}`,
		"args":  []any{"get", "{{ .kind }}", 5, true},
		"nested": map[string]any{
			"k": "{{ .ns }}",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tree.Render(Params{"ns": "prod", "kind": "pods"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["type"] != "instant" || m["query"] != `up{ns="prod"}` {
		t.Errorf("unexpected render: %#v", m)
	}
	args := m["args"].([]any)
	if args[1] != "pods" || args[2] != 5 || args[3] != true {
		t.Errorf("args = %#v", args)
	}
	if m["nested"].(map[string]any)["k"] != "prod" {
		t.Errorf("nested = %#v", m["nested"])
	}
	if _, err := CompileTree("request", map[string]any{"q": "{{ .a "}); err == nil {
		t.Error("expected compile error")
	}
	if _, err := tree.Render(Params{"ns": "prod"}, nil); err == nil {
		t.Error("expected render error for missing kind")
	}
}

func TestResultFunc(t *testing.T) {
	tpl, err := Parse("t", `{{ range result "pods" }}{{ .pod }},{{ end }}{{ (result "meta").ns }}`)
	if err != nil {
		t.Fatal(err)
	}
	results := Results{
		"pods": []any{map[string]any{"pod": "a"}, map[string]any{"pod": "b"}},
		"meta": map[string]any{"ns": "prod"},
	}
	out, err := tpl.Execute(nil, nil, results)
	if err != nil || out != "a,b,prod" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	// Unknown names list what is available; no results at all say so.
	_, err = tpl.Execute(nil, nil, Results{"other": 1})
	if err == nil || !strings.Contains(err.Error(), `result "pods": no such call result (available: other)`) {
		t.Errorf("err = %v", err)
	}
	_, err = tpl.Execute(nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `result "pods": no call result is available here`) {
		t.Errorf("err = %v", err)
	}
	// Outside Execute the function is defined but unusable, like param.
	if _, err := Parse("t", `{{ result "x" }}`); err != nil {
		t.Errorf("result must be a known function at parse time: %v", err)
	}
}

func TestRefs(t *testing.T) {
	tpl, err := Parse("t", `{{ result "a" }}{{ if result "b" }}{{ range $i, $x := result "c" }}{{ end }}{{ else }}{{ with (result "d") }}{{ end }}{{ end }}`+
		`{{ (result "e").k | upper }}{{ len (result "f") | printf "%d" }}{{ result .dyn }}{{ "g" | result }}{{ param "p" }}{{ define "sub" }}{{ result "h" }}{{ end }}{{ template "sub" . }}`)
	if err != nil {
		t.Fatal(err)
	}
	got := tpl.Refs("result")
	sortStrings(got)
	// .dyn and the piped "g" are dynamic, param is another function.
	if want := []string{"a", "b", "c", "d", "e", "f", "h"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs = %v, want %v", got, want)
	}
	if got := tpl.Refs("param"); !reflect.DeepEqual(got, []string{"p"}) {
		t.Errorf("Refs(param) = %v", got)
	}
	plain, _ := Parse("t", `no actions`)
	if got := plain.Refs("result"); len(got) != 0 {
		t.Errorf("Refs on a plain template = %v", got)
	}

	tree, err := CompileTree("request", map[string]any{
		"query": `{{ result "pods" }}`,
		"args":  []any{"get", `{{ result "ns" }}`, 5},
		"lit":   "no template here",
		"nested": map[string]any{
			"k": `{{ result "pods" }}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got = tree.Refs("result")
	sortStrings(got)
	if want := []string{"ns", "pods", "pods"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tree.Refs = %v, want %v", got, want)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func TestPathEscape(t *testing.T) {
	if got := PathEscape("a/b c?d#e"); got != "a%2Fb%20c%3Fd%23e" {
		t.Errorf("PathEscape = %q", got)
	}
	if got := PathEscape(nil); got != "" {
		t.Errorf("PathEscape(nil) = %q", got)
	}
	tpl, err := Parse("t", `/silence/{{ pathEscape .id }}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tpl.Execute(map[string]any{"id": "x/../y"}, Params{"id": "x/../y"}, nil)
	if err != nil || out != "/silence/x%2F..%2Fy" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}
