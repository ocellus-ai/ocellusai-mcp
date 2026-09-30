package series

import (
	"math"
	"strings"
	"testing"
)

func TestRegular(t *testing.T) {
	var pts []MultiPoint
	for _, i := range []int{4, 0, 1, 3} { // out of order, slot 2 missing
		pts = append(pts, MultiPoint{T: baseTS + 0.25 + 15*float64(i), V: []float64{float64(i), -float64(i)}})
	}
	g, reason := Regular(pts, 2, 0)
	if reason != "" {
		t.Fatal(reason)
	}
	if g.Step != 15 || g.Gaps != 1 || g.Len() != 5 || g.Times[2] != baseTS+0.25+30 {
		t.Errorf("grid = step %v gaps %d len %d times[2] %v", g.Step, g.Gaps, g.Len(), g.Times[2])
	}
	if !math.IsNaN(g.Cols[0][2]) || !math.IsNaN(g.Cols[1][2]) || g.Cols[0][3] != 3 || g.Cols[1][4] != -4 {
		t.Errorf("cols = %v", g.Cols)
	}
	if pts[0].T != baseTS+0.25+60 {
		t.Error("the input must not be reordered")
	}
	// An explicit step wider than the data merges samples into slots.
	if g, reason = Regular(pts, 2, 30); reason != "" || g.Len() != 3 || g.Gaps != 0 {
		t.Errorf("step 30: %q len %d gaps %d", reason, g.Len(), g.Gaps)
	}
	for name, tc := range map[string]struct {
		pts  []MultiPoint
		step float64
		want string
	}{
		"empty":     {nil, 0, "no samples"},
		"one time":  {[]MultiPoint{{T: 1, V: []float64{1}}, {T: 1, V: []float64{2}}}, 0, "cannot infer the step"},
		"irregular": {[]MultiPoint{{T: 0, V: []float64{1}}, {T: 1, V: []float64{1}}, {T: 100, V: []float64{1}}}, 0, "not on a regular grid"},
	} {
		if _, reason := Regular(tc.pts, 1, tc.step); !strings.Contains(reason, tc.want) {
			t.Errorf("%s: reason %q, want %q", name, reason, tc.want)
		}
	}
	sp := ScalarPoints([]Point{{T: 1, V: 2}, {T: 2, V: 3}})
	if len(sp) != 2 || sp[1].T != 2 || len(sp[1].V) != 1 || sp[1].V[0] != 3 {
		t.Errorf("ScalarPoints = %v", sp)
	}
}
