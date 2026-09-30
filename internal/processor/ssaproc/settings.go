package ssaproc

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/ssa"
)

// Defaults of the `with` settings.
const (
	DefaultMinPoints = 20
	DefaultIters     = 8
	DefaultMaxGaps   = 0.2
	// autoFullMax and autoK are the rule of the ssa command for k = 0: all
	// components up to this window (the shorter side of the trajectory
	// matrix), the 30 leading ones by the randomized method above it.
	autoFullMax = 300
	autoK       = 30
	// spectrumShow is the number of leading singular values reported.
	spectrumShow = 20
)

// Forecast methods.
const (
	MethodL = "L"
	MethodK = "K"
)

var (
	uniKeys   = []string{"window", "step", "k", "iters", "groups", "forecast", "method", "clip", "min_points", "max_gaps", "reconstruction", "min_delta", "min_rel_delta"}
	multiKeys = append(slices.Clone(uniKeys), "normalize")
)

// bounds is a clip range.
type bounds struct{ lo, hi float64 }

type settings struct {
	window   processor.Span // unset = n/3
	step     float64        // seconds; 0 = infer per series
	k, iters int
	groups   [][]int // nil = the suggested groups
	forecast processor.Span
	method   string
	// clipAll applies to every dim; clipDim (ssa_mv) names dims.
	clipAll        *bounds
	clipDim        map[string]bounds
	minPoints      int
	maxGaps        float64
	reconstruction bool
	normalize      bool
	minDelta       processor.DimFloats
	minRelDelta    float64
}

