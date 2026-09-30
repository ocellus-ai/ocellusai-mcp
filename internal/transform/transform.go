// Package transform runs jq expressions (gojq) over JSON-compatible values.
package transform

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/itchyny/gojq"
)

// Program is a compiled jq expression. Tool parameters are bound to $params.
type Program struct {
	expr string
	code *gojq.Code
}

// Compile parses and compiles a jq expression once; it is safe for concurrent Run calls.
func Compile(expr string) (*Program, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("jq parse: %w", err)
	}
	code, err := gojq.Compile(q, gojq.WithVariables([]string{"$params"}))
	if err != nil {
		return nil, fmt.Errorf("jq compile: %w", err)
	}
	return &Program{expr: expr, code: code}, nil
}

// Run evaluates the program. Zero outputs yield nil, one output is returned as
// is, several outputs are collected into an array.
func (p *Program) Run(ctx context.Context, input any, params map[string]any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	iter := p.code.RunWithContext(ctx, Normalize(input), Normalize(params))
	var out []any
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			return nil, fmt.Errorf("jq: %w", err)
		}
		out = append(out, v)
	}
	switch len(out) {
	case 0:
		return nil, nil
	case 1:
		return out[0], nil
	default:
		return out, nil
	}
}

// Normalize converts v into the value set gojq accepts (nil, bool, int,
// float64, string, []any, map[string]any). Unknown types go through JSON.
func Normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int, float64:
		return x
	case int8:
		return int(x)
	case int16:
		return int(x)
	case int32:
		return int(x)
	case int64:
		return int(x)
	case uint8:
		return int(x)
	case uint16:
		return int(x)
	case uint32:
		return int(x)
	case uint:
		if x > math.MaxInt {
			return float64(x)
		}
		return int(x)
	case uint64:
		if x > math.MaxInt {
			return float64(x)
		}
		return int(x)
	case float32:
		return float64(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Normalize(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Normalize(e)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = e
		}
		return out
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		var generic any
		if err := json.Unmarshal(b, &generic); err != nil {
			return string(b)
		}
		return generic
	}
}
