// Package series defines the two canonical time-series forms that processors
// exchange, and the Go types they are parsed into. Every processor that
// works on time series accepts exactly one of these forms; whatever a worker
// returns is brought into the form by a preceding fn: jq step, and a
// transform (series in, series out) hands the same form on to the next step.
//
// # Scalar series: the Prometheus matrix
//
// A set of labelled series with one value per sample travels as a Prometheus
// range result, the matrix of the Prometheus HTTP API:
//
//	{"resultType": "matrix",
//	 "result": [{"metric": {"pod": "api-1"}, "values": [[1757600000, "0.25"], ...]}, ...]}
//
// This is the wire format of Prometheus, VictoriaMetrics, Thanos, Mimir and
// Loki metric queries alike, so the prometheus worker produces it directly.
// Values are accepted both as numeric strings (the API format) and as JSON
// numbers; NaN and ±Inf samples are dropped. ParseMatrix reads it, ToMatrix
// writes it back (values as strings, like the API).
//
// # Multivariate series: the neutral form
//
// A set of labelled series with a vector of values per sample has no
// Prometheus equivalent and travels in this form:
//
//	{"dims": ["cpu", "mem"],
//	 "series": [{"labels": {"instance": "vm-1"},
//	             "points": [[1757600000, 0.30, 0.70], ...],
//	             "reason": "..."}, ...]}
//
// dims names the vector components in order; every point is a timestamp
// followed by exactly len(dims) values (JSON numbers, numeric strings are
// accepted too; a point with a non-finite component is dropped). A series
// may carry a reason: a transform that could not build it (a join whose
// input is missing) leaves the explanation there with an empty points list,
// and an analysis reports the series as skipped with that reason instead of
// silently losing the object. Unknown keys are ignored, so a transform may
// add diagnostics (join adds input_points) next to the standard fields.
// ParseMulti reads the form, ToMulti writes it.
package series

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// Point is one sample of a scalar series.
type Point struct {
	T float64 // unix seconds
	V float64
}

// Series is one scalar time series: finite samples in the order received.
type Series struct {
	Labels map[string]string
	Points []Point
}

// MultiPoint is one sample of a multivariate series: the values of every
// dimension at one timestamp.
type MultiPoint struct {
	T float64 // unix seconds
	// V[i] is the value of dimension Dims[i]; len(V) == len(Dims).
	V []float64
}

// MultiSeries is one multivariate time series.
type MultiSeries struct {
	Labels map[string]string
	// Dims are the dimension names shared by every series of a Multi; they
	// are repeated here so a detector can name what it reports.
	Dims   []string
	Points []MultiPoint
	// Reason, when non-empty, explains why the series has no usable points
	// (set by the transform that failed to build it). An analysis reports
	// such a series as skipped with this reason.
	Reason string
}

// Multi is the parsed multivariate form: the dimension names and the series.
type Multi struct {
	Dims   []string
	Series []MultiSeries
}

// ParseMatrix reads the scalar form (a Prometheus range result).
func ParseMatrix(data any) ([]Series, error) {
	m, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a Prometheus range result {resultType: matrix, result: [...]} (a range query, or a preceding fn: jq step that builds this shape), got %s", processor.JSONType(data))
	}
	rt, _ := m["resultType"].(string)
	if rt != "matrix" {
		return nil, fmt.Errorf("expected resultType \"matrix\" (a range query: request.type: range; other sources are shaped by a preceding fn: jq step), got %q", rt)
	}
	var items []any
	if m["result"] != nil {
		items, ok = m["result"].([]any)
		if !ok {
			return nil, fmt.Errorf("result: expected array, got %s", processor.JSONType(m["result"]))
		}
	}
	out := make([]Series, 0, len(items))
	for i, item := range items {
		im, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("result[%d]: expected object, got %s", i, processor.JSONType(item))
		}
		s := Series{Labels: map[string]string{}}
		if metric, ok := im["metric"].(map[string]any); ok {
			for k, v := range metric {
				s.Labels[k] = stringify(v)
			}
		}
		var raw []any
		if im["values"] != nil {
			raw, ok = im["values"].([]any)
			if !ok {
				return nil, fmt.Errorf("result[%d].values: expected array, got %s", i, processor.JSONType(im["values"]))
			}
		}
		s.Points = make([]Point, 0, len(raw))
		for j, sample := range raw {
			pair, ok := sample.([]any)
			if !ok || len(pair) != 2 {
				return nil, fmt.Errorf("result[%d].values[%d]: expected [timestamp, value]", i, j)
			}
			t, err := processor.Number(pair[0])
			if err != nil {
				return nil, fmt.Errorf("result[%d].values[%d]: timestamp: %w", i, j, err)
			}
			v, err := processor.Number(pair[1])
			if err != nil {
				return nil, fmt.Errorf("result[%d].values[%d]: value: %w", i, j, err)
			}
			if !finite(t) || !finite(v) {
				continue
			}
			s.Points = append(s.Points, Point{T: t, V: v})
		}
		out = append(out, s)
	}
	return out, nil
}