// parseSettings reads every key. At load time a value that is still a
// template is skipped; at run time everything must be a final value.
func parseSettings(with map[string]any, load, multi bool) (*settings, error) {
	keys := uniKeys
	if multi {
		keys = multiKeys
	}
	if err := processor.CheckKeys(with, keys...); err != nil {
		return nil, err
	}
	st := &settings{iters: DefaultIters, method: MethodL, minPoints: DefaultMinPoints, maxGaps: DefaultMaxGaps, normalize: true}
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }
	var err error
	if !deferred("window") {
		if st.window, err = processor.SpanOf(with, "window"); err != nil {
			return nil, err
		}
		if st.window.Points == 1 {
			return nil, fmt.Errorf("with.window: must be at least 2 points, got 1")
		}
	}
	if !deferred("step") {
		if st.step, err = processor.Seconds(with, "step", 0); err != nil {
			return nil, err
		}
		if v, ok := with["step"]; ok && v != nil && st.step <= 0 {
			return nil, fmt.Errorf("with.step: must be > 0 seconds, got %v", st.step)
		}
	}
	if !deferred("k") {
		if st.k, err = processor.Int(with, "k", 0); err != nil {
			return nil, err
		}
		if st.k < 0 {
			return nil, fmt.Errorf("with.k: must be >= 0 (0 = all components up to a window of %d, %d above), got %d", autoFullMax, autoK, st.k)
		}
	}
	if !deferred("iters") {
		if st.iters, err = processor.Int(with, "iters", DefaultIters); err != nil {
			return nil, err
		}
		if st.iters < 0 {
			return nil, fmt.Errorf("with.iters: must be >= 0, got %d", st.iters)
		}
	}
	if !deferred("groups") {
		spec, err := processor.String(with, "groups")
		if err != nil {
			return nil, err
		}
		if st.groups, err = ssa.ParseGroups(spec); err != nil {
			return nil, fmt.Errorf("with.groups: %w", err)
		}
	}
	if !deferred("forecast") {
		if st.forecast, err = processor.SpanOf(with, "forecast"); err != nil {
			return nil, err
		}
	}
	if !deferred("method") {
		m, err := processor.String(with, "method")
		if err != nil {
			return nil, err
		}
		switch strings.ToUpper(strings.TrimSpace(m)) {
		case "", MethodL:
			st.method = MethodL
		case MethodK:
			st.method = MethodK
		default:
			return nil, fmt.Errorf("with.method: must be L (recurrent forecast) or K (vector forecast), got %q", m)
		}
	}
	if err := st.parseClip(with, load, multi); err != nil {
		return nil, err
	}
	if !deferred("min_points") {
		if st.minPoints, err = processor.Int(with, "min_points", DefaultMinPoints); err != nil {
			return nil, err
		}
		if st.minPoints < 3 {
			return nil, fmt.Errorf("with.min_points: must be >= 3, got %d", st.minPoints)
		}
	}
	if !deferred("max_gaps") {
		if st.maxGaps, err = processor.Float(with, "max_gaps", DefaultMaxGaps); err != nil {
			return nil, err
		}
		if st.maxGaps < 0 || st.maxGaps > 1 {
			return nil, fmt.Errorf("with.max_gaps: must be a share of the grid in [0, 1], got %v", st.maxGaps)
		}
	}
	if !deferred("reconstruction") {
		if st.reconstruction, err = processor.Bool(with, "reconstruction", false); err != nil {
			return nil, err
		}
	}
	if _, isObj := with["min_delta"].(map[string]any); isObj && !multi {
		return nil, fmt.Errorf("with.min_delta: must be a number, got object (per-dim values are for ssa_mv)")
	}
	if st.minDelta, err = processor.ParseDimFloats(with, "min_delta", load); err != nil {
		return nil, err
	}
	if !deferred("min_rel_delta") {
		if st.minRelDelta, err = processor.Float(with, "min_rel_delta", 0); err != nil {
			return nil, err
		}
		if st.minRelDelta < 0 {
			return nil, fmt.Errorf("with.min_rel_delta: must be >= 0 (0 = off), got %v", st.minRelDelta)
		}
	}
	if !multi {
		return st, nil
	}
	if !deferred("normalize") {
		if st.normalize, err = processor.Bool(with, "normalize", true); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// parseClip reads with.clip: [lo, hi] or "lo,hi" for every dim, and in
// ssa_mv also an object {dim: [lo, hi] | "lo,hi"}.
func (st *settings) parseClip(with map[string]any, load, multi bool) error {
	v, ok := with["clip"]
	if !ok || v == nil || (load && processor.IsTemplate(v)) {
		return nil
	}
	if obj, isObj := v.(map[string]any); isObj {
		if !multi {
			return fmt.Errorf("with.clip: must be [lo, hi] or \"lo,hi\", got object")
		}
		st.clipDim = make(map[string]bounds, len(obj))
		for dim, x := range obj {
			if load && processor.IsTemplate(x) {
				continue
			}
			b, err := parseBounds("clip."+dim, x)
			if err != nil {
				return err
			}
			st.clipDim[dim] = b
		}
		return nil
	}
	b, err := parseBounds("clip", v)
	if err != nil {
		return err
	}
	st.clipAll = &b
	return nil
}

func parseBounds(key string, v any) (bounds, error) {
	var parts []any
	switch x := v.(type) {
	case string:
		for _, p := range strings.Split(x, ",") {
			parts = append(parts, strings.TrimSpace(p))
		}
	case []any:
		parts = x
	default:
		return bounds{}, fmt.Errorf("with.%s: must be [lo, hi] or \"lo,hi\", got %s", key, processor.JSONType(v))
	}
	if len(parts) != 2 {
		return bounds{}, fmt.Errorf("with.%s: must be [lo, hi], got %d value(s)", key, len(parts))
	}
	var b [2]float64
	for i, p := range parts {
		f, err := processor.Float(map[string]any{key: p}, key, 0)
		if err != nil {
			return bounds{}, err
		}
		b[i] = f
	}
	if b[0] >= b[1] {
		return bounds{}, fmt.Errorf("with.%s: want lo < hi, got [%s, %s]", key, strconv.FormatFloat(b[0], 'g', -1, 64), strconv.FormatFloat(b[1], 'g', -1, 64))
	}
	return bounds{b[0], b[1]}, nil
}

// clipFor returns the clip range of dim, if any.
func (st *settings) clipFor(dim string) (bounds, bool) {
	if b, ok := st.clipDim[dim]; ok {
		return b, true
	}
	if st.clipAll != nil {
		return *st.clipAll, true
	}
	return bounds{}, false
}

// checkDims reports clip and min_delta members that name no dim.
func (st *settings) checkDims(dims []string) error {
	if err := st.minDelta.Check("min_delta", dims); err != nil {
		return err
	}
	var unknown []string
	for dim := range st.clipDim {
		if !slices.Contains(dims, dim) {
			unknown = append(unknown, dim)
		}
	}
	if unknown != nil {
		slices.Sort(unknown)
		return fmt.Errorf("with.clip: unknown dimension(s) %s (dims: %s)", strings.Join(unknown, ", "), strings.Join(dims, ", "))
	}
	return nil
}
