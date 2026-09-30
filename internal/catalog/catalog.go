// Package catalog loads tool definitions from YAML files, validates them
// (fail fast, naming file and field) and compiles them for the pipeline.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/template"
	"github.com/ocellus-ai/ocellusai-mcp/internal/transform"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

var (
	toolNameRe  = regexp.MustCompile(`^[a-z0-9_]+$`)
	paramNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	callNameRe  = paramNameRe
)

// ParamTypes lists the supported params.*.type values.
var ParamTypes = []string{"string", "integer", "number", "boolean", "array", "object"}

// ParamSpec describes one input parameter; it maps 1:1 onto a JSON Schema property.
type ParamSpec struct {
	Type        string   `yaml:"type"`
	Required    bool     `yaml:"required"`
	Default     any      `yaml:"default"`
	Description string   `yaml:"description"`
	Enum        []any    `yaml:"enum"`
	Minimum     *float64 `yaml:"minimum"`
	Maximum     *float64 `yaml:"maximum"`
	Pattern     string   `yaml:"pattern"`
}

// Param is a named ParamSpec; Params keeps YAML order.
type Param struct {
	Name string
	ParamSpec
}

// Params is an ordered list of parameters.
type Params []Param

// UnmarshalYAML decodes a mapping while preserving key order.
func (p *Params) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: params must be a mapping", node.Line)
	}
	out := make(Params, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		var spec ParamSpec
		dec := strictNode(node.Content[i+1])
		if err := dec(&spec); err != nil {
			return fmt.Errorf("params.%s: %w", node.Content[i].Value, err)
		}
		out = append(out, Param{Name: node.Content[i].Value, ParamSpec: spec})
	}
	*p = out
	return nil
}

// strictNode decodes a yaml.Node into v rejecting unknown fields.
func strictNode(n *yaml.Node) func(v any) error {
	return func(v any) error {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		if err := enc.Encode(n); err != nil {
			return err
		}
		_ = enc.Close()
		dec := yaml.NewDecoder(&buf)
		dec.KnownFields(true)
		return dec.Decode(v)
	}
}

// ResponseSpec describes the optional post-processing of the worker result.
type ResponseSpec struct {
	JQ     string `yaml:"jq"`
	Render string `yaml:"render"`
}

// Step is one entry of the `process:` list: a processor name and its `with`
// section, which the processor owns and validates itself.
type Step struct {
	Fn   string         `yaml:"fn"`
	With map[string]any `yaml:"with"`
}

// Steps is the `process:` list. It decodes each step strictly and reports a
// readable error when the section is not a list.
type Steps []Step

// UnmarshalYAML decodes a sequence of steps rejecting unknown step fields.
func (s *Steps) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*s = nil
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: process must be a list of steps ({fn: <processor>, with: {...}})", node.Line)
	}
	out := make(Steps, 0, len(node.Content))
	for i, n := range node.Content {
		var st Step
		if err := strictNode(n)(&st); err != nil {
			return fmt.Errorf("process[%d]: %w", i, err)
		}
		out = append(out, st)
	}
	*s = out
	return nil
}

// Call is one entry of the `calls:` list: a named invocation of a worker
// instance. Calls run sequentially; the request templates of a call may read
// the outputs of the calls before it through the result function, and the
// optional jq shapes the worker result before it is stored under Name.
type Call struct {
	Name    string         `yaml:"name"`
	Worker  string         `yaml:"worker"`
	Request map[string]any `yaml:"request"`
	JQ      string         `yaml:"jq"`
}

// Calls is the `calls:` list. It decodes each call strictly and reports a
// readable error when the section is not a list.
type Calls []Call

// UnmarshalYAML decodes a sequence of calls rejecting unknown call fields.
func (c *Calls) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*c = nil
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: calls must be a list of calls ({name: <name>, worker: <instance>, request: {...}, jq: <optional>})", node.Line)
	}
	out := make(Calls, 0, len(node.Content))
	for i, n := range node.Content {
		var call Call
		if err := strictNode(n)(&call); err != nil {
			return fmt.Errorf("calls[%d]: %w", i, err)
		}
		out = append(out, call)
	}
	*c = out
	return nil
}

// Spec is the YAML description of one tool. A tool invokes its workers in one
// of two forms: the single-call form (`worker:` + `request:`) or the `calls:`
// list of named sequential calls; the two are mutually exclusive.
type Spec struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Worker      string         `yaml:"worker"`
	Params      Params         `yaml:"params"`
	Request     map[string]any `yaml:"request"`
	Calls       Calls          `yaml:"calls"`
	Process     Steps          `yaml:"process"`
	Response    *ResponseSpec  `yaml:"response"`
}

