// Package processor defines the Processor interface: an optional step that
// transforms the worker result before jq/render. Steps are declared in the
// tool YAML under `process:` and chained in order; each step receives the
// previous step's output. Like workers, processors own their `with` section
// and validate it themselves, so new processors need no catalog changes.
package processor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

// Processor is one step of the `process:` chain.
type Processor interface {
	// Name is referenced from YAML (process: [{fn: <name>, with: {...}}]).
	Name() string
	// Validate checks a step's raw (unrendered) `with` section at catalog load
	// time. Values may still contain template actions; use IsTemplate to skip
	// checks that need the rendered value.
	Validate(with map[string]any) error
	// Run applies the step to data (the worker result or the previous step's
	// output) with an already rendered `with` section. params are the bound
	// tool arguments (after defaults), the same values the `with` templates
	// see; processors that evaluate expressions expose them as $params. Input
	// and output are JSON-compatible values; that is the contract the chain
	// and jq rely on.
	Run(ctx context.Context, with map[string]any, data any, params map[string]any) (any, error)
}

// Registry maps processor names to implementations.
type Registry map[string]Processor

// Register adds p; a duplicate name is an error.
func (r Registry) Register(p Processor) error {
	if _, dup := r[p.Name()]; dup {
		return fmt.Errorf("processor %q registered twice", p.Name())
	}
	r[p.Name()] = p
	return nil
}

// Names returns the registered processor names, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for n := range r {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// CheckKeys returns an error naming `with` keys that are not in allowed.
func CheckKeys(with map[string]any, allowed ...string) error {
	var unknown []string
	for k := range with {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("with: unknown field(s) %s (allowed: %s)", strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// IsTemplate reports whether v is a string that still contains a template
// action, i.e. its final value is only known after rendering.
func IsTemplate(v any) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, "{{")
}

// Float fetches an optional numeric field. Numbers and numeric strings (the
// result of rendering a template) are accepted; absent or nil yields def.
func Float(with map[string]any, key string, def float64) (float64, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return def, nil
	}
	switch x := v.(type) {
	case float64:
		return finite(key, x)
	case float32:
		return finite(key, float64(x))
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case string:
		if IsTemplate(x) {
			return 0, fmt.Errorf("with.%s: unrendered template %q", key, x)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("with.%s: %q is not a number", key, x)
		}
		return finite(key, f)
	default:
		return 0, fmt.Errorf("with.%s: must be a number, got %T", key, v)
	}
}

func finite(key string, f float64) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("with.%s: must be a finite number", key)
	}
	return f, nil
}

// Int fetches an optional integer field; a fractional value is an error.
func Int(with map[string]any, key string, def int) (int, error) {
	f, err := Float(with, key, float64(def))
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("with.%s: must be an integer, got %v", key, f)
	}
	return int(f), nil
}

// String fetches an optional string field. A present non-string value is an error.
func String(with map[string]any, key string) (string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("with.%s: must be a string, got %T", key, v)
	}
	return s, nil
}

// Number converts one data value to float64: JSON numbers and numeric
// strings (the Prometheus API writes samples as strings) are accepted.
// Unlike Float it reads a value, not a `with` key, and leaves NaN/Inf to the
// caller, which usually drops such samples.
func Number(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", x)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("expected a number, got %s", JSONType(v))
	}
}

// Seconds fetches an optional length of time: a number or a numeric string
// is seconds, any other string is a Prometheus duration such as 30s or 5m.
// Absent or nil yields def; a negative value is an error, zero is not (a
// processor that needs a positive value checks that itself).
func Seconds(with map[string]any, key string, def float64) (float64, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return def, nil
	}
	var sec float64
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if IsTemplate(s) {
			return 0, fmt.Errorf("with.%s: unrendered template %q", key, s)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			d, derr := model.ParseDuration(s)
			if derr != nil {
				return 0, fmt.Errorf("with.%s: expected seconds or a duration such as 30s, got %q", key, s)
			}
			f = time.Duration(d).Seconds()
		}
		sec = f
	} else {
		f, err := Float(with, key, def)
		if err != nil {
			return 0, err
		}
		sec = f
	}
	if math.IsNaN(sec) || math.IsInf(sec, 0) {
		return 0, fmt.Errorf("with.%s: must be a finite number of seconds, got %v", key, sec)
	}
	if sec < 0 {
		return 0, fmt.Errorf("with.%s: must be >= 0 seconds, got %v", key, sec)
	}
	return sec, nil
}

// Span is a length given either in points or as a duration; a duration is
// converted to points once the step of a series is known.
type Span struct {
	Points  int
	Seconds float64
}