// ToMatrix writes series in the scalar form. Values are formatted as strings,
// as the Prometheus HTTP API does, so jq expressions written for the worker
// result keep working after a transform.
func ToMatrix(series []Series) map[string]any {
	result := make([]any, 0, len(series))
	for _, s := range series {
		values := make([]any, 0, len(s.Points))
		for _, p := range s.Points {
			values = append(values, []any{p.T, strconv.FormatFloat(p.V, 'g', -1, 64)})
		}
		result = append(result, map[string]any{"metric": LabelsToAny(s.Labels), "values": values})
	}
	return map[string]any{"resultType": "matrix", "result": result}
}

// ParseMulti reads the multivariate form.
func ParseMulti(data any) (Multi, error) {
	m, ok := data.(map[string]any)
	if !ok {
		return Multi{}, fmt.Errorf("expected a multivariate series object {dims: [...], series: [...]} (the output of fn: join), got %s", processor.JSONType(data))
	}
	dims, err := parseDims(m["dims"])
	if err != nil {
		return Multi{}, err
	}
	var items []any
	if m["series"] != nil {
		items, ok = m["series"].([]any)
		if !ok {
			return Multi{}, fmt.Errorf("series: expected array, got %s", processor.JSONType(m["series"]))
		}
	}
	out := Multi{Dims: dims, Series: make([]MultiSeries, 0, len(items))}
	for i, item := range items {
		im, ok := item.(map[string]any)
		if !ok {
			return Multi{}, fmt.Errorf("series[%d]: expected object, got %s", i, processor.JSONType(item))
		}
		s := MultiSeries{Labels: map[string]string{}, Dims: dims}
		if im["labels"] != nil {
			lm, ok := im["labels"].(map[string]any)
			if !ok {
				return Multi{}, fmt.Errorf("series[%d].labels: expected object, got %s", i, processor.JSONType(im["labels"]))
			}
			for k, v := range lm {
				s.Labels[k] = stringify(v)
			}
		}
		if im["reason"] != nil {
			r, ok := im["reason"].(string)
			if !ok {
				return Multi{}, fmt.Errorf("series[%d].reason: expected string, got %s", i, processor.JSONType(im["reason"]))
			}
			s.Reason = r
		}
		var raw []any
		if im["points"] != nil {
			raw, ok = im["points"].([]any)
			if !ok {
				return Multi{}, fmt.Errorf("series[%d].points: expected array, got %s", i, processor.JSONType(im["points"]))
			}
		}
		s.Points = make([]MultiPoint, 0, len(raw))
		for j, sample := range raw {
			row, ok := sample.([]any)
			if !ok || len(row) != len(dims)+1 {
				return Multi{}, fmt.Errorf("series[%d].points[%d]: expected [timestamp, %s]", i, j, strings.Join(dims, ", "))
			}
			t, err := processor.Number(row[0])
			if err != nil {
				return Multi{}, fmt.Errorf("series[%d].points[%d]: timestamp: %w", i, j, err)
			}
			p := MultiPoint{T: t, V: make([]float64, len(dims))}
			keep := finite(t)
			for d, dim := range dims {
				v, err := processor.Number(row[d+1])
				if err != nil {
					return Multi{}, fmt.Errorf("series[%d].points[%d]: %s: %w", i, j, dim, err)
				}
				p.V[d] = v
				keep = keep && finite(v)
			}
			if keep {
				s.Points = append(s.Points, p)
			}
		}
		out.Series = append(out.Series, s)
	}
	return out, nil
}

func parseDims(v any) ([]string, error) {
	raw, ok := v.([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("dims: expected a non-empty list of dimension names, got %s", processor.JSONType(v))
	}
	dims := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, d := range raw {
		s, ok := d.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("dims[%d]: expected a non-empty string, got %s", i, processor.JSONType(d))
		}
		if seen[s] {
			return nil, fmt.Errorf("dims[%d]: duplicate dimension %q", i, s)
		}
		seen[s] = true
		dims = append(dims, s)
	}
	return dims, nil
}

// ToMulti writes m in the multivariate form. Every series gets its points
// (possibly empty) and, when set, its reason; extra holds additional keys
// per series (diagnostics such as input_points) and may be nil or shorter
// than m.Series.
func ToMulti(m Multi, extra []map[string]any) map[string]any {
	list := make([]any, 0, len(m.Series))
	for i, s := range m.Series {
		points := make([]any, 0, len(s.Points))
		for _, p := range s.Points {
			row := make([]any, 0, len(p.V)+1)
			row = append(row, p.T)
			for _, v := range p.V {
				row = append(row, v)
			}
			points = append(points, row)
		}
		entry := map[string]any{"labels": LabelsToAny(s.Labels), "points": points}
		if s.Reason != "" {
			entry["reason"] = s.Reason
		}
		if i < len(extra) {
			for k, v := range extra[i] {
				entry[k] = v
			}
		}
		list = append(list, entry)
	}
	dims := make([]any, 0, len(m.Dims))
	for _, d := range m.Dims {
		dims = append(dims, d)
	}
	return map[string]any{"dims": dims, "series": list}
}

// LabelsToAny converts labels to the JSON-compatible map used in reports.
func LabelsToAny(l map[string]string) map[string]any {
	out := make(map[string]any, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

// FormatTime renders a unix timestamp (seconds, possibly fractional) as RFC 3339 UTC.
func FormatTime(ts float64) string {
	sec := math.Floor(ts)
	return time.Unix(int64(sec), int64((ts-sec)*1e9)).UTC().Format(time.RFC3339)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
