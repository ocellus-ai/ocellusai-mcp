// Package template renders request and response templates with text/template,
// the sprig function set and a few Prometheus helpers.
package template

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/prometheus/common/model"
)

// Params is the per-call parameter set exposed to templates via {{ param "name" }}.
type Params map[string]any

// Results holds the outputs of the named worker calls executed so far, keyed
// by call name. Templates read them through the result function.
type Results map[string]any

// Template is a compiled text/template.
type Template struct {
	name string
	t    *template.Template
}

// Parse compiles a template. Unknown functions and syntax errors are reported here.
func Parse(name, text string) (*Template, error) {
	t, err := template.New(name).Funcs(FuncMap()).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	return &Template{name: name, t: t}, nil
}

// Execute renders the template with data as the root context ("."), params
// available through the param function and results through the result
// function. results may be nil for tools without named calls.
func (t *Template) Execute(data any, params Params, results Results) (string, error) {
	c, err := t.t.Clone()
	if err != nil {
		return "", fmt.Errorf("template %s: %w", t.name, err)
	}
	c.Funcs(template.FuncMap{"param": paramFunc(params), "result": resultFunc(results)})
	var buf bytes.Buffer
	if err := c.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template %s: %w", t.name, err)
	}
	return buf.String(), nil
}

// FuncMap returns the function set available in every template: sprig plus
// promLabel, promRegex, promDuration, promDurationSeconds, pathEscape, param
// and result.
func FuncMap() template.FuncMap {
	fm := sprig.TxtFuncMap()
	fm["promLabel"] = PromLabel
	fm["promRegex"] = PromRegex
	fm["promDuration"] = PromDuration
	fm["promDurationSeconds"] = PromDurationSeconds
	fm["pathEscape"] = PathEscape
	fm["param"] = func(string) (any, error) {
		return nil, errors.New("param is only available while executing a template")
	}
	fm["result"] = func(string) (any, error) {
		return nil, errors.New("result is only available while executing a template")
	}
	return fm
}

func paramFunc(p Params) func(string) (any, error) {
	return func(name string) (any, error) {
		v, ok := p[name]
		if !ok {
			return nil, fmt.Errorf("param %q is not declared for this tool", name)
		}
		return v, nil
	}
}

// resultFunc reads the output of an earlier named call. Only calls executed
// before the current template are present, so a reference to a later call
// fails the same way as an unknown name.
func resultFunc(r Results) func(string) (any, error) {
	return func(name string) (any, error) {
		v, ok := r[name]
		if !ok {
			if len(r) == 0 {
				return nil, fmt.Errorf("result %q: no call result is available here", name)
			}
			names := make([]string, 0, len(r))
			for n := range r {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("result %q: no such call result (available: %s)", name, strings.Join(names, ", "))
		}
		return v, nil
	}
}

// Refs returns the string literals passed as the first argument to the
// template function fn, for example "pods" for {{ result "pods" }}, across
// every template defined in t. Arguments that are not literals are not
// reported; those are checked when the template executes.
func (t *Template) Refs(fn string) []string {
	var out []string
	for _, tpl := range t.t.Templates() {
		if tpl.Tree != nil && tpl.Root != nil {
			out = walkRefs(tpl.Root, fn, out)
		}
	}
	return out
}

func walkRefs(n parse.Node, fn string, out []string) []string {
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return out
		}
		for _, c := range x.Nodes {
			out = walkRefs(c, fn, out)
		}
	case *parse.ActionNode:
		out = walkRefs(x.Pipe, fn, out)
	case *parse.IfNode:
		out = walkBranch(&x.BranchNode, fn, out)
	case *parse.RangeNode:
		out = walkBranch(&x.BranchNode, fn, out)
	case *parse.WithNode:
		out = walkBranch(&x.BranchNode, fn, out)
	case *parse.TemplateNode:
		if x.Pipe != nil {
			out = walkRefs(x.Pipe, fn, out)
		}
	case *parse.PipeNode:
		if x == nil {
			return out
		}
		for _, c := range x.Cmds {
			out = walkRefs(c, fn, out)
		}
	case *parse.CommandNode:
		if len(x.Args) >= 2 {
			if id, ok := x.Args[0].(*parse.IdentifierNode); ok && id.Ident == fn {
				if s, ok := x.Args[1].(*parse.StringNode); ok {
					out = append(out, s.Text)
				}
			}
		}
		for _, a := range x.Args {
			out = walkRefs(a, fn, out)
		}
	case *parse.ChainNode:
		out = walkRefs(x.Node, fn, out)
	}
	return out
}