// CompiledStep is a Step bound to its processor with the `with` templates compiled.
type CompiledStep struct {
	Fn        string
	Processor processor.Processor
	// With is the compiled `with` section; it renders to a map per call.
	With *template.Tree
}

// CompiledCall is a Call bound to its worker instance with the request
// templates and the optional jq compiled.
type CompiledCall struct {
	// Name is the key under which the call result is stored; it is empty for
	// the single-call form, whose result is passed on as is.
	Name string
	// WorkerName is the configured instance name (`worker:` in YAML).
	WorkerName string
	Worker     worker.Worker
	// Request is the compiled request section; it renders to a map per call.
	Request *template.Tree
	// JQ shapes the worker result before it is stored; nil when absent.
	JQ *transform.Program
}

// Tool is a validated, compiled tool ready to be executed by the pipeline.
type Tool struct {
	Spec Spec
	File string
	// Schema is the MCP inputSchema.
	Schema *jsonschema.Schema
	// Calls is the compiled call chain, in order. The single-call form
	// (`worker:` + `request:`) yields exactly one call with an empty Name.
	Calls []CompiledCall
	// Steps is the compiled `process:` chain, in order; empty when absent.
	Steps []CompiledStep
	// JQ is nil when response.jq is absent.
	JQ *transform.Program
	// Render is nil when response.render is absent.
	Render *template.Template

	resolved *jsonschema.Resolved
	defaults map[string]any
}

// Load reads every *.yaml / *.yml file in dir (sorted by name).
func Load(dir string, workers worker.Registry, processors processor.Registry) ([]*Tool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("tools_dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("tools_dir %s: no *.yaml files found", dir)
	}
	tools := make([]*Tool, 0, len(files))
	seen := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		t, err := Parse(f, data, workers, processors)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[t.Spec.Name]; dup {
			return nil, fmt.Errorf("%s: name: %q already defined in %s", f, t.Spec.Name, prev)
		}
		seen[t.Spec.Name] = f
		tools = append(tools, t)
	}
	return tools, nil
}

// Parse validates and compiles a single YAML document. file is used in errors.
// processors may be nil when the catalog is not expected to use `process:`.
func Parse(file string, data []byte, workers worker.Registry, processors processor.Registry) (*Tool, error) {
	t, err := parse(data, workers, processors)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	t.File = file
	return t, nil
}

func parse(data []byte, workers worker.Registry, processors processor.Registry) (*Tool, error) {
	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	if spec.Name == "" {
		return nil, errors.New("name: required")
	}
	if !toolNameRe.MatchString(spec.Name) {
		return nil, fmt.Errorf("name: %q must match %s", spec.Name, toolNameRe)
	}
	if strings.TrimSpace(spec.Description) == "" {
		return nil, errors.New("description: required (it is what the agent sees)")
	}
	calls, named, err := callList(spec)
	if err != nil {
		return nil, err
	}

	t := &Tool{Spec: spec, defaults: map[string]any{}}
	if err := t.buildSchema(); err != nil {
		return nil, err
	}
	t.Calls, err = compileCalls(calls, named, workers)
	if err != nil {
		return nil, err
	}
	steps, err := compileSteps(spec.Process, processors)
	if err != nil {
		return nil, err
	}
	t.Steps = steps
	if spec.Response != nil {
		if strings.TrimSpace(spec.Response.JQ) != "" {
			prog, err := transform.Compile(spec.Response.JQ)
			if err != nil {
				return nil, fmt.Errorf("response.jq: %w", err)
			}
			t.JQ = prog
		}
		if strings.TrimSpace(spec.Response.Render) != "" {
			tpl, err := template.Parse("response.render", spec.Response.Render)
			if err != nil {
				return nil, fmt.Errorf("response.render: %w", err)
			}
			t.Render = tpl
		}
	}
	if err := t.checkResultRefs(); err != nil {
		return nil, err
	}
	return t, nil
}

// callList picks the call form of spec: the single-call form becomes one
// unnamed call, the calls: list is used as is. named reports which form it was.
func callList(spec Spec) (calls Calls, named bool, err error) {
	if len(spec.Calls) == 0 {
		if spec.Worker == "" {
			return nil, false, errors.New("worker: required (or a calls: list)")
		}
		if len(spec.Request) == 0 {
			return nil, false, errors.New("request: required")
		}
		return Calls{{Worker: spec.Worker, Request: spec.Request}}, false, nil
	}
	if spec.Worker != "" {
		return nil, true, errors.New("worker: not allowed together with calls (set calls[i].worker instead)")
	}
	if len(spec.Request) != 0 {
		return nil, true, errors.New("request: not allowed together with calls (set calls[i].request instead)")
	}
	return spec.Calls, true, nil
}

