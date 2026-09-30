// Package join is the `join` processor: a transform that turns several
// scalar series sets (Prometheus matrices, one per named call) into one
// multivariate series per object by aligning the inputs by label and by
// timestamp. It is the step that feeds anomaly_mv and any other processor
// working on multivariate series, so none of them has to know about calls
// or carry its own join.
//
// Input: the object a calls: tool hands to process ({name: result}); every
// input named in with.inputs must be a Prometheus range result (matrix; see
// package series). A tool without calls can still build that object with a
// preceding fn: jq step.
//
// with:
//
//	inputs   call names, required (a bare string is a list of one); they
//	         become the dims of the output, in this order.
//	join_by  label name(s), required: series of different inputs with equal
//	         values of these labels describe the same object.
//
// Output: the multivariate series form of package series,
//
//	{"dims": inputs, "series": [{"labels": {join_by labels}, "points": [[ts, v...], ...],
//	                             "input_points": {name: n}, "reason": "..."}, ...]}
//
// with one series per join key seen in any input. points are the samples
// present in every input at exactly the same timestamp (inner join; the
// first input drives the order), so the calls should share start, end and
// step. input_points tells how many samples each input had before the join.
// An object that could not be joined is kept with no points and a reason,
// so a following analysis reports it as skipped instead of losing it: a
// series lacking a join_by label ("input mem: missing join_by label(s)
// instance", listed first, with its own labels) and a join key absent from
// some input ("missing in input(s) cpu"). Two series of one input sharing a
// join key are an error: aggregate the query to one series per object.
package join

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: join).
const Name = "join"

// Processor implements processor.Processor.
type Processor struct{}

// New creates the join processor.
func New() *Processor { return &Processor{} }

// Name returns "join".
func (*Processor) Name() string { return Name }

type settings struct {
	inputs []string
	joinBy []string
}

// Validate checks the raw `with` section at catalog load time. Lists that
// still contain templates are only checked in Run.
func (*Processor) Validate(with map[string]any) error {
	_, err := parseSettings(with, true)
	return err
}

// parseSettings reads inputs and join_by. Both are required; at load time a
// list with a templated element is accepted as is and checked after rendering.
func parseSettings(with map[string]any, load bool) (*settings, error) {
	if err := processor.CheckKeys(with, "inputs", "join_by"); err != nil {
		return nil, err
	}
	st := &settings{}
	var err error
	if st.inputs, err = stringList(with, "inputs", load, "the call names to join, e.g. [cpu, mem]"); err != nil {
		return nil, err
	}
	if st.joinBy, err = stringList(with, "join_by", load, "the labels to join the inputs by, e.g. [instance]"); err != nil {
		return nil, err
	}
	return st, nil
}

// stringList reads a required list of non-empty, distinct strings; a bare
// string is a one-element list. At load time an element that is still a
// template makes the whole list deferred (nil without error).
func stringList(with map[string]any, key string, load bool, hint string) ([]string, error) {
	v, ok := with[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("with.%s: required (%s)", key, hint)
	}
	var items []any
	switch x := v.(type) {
	case string:
		items = []any{x}
	case []any:
		items = x
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("with.%s: must be a list of strings, got %s", key, processor.JSONType(v))
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("with.%s[%d]: must be a string, got %s", key, i, processor.JSONType(it))
		}
		if processor.IsTemplate(s) {
			if load {
				return nil, nil
			}
			return nil, fmt.Errorf("with.%s[%d]: unrendered template %q", key, i, s)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("with.%s[%d]: must not be empty", key, i)
		}
		if seen[s] {
			return nil, fmt.Errorf("with.%s: duplicate %q", key, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("with.%s: must not be empty (%s)", key, hint)
	}
	return out, nil
}

// indexedInput is one input's series indexed by join key.
type indexedInput struct {
	name   string
	byKey  map[string]*series.Series
	unjoin []series.Series // series lacking a join_by label
}

// group is one object (join key) across the inputs.
type group struct {
	labels map[string]string
	// byInput holds the series of every input for this key; nil when the
	// input has no series with these labels.
	byInput []*series.Series
}

// Run joins the listed inputs and returns the multivariate series form.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	obj, ok := data.(map[string]any)
	if !ok {
		shape := make([]string, len(st.inputs))
		for i, name := range st.inputs {
			shape[i] = name + ": matrix"
		}
		return nil, fmt.Errorf("expected an object of call results {%s} (a calls: tool, or a preceding fn: jq step that builds it), got %s", strings.Join(shape, ", "), processor.JSONType(data))
	}
	inputs := make([]indexedInput, len(st.inputs))
	for i, name := range st.inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, ok := obj[name]
		if !ok {
			return nil, fmt.Errorf("input %s: no such call result (available: %s)", name, strings.Join(sortedKeys(obj), ", "))
		}
		all, err := series.ParseMatrix(raw)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		inputs[i], err = indexInput(name, all, st.joinBy)
		if err != nil {
			return nil, err
		}
	}

	out := series.Multi{Dims: st.inputs}
	var extra []map[string]any
	for _, in := range inputs {
		for _, s := range in.unjoin {
			out.Series = append(out.Series, series.MultiSeries{
				Labels: s.Labels,
				Dims:   st.inputs,
				Reason: fmt.Sprintf("input %s: missing join_by label(s) %s", in.name, strings.Join(missingLabels(s.Labels, st.joinBy), ", ")),
			})
			extra = append(extra, map[string]any{"input_points": map[string]any{in.name: len(s.Points)}})
		}
	}
	for _, g := range joinGroups(inputs, st.joinBy) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ms := series.MultiSeries{Labels: g.labels, Dims: st.inputs}
		var absent []string
		inputPoints := make(map[string]any, len(inputs))
		for i, s := range g.byInput {
			if s == nil {
				absent = append(absent, inputs[i].name)
				inputPoints[inputs[i].name] = 0
				continue
			}
			inputPoints[inputs[i].name] = len(s.Points)
		}
		if len(absent) > 0 {
			ms.Reason = fmt.Sprintf("missing in input(s) %s", strings.Join(absent, ", "))
		} else {
			ms.Points = joinPoints(g.byInput)
		}
		out.Series = append(out.Series, ms)
		extra = append(extra, map[string]any{"input_points": inputPoints})
	}
	return series.ToMulti(out, extra), nil
}

