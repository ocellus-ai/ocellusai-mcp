package tsanomaly

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// synthPlateau is flat noise (sigma 1) with a four-point ramp into a
// plateau at +8 over [start, end] (26 points, a CPU saturation seen on a
// production fleet), the shape the AR residual misses and the level
// detector is for. With a window of 60 the trailing median starts to catch
// up after 30 elevated points, so the whole plateau stays scored.
func synthPlateau(seed int64) (x []float64, start, end int) {
	const n = 1440
	rng := rand.New(rand.NewSource(seed))
	x = make([]float64, n)
	for t := range x {
		x[t] = 10 + rng.NormFloat64()
	}
	start, end = 700, 725
	for k := 1; k <= 4; k++ { // 696..699: 11.6, 13.2, 14.8, 16.4
		x[start-5+k] = 10 + 8*float64(k)/5
	}
	for t := start; t <= end; t++ {
		x[t] = 18 + rng.NormFloat64()
	}
	return x, start, end
}

func TestLevelShiftPlateau(t *testing.T) {
	x, start, end := synthPlateau(1)
	sc, err := (&LevelShift{Window: 60}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	for i := start; i <= end; i++ {
		if sc[i] <= 4 {
			t.Errorf("plateau point %d scored %.2f, want > 4", i, sc[i])
		}
	}
	// the median catches up Window/2 points after the plateau ends; outside
	// that stretch flat noise must stay under the floor
	for i := range sc {
		if (i < start-4 || i > end+30) && sc[i] > 4.5 {
			t.Errorf("noise point %d scored %.2f", i, sc[i])
		}
	}
	// a short window catches up half-way through the plateau
	sc, err = (&LevelShift{Window: 20}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if sc[start+2] <= 4 || sc[end] >= 2 {
		t.Errorf("window 20: start+2 %.2f (want > 4), end %.2f (want < 2)", sc[start+2], sc[end])
	}
	if !isCalibrated(&LevelShift{}) || isSubsequence(&LevelShift{}) || (&LevelShift{Window: 60}).Name() != "level(w=60)" {
		t.Error("markers or name: level is calibrated and point-wise (POT), not window-spread")
	}
}

// TestLevelShiftAR documents why the detector exists: on the same plateau
// the AR residual fires on the ramp at most, never on the level itself.
func TestLevelShiftAR(t *testing.T) {
	x, start, end := synthPlateau(2)
	ar, err := (&ARResidual{Order: 3}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	if m := maxIn(ar, start+6, end); m > 4 {
		t.Errorf("AR scored the plateau interior %.2f, expected the ramp only", m)
	}
	lv, _ := (&LevelShift{Window: 60}).Score(x)
	if m := maxIn(lv, start+6, end); m <= 4 {
		t.Errorf("level scored the plateau interior %.2f, want > 4", m)
	}
}

// The rolling median must equal the brute-force median of the trailing
// window at every point.
func TestLevelShiftRollingMedian(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	n, w := 500, 7
	x := make([]float64, n)
	for i := range x {
		x[i] = rng.NormFloat64()
	}
	x[100], x[101] = x[50], x[50] // duplicates must be removed one at a time
	got, err := (&LevelShift{Window: w, MinPoints: 1}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	resid := make([]float64, n)
	for i := 1; i < n; i++ {
		resid[i] = x[i] - Median(x[maxInt(0, i-w):i])
	}
	med, mad := MAD(resid[1:])
	for i := 1; i < n; i++ {
		want := math.Abs(resid[i]-med) / mad
		if math.Abs(got[i]-want) > 1e-9 {
			t.Fatalf("i=%d: score %.6f, brute force %.6f", i, got[i], want)
		}
	}
	if got[0] != 0 {
		t.Errorf("the first point has no window and must score 0, got %v", got[0])
	}
}

// A gap is neither a shift nor hidden by one: imputed points score 0 and
// the points after the gap are scored against the real samples before it.
func TestLevelShiftGap(t *testing.T) {
	x, _, _, _ := synthUni(11)
	for i := 500; i < 561; i++ {
		x[i] = math.NaN()
	}
	sc, err := (&LevelShift{Window: 100}).Score(x)
	if err != nil {
		t.Fatal(err)
	}
	for i := 480; i < 620; i++ {
		if sc[i] > 4 {
			t.Errorf("point %d near the gap scored %.2f", i, sc[i])
		}
		if i >= 500 && i < 561 && sc[i] != 0 {
			t.Errorf("imputed point %d scored %v, want 0", i, sc[i])
		}
	}
	// a plateau reached right after the gap is still a shift against the pre-gap level
	for i := 561; i < 600; i++ {
		x[i] = 40
	}
	sc, _ = (&LevelShift{Window: 100}).Score(x)
	if m := maxIn(sc, 561, 590); m <= 4 {
		t.Errorf("shift after the gap scored %.2f, want > 4", m)
	}
}

// A long plateau is flagged at its edges (the climb and the return), never
// strictly inside once the trailing median has caught up. The plateau is
// +20 sigma so that it clears the POT cap whatever the fit does.
func TestLevelShiftPlateauEdges(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	x := make([]float64, 2000)
	for i := range x {
		x[i] = rng.NormFloat64()
	}
	for i := 1500; i < 1700; i++ {
		x[i] = 20 + rng.NormFloat64()
	}
	ens, err := EnsembleUni(x, CombineMaxZ, &LevelShift{Window: 100})
	if err != nil {
		t.Fatal(err)
	}
	fl, thr := ens.Flags(ThresholdPolicy{})
	if th := thr["level(w=100)"]; th < 4 || th > 13 { // cap = 3 * 3.72 * width at risk 1e-4
		t.Errorf("a point-wise detector gets a POT threshold between the floor and the cap, got %v", th)
	}
	rs := Ranges(fl, ens.Combined, 1, 10)
	climb, inside := false, false
	for _, r := range rs {
		if r.Start <= 1505 && r.End >= 1505 {
			climb = true
		}
		if r.Start > 1560 && r.End < 1680 {
			inside = true
		}
	}
	if !climb || inside {
		t.Fatalf("climb=%v inside=%v ranges=%v", climb, inside, rs)
	}
}

func TestLevelShiftErrors(t *testing.T) {
	x := []float64{1, 2, 3, 4, 5}
	for name, d := range map[string]*LevelShift{"window 1": {Window: 1}, "window >= n": {Window: 5}} {
		if _, err := d.Score(x); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := (&LevelShift{Window: 2}).Score([]float64{1, 2, 3}); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("n=3: %v", err)
	}
	if _, err := (&LevelShift{Window: 2}).Score([]float64{1, math.NaN(), math.NaN(), math.NaN(), 2}); err == nil {
		t.Error("nothing to score: expected an error")
	}
}

func TestAnalyzeIncludesLevel(t *testing.T) {
	x, _, _, _ := synthUni(5)
	f := &Frame{Timestamps: make([]int64, len(x)), Names: []string{"m"}, Cols: [][]float64{x}}
	for i := range f.Timestamps {
		f.Timestamps[i] = int64(i) * 60
	}
	rep, err := Analyze(f, Options{Window: 50, SkipIForest: true})
	if err != nil {
		t.Fatal(err)
	}
	has := func(r *Report) bool {
		for _, p := range r.Metrics[0].Ensemble.Parts {
			if p.Detector == "level(w=50)" {
				return true
			}
		}
		return false
	}
	if !has(rep) {
		t.Errorf("default ensemble lacks the level detector: %v", rep.Metrics[0].Ensemble.Parts)
	}
	rep, err = Analyze(f, Options{Window: 50, SkipIForest: true, SkipLevel: true})
	if err != nil {
		t.Fatal(err)
	}
	if has(rep) {
		t.Error("SkipLevel did not remove the level detector")
	}
}