// compileCalls resolves every call against the worker registry, lets the
// worker validate its raw request and compiles the templates and jq. Errors
// of the single-call form keep their historical shape (request.<key>: ...);
// errors of the calls: form are prefixed with calls[i].
func compileCalls(calls Calls, named bool, workers worker.Registry) ([]CompiledCall, error) {
	out := make([]CompiledCall, 0, len(calls))
	seen := map[string]int{}
	for i, c := range calls {
		field := ""
		reqField := "request"
		if named {
			field = fmt.Sprintf("calls[%d]", i)
			reqField = field + ".request"
			switch {
			case strings.TrimSpace(c.Name) == "":
				return nil, fmt.Errorf("%s.name: required (it is the key the result is stored under)", field)
			case !callNameRe.MatchString(c.Name):
				return nil, fmt.Errorf("%s.name: %q must match %s", field, c.Name, callNameRe)
			}
			if prev, dup := seen[c.Name]; dup {
				return nil, fmt.Errorf("%s.name: duplicate call name %q (also calls[%d])", field, c.Name, prev)
			}
			seen[c.Name] = i
		}
		if c.Worker == "" {
			return nil, fmt.Errorf("%s: required (configured: %s)", join(field, "worker"), strings.Join(workers.Names(), ", "))
		}
		w, ok := workers[c.Worker]
		if !ok {
			return nil, fmt.Errorf("%s: unknown worker %q (configured: %s)", join(field, "worker"), c.Worker, strings.Join(workers.Names(), ", "))
		}
		if len(c.Request) == 0 {
			return nil, fmt.Errorf("%s: required", reqField)
		}
		if err := w.Validate(c.Request); err != nil {
			if named {
				return nil, fmt.Errorf("%s (%s): %w", field, c.Name, err)
			}
			return nil, err
		}
		tree, err := template.CompileTree(reqField, c.Request)
		if err != nil {
			return nil, err
		}
		cc := CompiledCall{Name: c.Name, WorkerName: c.Worker, Worker: w, Request: tree}
		if strings.TrimSpace(c.JQ) != "" {
			prog, err := transform.Compile(c.JQ)
			if err != nil {
				return nil, fmt.Errorf("%s.jq: %w", field, err)
			}
			cc.JQ = prog
		}
		out = append(out, cc)
	}
	return out, nil
}

// join builds "prefix.name" or just "name" when prefix is empty.
func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// checkResultRefs verifies at load time that every literal `result "x"` names
// a call that is executed before the template runs: earlier calls for a call
// request, any call for process[i].with and response.render.
func (t *Tool) checkResultRefs() error {
	var names []string
	for i, c := range t.Calls {
		field := "request"
		if t.Named() {
			field = fmt.Sprintf("calls[%d].request", i)
		}
		if err := checkRefs(c.Request.Refs("result"), names, field); err != nil {
			return err
		}
		if c.Name != "" {
			names = append(names, c.Name)
		}
	}
	for i, s := range t.Steps {
		if err := checkRefs(s.With.Refs("result"), names, fmt.Sprintf("process[%d].with", i)); err != nil {
			return err
		}
	}
	if t.Render != nil {
		if err := checkRefs(t.Render.Refs("result"), names, "response.render"); err != nil {
			return err
		}
	}
	return nil
}

func checkRefs(refs, available []string, field string) error {
	for _, r := range refs {
		if contains(available, r) {
			continue
		}
		if len(available) == 0 {
			return fmt.Errorf("%s: result %q: no call result is available here (name the calls in a calls: list and reference earlier ones)", field, r)
		}
		return fmt.Errorf("%s: result %q: no such call executed before this point (available: %s)", field, r, strings.Join(available, ", "))
	}
	return nil
}

// Named reports whether the tool uses the calls: form. Then the value entering
// process/response is an object {call name: result}; in the single-call form
// it is the worker result itself.
func (t *Tool) Named() bool {
	return len(t.Spec.Calls) > 0
}

// WorkerNames returns the worker instance names used by the tool, in call
// order and without duplicates.
func (t *Tool) WorkerNames() []string {
	var out []string
	for _, c := range t.Calls {
		if !contains(out, c.WorkerName) {
			out = append(out, c.WorkerName)
		}
	}
	return out
}

