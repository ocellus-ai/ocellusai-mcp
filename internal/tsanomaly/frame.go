package tsanomaly

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Series is one raw metric as returned by a TSDB: possibly irregular
// timestamps (unix seconds) with gaps.
type Series struct {
	Name       string
	Labels     map[string]string
	Timestamps []int64
	Values     []float64
}

// Key renders the series as a Prometheus-style selector, e.g.
// cpu_usage{instance="a",job="node"}.
func (s Series) Key() string {
	if len(s.Labels) == 0 {
		return s.Name
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, s.Labels[k])
	}
	return s.Name + "{" + strings.Join(parts, ",") + "}"
}

// Frame is a set of series aligned to one regular time grid. Cols[j] is the
// column for Names[j]; missing samples are NaN (detectors impute them).
type Frame struct {
	Timestamps []int64
	Names      []string
	Cols       [][]float64
}

func (f *Frame) Len() int { return len(f.Timestamps) }

// Align resamples every series onto the grid start, start+step, ..., end
// (unix seconds). Each grid slot takes the last sample whose timestamp falls
// in (slot-step, slot]; empty slots become NaN. Series that are entirely
// empty on the grid are dropped.
func Align(series []Series, start, end, step int64) (*Frame, error) {
	if step <= 0 || end < start {
		return nil, fmt.Errorf("align: bad grid (start=%d end=%d step=%d)", start, end, step)
	}
	n := int((end-start)/step) + 1
	ts := make([]int64, n)
	for i := range ts {
		ts[i] = start + int64(i)*step
	}
	f := &Frame{Timestamps: ts}
	for _, s := range series {
		col := make([]float64, n)
		for i := range col {
			col[i] = math.NaN()
		}
		seen := false
		for i, t := range s.Timestamps {
			if t < start-step+1 || t > end {
				continue
			}
			slot := int((t - start + step - 1) / step) // ceil
			if slot < 0 || slot >= n {
				continue
			}
			col[slot] = s.Values[i]
			seen = true
		}
		if !seen {
			continue
		}
		f.Names = append(f.Names, s.Key())
		f.Cols = append(f.Cols, col)
	}
	if len(f.Cols) == 0 {
		return nil, fmt.Errorf("align: no series had samples on the grid")
	}
	return f, nil
}

// DropFlat removes columns whose values are (almost) constant; such metrics
// carry no signal and only add noise to multivariate detectors.
func (f *Frame) DropFlat() *Frame {
	out := &Frame{Timestamps: f.Timestamps}
	for j, c := range f.Cols {
		y := Impute(c)
		_, mad := MAD(y)
		mn, mx := math.Inf(1), math.Inf(-1)
		for _, v := range y {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if mad <= 1e-12 && mx-mn <= 1e-12 {
			continue
		}
		out.Names = append(out.Names, f.Names[j])
		out.Cols = append(out.Cols, c)
	}
	return out
}
