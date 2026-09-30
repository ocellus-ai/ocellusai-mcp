// Package jq is the `jq` processor: one gojq expression applied to the data
// flowing through the process chain. It is the generic shaping step of the
// chain — it picks a call result by name, turns a worker payload into the
// canonical input of a typed processor (anomaly, outliers) or trims a report
// before the next step — so typed processors can each accept one strict
// input form instead of growing their own selectors.
//
// with:
//
//	expr  the jq expression, required. It must be a literal (no template
//	      actions): the expression is compiled once at catalog load and tool
//	      parameters are available inside it as $params, exactly as in
//	      response.jq.
//
// Output follows response.jq: zero results → null, one result → as is,
// several results → an array.
package jq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/transform"
)

// Name is the processor name referenced from YAML (fn: jq).
const Name = "jq"

// Processor implements processor.Processor. Compiled expressions are cached
// by their text; since expr must be a literal, the cache is bounded by the
// number of jq steps in the catalog.
type Processor struct {
	mu       sync.Mutex
	programs map[string]*transform.Program
}

// New creates the jq processor.
func New() *Processor { return &Processor{programs: map[string]*transform.Program{}} }

// Name returns "jq".
func (*Processor) Name() string { return Name }

// Validate compiles with.expr at catalog load time, so a syntax error names
// the file and the step before the server starts.
func (p *Processor) Validate(with map[string]any) error {
	_, err := p.program(with)
	return err
}

// Run evaluates the expression over data with the tool parameters bound to $params.
func (p *Processor) Run(ctx context.Context, with map[string]any, data any, params map[string]any) (any, error) {
	prog, err := p.program(with)
	if err != nil {
		return nil, err
	}
	return prog.Run(ctx, data, params)
}

// program reads with.expr and returns its compiled form, compiling on first use.
func (p *Processor) program(with map[string]any) (*transform.Program, error) {
	if err := processor.CheckKeys(with, "expr"); err != nil {
		return nil, err
	}
	expr, err := processor.String(with, "expr")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(expr) == "" {
		return nil, errors.New("with.expr: required (a jq expression)")
	}
	if processor.IsTemplate(expr) {
		return nil, fmt.Errorf("with.expr: must be a literal jq expression, template actions are not allowed (tool parameters are available as $params): %q", expr)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prog, ok := p.programs[expr]; ok {
		return prog, nil
	}
	prog, err := transform.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("with.expr: %w", err)
	}
	p.programs[expr] = prog
	return prog, nil
}