// WorkerTypes returns the worker types behind WorkerNames, in the same order
// and without duplicates.
func (t *Tool) WorkerTypes() []string {
	var out []string
	for _, c := range t.Calls {
		if typ := c.Worker.Type(); !contains(out, typ) {
			out = append(out, typ)
		}
	}
	return out
}

// compileSteps resolves every `process:` step against the registry, lets the
// processor validate its raw `with` section and compiles the templates in it.
func compileSteps(steps Steps, processors processor.Registry) ([]CompiledStep, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	out := make([]CompiledStep, 0, len(steps))
	for i, s := range steps {
		field := fmt.Sprintf("process[%d]", i)
		if strings.TrimSpace(s.Fn) == "" {
			return nil, fmt.Errorf("%s.fn: required (available: %s)", field, strings.Join(processors.Names(), ", "))
		}
		p, ok := processors[s.Fn]
		if !ok {
			return nil, fmt.Errorf("%s.fn: unknown processor %q (available: %s)", field, s.Fn, strings.Join(processors.Names(), ", "))
		}
		with := s.With
		if with == nil {
			with = map[string]any{}
		}
		if err := p.Validate(with); err != nil {
			return nil, fmt.Errorf("%s (%s): %w", field, s.Fn, err)
		}
		tree, err := template.CompileTree(field+".with", with)
		if err != nil {
			return nil, err
		}
		out = append(out, CompiledStep{Fn: s.Fn, Processor: p, With: tree})
	}
	return out, nil
}

func (t *Tool) buildSchema() error {
	s := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
	seen := map[string]bool{}
	for _, p := range t.Spec.Params {
		field := "params." + p.Name
		if !paramNameRe.MatchString(p.Name) {
			return fmt.Errorf("%s: name must match %s", field, paramNameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: duplicate parameter", field)
		}
		seen[p.Name] = true
		if !contains(ParamTypes, p.Type) {
			return fmt.Errorf("%s.type: %q is not one of %s", field, p.Type, strings.Join(ParamTypes, ", "))
		}
		if p.Required && p.Default != nil {
			return fmt.Errorf("%s: required and default are mutually exclusive", field)
		}
		if p.Pattern != "" && p.Type != "string" {
			return fmt.Errorf("%s.pattern: only valid for type string", field)
		}
		if (p.Minimum != nil || p.Maximum != nil) && p.Type != "integer" && p.Type != "number" {
			return fmt.Errorf("%s.minimum/maximum: only valid for numeric types", field)
		}
		ps := &jsonschema.Schema{
			Type:        p.Type,
			Description: p.Description,
			Enum:        p.Enum,
			Minimum:     p.Minimum,
			Maximum:     p.Maximum,
			Pattern:     p.Pattern,
		}
		if p.Default != nil {
			def := normalizeYAML(p.Default)
			b, err := json.Marshal(def)
			if err != nil {
				return fmt.Errorf("%s.default: %w", field, err)
			}
			ps.Default = b
			t.defaults[p.Name] = def
		}
		s.Properties[p.Name] = ps
		s.PropertyOrder = append(s.PropertyOrder, p.Name)
		if p.Required {
			s.Required = append(s.Required, p.Name)
		}
	}
	resolved, err := s.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return fmt.Errorf("params: %w", err)
	}
	t.Schema = s
	t.resolved = resolved
	return nil
}

// BindArgs unmarshals raw JSON arguments, validates them against the input
// schema, applies defaults and returns the parameter set used for rendering.
// Declared parameters that are absent are present with a nil value so
// templates can test them. Integer parameters are returned as int.
func (t *Tool) BindArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}
	if err := t.resolved.Validate(args); err != nil {
		return nil, err
	}
	for _, p := range t.Spec.Params {
		v, ok := args[p.Name]
		if !ok {
			if def, has := t.defaults[p.Name]; has {
				v = clone(def)
			} else {
				v = nil
			}
		}
		if p.Type == "integer" {
			if f, isFloat := v.(float64); isFloat {
				v = int(f)
			}
		}
		args[p.Name] = v
	}
	return args, nil
}

// Names returns tool names in catalog order.
func Names(tools []*Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Spec.Name
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// normalizeYAML converts yaml.v3 generic values (map[string]any / []any / int /
// float64 / ...) into the JSON-style value set used at runtime.
func normalizeYAML(v any) any {
	return transform.Normalize(v)
}

func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	default:
		return v
	}
}