func walkBranch(b *parse.BranchNode, fn string, out []string) []string {
	out = walkRefs(b.Pipe, fn, out)
	out = walkRefs(b.List, fn, out)
	if b.ElseList != nil {
		out = walkRefs(b.ElseList, fn, out)
	}
	return out
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)

// PromLabel escapes a value for use as a PromQL label matcher value and wraps it in quotes.
func PromLabel(v any) string {
	return `"` + labelEscaper.Replace(stringify(v)) + `"`
}

// PromRegex escapes regular-expression metacharacters so the value matches literally.
func PromRegex(v any) string {
	return regexp.QuoteMeta(stringify(v))
}

// PromDuration validates a Prometheus duration (5m, 1h30m, 2d, 1w) and returns
// it in canonical form.
func PromDuration(v any) (string, error) {
	d, err := model.ParseDuration(stringify(v))
	if err != nil {
		return "", fmt.Errorf("promDuration: %w", err)
	}
	return d.String(), nil
}

// PromDurationSeconds converts a Prometheus duration to whole seconds.
func PromDurationSeconds(v any) (int64, error) {
	d, err := model.ParseDuration(stringify(v))
	if err != nil {
		return 0, fmt.Errorf("promDurationSeconds: %w", err)
	}
	return int64(time.Duration(d).Seconds()), nil
}

// PathEscape escapes a value for use as one segment of a URL path, so that
// "/" and "?" in a parameter cannot change the request path (rest worker).
func PathEscape(v any) string {
	return url.PathEscape(stringify(v))
}

func stringify(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case fmt.Stringer:
		return s.String()
	default:
		return fmt.Sprint(v)
	}
}

// Tree is a request section (map/list/scalars) whose string leaves are compiled
// templates. Strings without template actions are kept as literals.
type Tree struct {
	root any
}

// CompileTree compiles every string leaf of v. name prefixes error messages.
func CompileTree(name string, v any) (*Tree, error) {
	root, err := compileNode(name, v)
	if err != nil {
		return nil, err
	}
	return &Tree{root: root}, nil
}

func compileNode(path string, v any) (any, error) {
	switch x := v.(type) {
	case string:
		if !strings.Contains(x, "{{") {
			return x, nil
		}
		return Parse(path, x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			n, err := compileNode(path+"."+k, val)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			n, err := compileNode(fmt.Sprintf("%s[%d]", path, i), val)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

// Render executes every template leaf with params as the root context (and
// results available through the result function) and returns a copy of the
// tree with plain strings.
func (tr *Tree) Render(params Params, results Results) (any, error) {
	return renderNode(tr.root, params, results)
}

// Refs collects Template.Refs over every template leaf of the tree.
func (tr *Tree) Refs(fn string) []string {
	return refsNode(tr.root, fn, nil)
}

func refsNode(v any, fn string, out []string) []string {
	switch x := v.(type) {
	case *Template:
		return append(out, x.Refs(fn)...)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = refsNode(x[k], fn, out)
		}
	case []any:
		for _, val := range x {
			out = refsNode(val, fn, out)
		}
	}
	return out
}

func renderNode(v any, params Params, results Results) (any, error) {
	switch x := v.(type) {
	case *Template:
		return x.Execute(map[string]any(params), params, results)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			n, err := renderNode(val, params, results)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			n, err := renderNode(val, params, results)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}
