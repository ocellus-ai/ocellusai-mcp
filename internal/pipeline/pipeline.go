// Package pipeline glues a tool call together: arguments → for every call:
// request templates → worker → call jq → process steps → jq → render.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/template"
)

// Stage names used in errors and logs.
const (
	StageArguments = "arguments"
	StageRequest   = "request"
	StageWorker    = "worker"
	StageProcess   = "process"
	StageJQ        = "jq"
	StageRender    = "render"
	StageEncode    = "encode"
)

// Error reports which stage of the pipeline failed.
type Error struct {
	Stage string
	Err   error
}

func (e *Error) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Result is what the agent receives.
type Result struct {
	// Text is the rendered template or the pretty-printed JSON of Structured.
	Text string
	// Structured is the jq result (or the raw worker result when jq is absent).
	Structured any
}

// Pipeline executes tools.
type Pipeline struct {
	logger *slog.Logger
}

// New creates a pipeline.
func New(logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{logger: logger}
}

// Run executes tool with raw JSON arguments.
//
// Calls run sequentially. In the single-call form the worker result is the
// value handed to process/response; in the calls: form every call result is
// stored under the call name and process/response receive {name: result}.
// Templates of later calls, of process[i].with and of response.render read
// earlier results through the result function.
func (p *Pipeline) Run(ctx context.Context, tool *catalog.Tool, raw json.RawMessage) (*Result, error) {
	params, err := tool.BindArgs(raw)
	if err != nil {
		return nil, &Error{Stage: StageArguments, Err: err}
	}
	results := template.Results{}
	var data any
	for i, call := range tool.Calls {
		out, err := p.runCall(ctx, tool, i, call, params, results)
		if err != nil {
			return nil, err
		}
		if tool.Named() {
			results[call.Name] = out
		} else {
			data = out
		}
	}
	if tool.Named() {
		merged := make(map[string]any, len(results))
		for k, v := range results {
			merged[k] = v
		}
		data = merged
	}
	for i, step := range tool.Steps {
		data, err = runStep(ctx, i, step, params, results, data)
		if err != nil {
			return nil, &Error{Stage: StageProcess, Err: err}
		}
		p.logger.Debug("process step done", "tool", tool.Spec.Name, "step", i, "fn", step.Fn)
	}
	if tool.JQ != nil {
		data, err = tool.JQ.Run(ctx, data, params)
		if err != nil {
			return nil, &Error{Stage: StageJQ, Err: err}
		}
	}
	res := &Result{Structured: data}
	if tool.Render != nil {
		res.Text, err = tool.Render.Execute(data, params, results)
		if err != nil {
			return nil, &Error{Stage: StageRender, Err: err}
		}
		return res, nil
	}
	res.Text, err = PrettyJSON(data)
	if err != nil {
		return nil, &Error{Stage: StageEncode, Err: err}
	}
	return res, nil
}

// runCall renders the request of one call with the parameters and the
// results collected so far, executes the worker and applies the call's jq.
// In the calls: form errors are prefixed with calls[i] (name); the
// single-call form keeps the plain error.
func (p *Pipeline) runCall(ctx context.Context, tool *catalog.Tool, i int, call catalog.CompiledCall, params map[string]any, results template.Results) (any, error) {
	loc := ""
	if tool.Named() {
		loc = fmt.Sprintf("calls[%d] (%s): ", i, call.Name)
	}
	rendered, err := call.Request.Render(params, results)
	if err != nil {
		return nil, &Error{Stage: StageRequest, Err: locate(loc, err)}
	}
	req, ok := rendered.(map[string]any)
	if !ok {
		return nil, &Error{Stage: StageRequest, Err: locate(loc, fmt.Errorf("request rendered to %T, want object", rendered))}
	}
	p.logger.Debug("request rendered", "tool", tool.Spec.Name, "call", call.Name, "worker", call.WorkerName, "request", req)

	out, err := call.Worker.Execute(ctx, req)
	if err != nil {
		return nil, &Error{Stage: StageWorker, Err: locate(loc, err)}
	}
	if call.JQ != nil {
		out, err = call.JQ.Run(ctx, out, params)
		if err != nil {
			return nil, &Error{Stage: StageJQ, Err: locate(loc, err)}
		}
	}
	return out, nil
}

// locate prefixes err with the call locator when there is one.
func locate(loc string, err error) error {
	if loc == "" {
		return err
	}
	return fmt.Errorf("%s%w", loc, err)
}

// runStep renders the step's `with` section with the call parameters and
// applies the processor to data. Errors name the step as process[i] (fn).
func runStep(ctx context.Context, i int, step catalog.CompiledStep, params map[string]any, results template.Results, data any) (any, error) {
	rendered, err := step.With.Render(params, results)
	if err != nil {
		return nil, fmt.Errorf("process[%d] (%s): %w", i, step.Fn, err)
	}
	with, ok := rendered.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("process[%d] (%s): with rendered to %T, want object", i, step.Fn, rendered)
	}
	out, err := step.Processor.Run(ctx, with, data, params)
	if err != nil {
		return nil, fmt.Errorf("process[%d] (%s): %w", i, step.Fn, err)
	}
	return out, nil
}

// PrettyJSON encodes v with two-space indentation and no HTML escaping.
func PrettyJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