// indexInput maps the series of one input by join key. Two series with the
// same key are an error: the query must aggregate to one series per object.
func indexInput(name string, all []series.Series, joinBy []string) (indexedInput, error) {
	in := indexedInput{name: name, byKey: make(map[string]*series.Series, len(all))}
	first := make(map[string]int, len(all))
	for i := range all {
		s := &all[i]
		key, ok := joinKey(s.Labels, joinBy)
		if !ok {
			in.unjoin = append(in.unjoin, *s)
			continue
		}
		if j, dup := first[key]; dup {
			return in, fmt.Errorf("input %s: result[%d] and result[%d] share the join key %s; aggregate the query to one series per object (e.g. sum by (%s) in PromQL)",
				name, j, i, describeKey(s.Labels, joinBy), strings.Join(joinBy, ", "))
		}
		first[key] = i
		in.byKey[key] = s
	}
	return in, nil
}

// joinGroups lists every join key seen in any input, in sorted key order,
// with the matching series of each input.
func joinGroups(inputs []indexedInput, joinBy []string) []group {
	var groups []group
	index := make(map[string]int)
	for i, in := range inputs {
		keys := make([]string, 0, len(in.byKey))
		for k := range in.byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := in.byKey[k]
			gi, ok := index[k]
			if !ok {
				gi = len(groups)
				index[k] = gi
				labels := make(map[string]string, len(joinBy))
				for _, l := range joinBy {
					labels[l] = s.Labels[l]
				}
				groups = append(groups, group{labels: labels, byInput: make([]*series.Series, len(inputs))})
			}
			groups[gi].byInput[i] = s
		}
	}
	return groups
}

// joinPoints inner-joins the inputs' samples by timestamp. The first input
// drives the order; a timestamp missing in any other input is dropped.
func joinPoints(all []*series.Series) []series.MultiPoint {
	lookups := make([]map[float64]float64, len(all)-1)
	for i, s := range all[1:] {
		m := make(map[float64]float64, len(s.Points))
		for _, p := range s.Points {
			m[p.T] = p.V
		}
		lookups[i] = m
	}
	out := make([]series.MultiPoint, 0, len(all[0].Points))
	for _, p := range all[0].Points {
		v := make([]float64, len(all))
		v[0] = p.V
		complete := true
		for i, m := range lookups {
			x, found := m[p.T]
			if !found {
				complete = false
				break
			}
			v[i+1] = x
		}
		if complete {
			out = append(out, series.MultiPoint{T: p.T, V: v})
		}
	}
	return out
}

func joinKey(labels map[string]string, joinBy []string) (string, bool) {
	parts := make([]string, 0, len(joinBy))
	for _, l := range joinBy {
		v, ok := labels[l]
		if !ok {
			return "", false
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, "\x1f"), true
}

func describeKey(labels map[string]string, joinBy []string) string {
	parts := make([]string, 0, len(joinBy))
	for _, l := range joinBy {
		parts = append(parts, fmt.Sprintf("%s=%q", l, labels[l]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func missingLabels(labels map[string]string, joinBy []string) []string {
	var out []string
	for _, l := range joinBy {
		if _, ok := labels[l]; !ok {
			out = append(out, l)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