// Resolve returns the span in points at the given step, at least 1 for a
// duration; 0 when unset.
func (s Span) Resolve(step float64) int {
	if s.Points > 0 {
		return s.Points
	}
	if s.Seconds > 0 {
		return max(1, int(math.Round(s.Seconds/step)))
	}
	return 0
}

// IsSet reports whether a length was given.
func (s Span) IsSet() bool { return s.Points > 0 || s.Seconds > 0 }

// SpanOf reads an optional length: a number is points, a string is points
// or a Prometheus duration such as 1h. Absent, nil and 0 are unset; a
// negative number is an error.
func SpanOf(with map[string]any, key string) (Span, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return Span{}, nil
	}
	points := func(n int) (Span, error) {
		if n < 0 {
			return Span{}, fmt.Errorf("with.%s: must be >= 0 points or a duration such as 1h, got %d", key, n)
		}
		return Span{Points: n}, nil
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if IsTemplate(s) {
			return Span{}, fmt.Errorf("with.%s: unrendered template %q", key, s)
		}
		if n, err := strconv.Atoi(s); err == nil {
			return points(n)
		}
		d, err := model.ParseDuration(s)
		if err != nil {
			return Span{}, fmt.Errorf("with.%s: expected a number of points or a duration such as 1h, got %q", key, s)
		}
		return Span{Seconds: time.Duration(d).Seconds()}, nil
	}
	n, err := Int(with, key, 0)
	if err != nil {
		return Span{}, err
	}
	return points(n)
}

// NoiseFloorShare is the share of the smallest significant change that a
// multivariate analysis takes as the noise floor of a dimension: anomaly_mv
// adds its square to the covariance diagonal, anomaly_ensemble adds Gaussian
// noise of that standard deviation to the detector input. Detection
// thresholds sit around 4 robust sigmas, so a floor of a quarter makes a
// change of about one unit the smallest one that can be flagged (measured on
// a production fleet on 2026-09-26: an eighth let twice as much noise through).
const NoiseFloorShare = 0.25

// DimFloats is an optional non-negative number per dimension of a
// multivariate series, read from `with`: a single number applies to every
// dimension, an object names dimensions ({cpu: 5, mem: 3}) and leaves the
// others at 0.
type DimFloats struct {
	all   float64
	byDim map[string]float64
}

// ParseDimFloats reads key. At load time a value that is still a template
// (the whole value or a member of the object) is skipped.
func ParseDimFloats(with map[string]any, key string, load bool) (DimFloats, error) {
	v, ok := with[key]
	if !ok || v == nil || (load && IsTemplate(v)) {
		return DimFloats{}, nil
	}
	nonNeg := func(name string, v any) (float64, error) {
		f, err := Float(map[string]any{name: v}, name, 0)
		if err != nil {
			return 0, err
		}
		if f < 0 {
			return 0, fmt.Errorf("with.%s: must be >= 0, got %v", name, f)
		}
		return f, nil
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		if _, isList := v.([]any); isList {
			return DimFloats{}, fmt.Errorf("with.%s: must be a number or an object of numbers per dimension, got array", key)
		}
		f, err := nonNeg(key, v)
		return DimFloats{all: f}, err
	}
	out := DimFloats{byDim: make(map[string]float64, len(obj))}
	for dim, x := range obj {
		if load && IsTemplate(x) {
			continue
		}
		f, err := nonNeg(key+"."+dim, x)
		if err != nil {
			return DimFloats{}, err
		}
		out.byDim[dim] = f
	}
	return out, nil
}

// Of returns the value for dimension dim.
func (d DimFloats) Of(dim string) float64 {
	if d.byDim != nil {
		return d.byDim[dim]
	}
	return d.all
}

// Check reports an object member that names none of dims (a typo would
// otherwise silently leave that dimension without a value).
func (d DimFloats) Check(key string, dims []string) error {
	known := make(map[string]bool, len(dims))
	for _, dim := range dims {
		known[dim] = true
	}
	var unknown []string
	for dim := range d.byDim {
		if !known[dim] {
			unknown = append(unknown, dim)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("with.%s: unknown dimension(s) %s (dims: %s)", key, strings.Join(unknown, ", "), strings.Join(dims, ", "))
}

// JSONType names the JSON type of a decoded value for error messages.
func JSONType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64, float32, int, int64, int32:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// Bool fetches an optional boolean field; "true"/"false" strings are accepted.
func Bool(with map[string]any, key string, def bool) (bool, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return def, nil
	}
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		if IsTemplate(x) {
			return false, fmt.Errorf("with.%s: unrendered template %q", key, x)
		}
		b, err := strconv.ParseBool(strings.TrimSpace(x))
		if err != nil {
			return false, fmt.Errorf("with.%s: %q is not a boolean", key, x)
		}
		return b, nil
	default:
		return false, fmt.Errorf("with.%s: must be a boolean, got %T", key, v)
	}
}
