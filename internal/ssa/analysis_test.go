package ssa

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"gonum.org/v1/gonum/stat"
)

// findingWith returns the first finding containing every part.
func findingWith(fs []Finding, parts ...string) (Finding, bool) {
	for _, f := range fs {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(f.Text, p)
		}
		if ok {
			return f, true
		}
	}
	return Finding{}, false
}

func TestAnalyzeTones(t *testing.T) {
	// A level with a drift, a daily cycle with two harmonics and white noise.
	x := tones(2016, 288, 0.4, 2)
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(5 * time.Minute)
	// The numbers of the ssa command's -report on the same series: the port
	// must not change the analysis.
	if a.Noise.Removed != 7 || math.Abs(a.Noise.Floor-29.6) > 0.05 || math.Abs(a.Noise.Channels[0].SD-0.4046) > 1e-4 {
		t.Errorf("noise = %+v", a.Noise)
	}
	want := []struct {
		comps  []int
		kind   string
		period float64
	}{{[]int{0}, KindTrend, 0}, {[]int{1, 2}, KindHarmonic, 288}, {[]int{3, 4}, KindHarmonic, 144}, {[]int{5, 6}, KindHarmonic, 96}}
	if len(a.Components) != len(want) {
		t.Fatalf("components = %+v", a.Components)
	}
	for i, w := range want {
		c := a.Components[i]
		if !slices.Equal(c.Comps, w.comps) || c.Kind != w.kind || math.Abs(c.Period-w.period) > 1 {
			t.Errorf("component %d = %+v, want %+v", i, c, w)
		}
	}
	if a.Components[1].Order != 1 || a.Components[2].Order != 2 || a.Components[3].Base != a.Components[1].Period {
		t.Errorf("harmonic family: %+v", a.Components[1:])
	}
	if !slices.Equal(a.Structure, seq(7)) || a.NoiseFrom != 7 {
		t.Errorf("structure %v, noise from %d", a.Structure, a.NoiseFrom)
	}
	if len(a.Coarse) != 2 || FormatGroups([][]int{a.Coarse[0].Comps, a.Coarse[1].Comps}) != "0;1-6" || !strings.Contains(a.Coarse[1].Label, "24.00 h, harmonics 1, 2, 3") {
		t.Errorf("coarse = %+v", a.Coarse)
	}
	if len(a.Fine) != 4 {
		t.Errorf("fine = %+v", a.Fine)
	}
	if f, ok := findingWith(a.Findings, "The window 288 points (1d) is 1 x the main period"); !ok || f.Level != LevelOK {
		t.Errorf("window finding missing: %+v", a.Findings)
	}
	// The daily pair carries most of the movement of the series.
	if sh := a.Components[1].Share[0]; sh < 0.7 || sh > 0.9 {
		t.Errorf("daily share = %v", sh)
	}

	ck, err := a.Check([][]int{{0}, seq(7)[1:]})
	if err != nil {
		t.Fatal(err)
	}
	if ck.Groups[0].Kind != GroupTrend || ck.Groups[1].Kind != GroupCycle || len(ck.Groups[1].Periods) != 3 {
		t.Errorf("group kinds: %s, %s %v", ck.Groups[0].Kind, ck.Groups[1].Kind, ck.Groups[1].Periods)
	}
	if ck.Separation > 0.1 || ck.ResidualCorr > 0.1 || ck.LeftOut != nil {
		t.Errorf("separation %v, residual %v, left out %v", ck.Separation, ck.ResidualCorr, ck.LeftOut)
	}
	if r := ck.Residual[0]; r.Explained < 0.99 || math.Abs(r.RobustSD-0.4046) > 1e-3 {
		t.Errorf("residual = %+v", r)
	}
	if g := ck.Groups[0].Channels[0]; math.Abs(g.Drift-7.22) > 0.01 || math.Abs(g.Mean-44.03) > 0.01 {
		t.Errorf("trend = %+v", g)
	}
	if g := ck.Groups[1].Channels[0]; math.Abs(g.PeakToPeak-19.8) > 0.05 {
		t.Errorf("cycle = %+v", g)
	}
	if _, ok := findingWith(ck.Findings, "Group 0 (trend): level 44.03, rises by 7.22"); !ok {
		t.Errorf("trend finding missing: %+v", ck.Findings)
	}
}

func TestAnalyzeTopK(t *testing.T) {
	s, err := Decompose(tones(4032, 288, 1, 4), 576, 30, 8)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(5 * time.Minute)
	if math.Abs(a.Noise.Floor-98.05) > 0.05 || !slices.Equal(a.Structure, seq(7)) {
		t.Errorf("floor %v, structure %v", a.Noise.Floor, a.Structure)
	}
	if f, ok := findingWith(a.Findings, "Top-k: err"); !ok || f.Level != LevelOK {
		t.Errorf("convergence finding missing: %+v", a.Findings)
	}
	// Too few components computed: the noise is not reached.
	s, err = Decompose(tones(4032, 288, 1, 4), 576, 5, 8)
	if err != nil {
		t.Fatal(err)
	}
	a = s.Analyze(0)
	if _, ok := findingWith(a.Findings, "Raise with.k"); !ok {
		t.Errorf("top-5: expected advice to raise k: %+v", a.Findings)
	}
}

func TestAnalyzeWindowAdvice(t *testing.T) {
	s, err := Decompose(tones(2016, 288, 1, 4), 250, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Without a time step: fit the window to the period found.
	a := s.Analyze(0)
	f, ok := findingWith(a.Findings, "The window 250 points is shorter than the main period", "Set with.window")
	if !ok || f.Level != LevelWarn {
		t.Errorf("window advice missing: %+v", a.Findings)
	}
	// At 5 minutes the period found (~24.5 h, biased by the window) is near a
	// day on a history of fewer than 7 cycles: the advice is the day, not the
	// biased period.
	a = s.Analyze(5 * time.Minute)
	f, ok = findingWith(a.Findings, "within 7% of the daily cycle 288 points (1d)", "set with.window to 288 points (1d)")
	if !ok || f.Level != LevelWarn {
		t.Errorf("daily advice missing: %+v", a.Findings)
	}
	if _, ok := findingWith(a.Findings, "shorter than the main period"); ok {
		t.Errorf("advice to fit the window to a biased period: %+v", a.Findings)
	}
}

func TestAnalyzeSaturation(t *testing.T) {
	x := tones(2016, 288, 1, 6)
	for i := range x {
		x[i] = min(x[i], 46)
	}
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(0)
	if a.Noise.Channels[0].AtMax == 0 {
		t.Fatalf("ceiling not found: %+v", a.Noise.Channels[0])
	}
	if f, ok := findingWith(a.Findings, "exactly at the maximum 46", "with.clip: [0, 46]"); !ok || f.Level != LevelWarn {
		t.Errorf("saturation finding missing: %+v", a.Findings)
	}
}

func TestAnalyzeDegenerate(t *testing.T) {
	zero, err := Decompose(make([]float64, 100), 30, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := zero.Analyze(0)
	if a.Components != nil || a.Structure != nil || len(a.Findings) != 1 || !strings.Contains(a.Findings[0].Text, "identically zero") {
		t.Errorf("zero series: %+v", a)
	}
	if _, err := a.Check([][]int{{0}}); err != nil {
		t.Errorf("zero series check: %v", err)
	}

	flat, err := Decompose(constant(100), 30, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a = flat.Analyze(0)
	if !a.Noise.Negligible || !slices.Equal(a.Structure, []int{0}) || a.Components[0].Kind != KindTrend {
		t.Errorf("constant series: noise %+v, components %+v", a.Noise, a.Components)
	}
	if _, ok := findingWith(a.Findings, "exactly at the"); ok {
		t.Errorf("a constant series does not saturate: %+v", a.Findings)
	}
	if f, ok := findingWith(a.Findings, "No saturation"); !ok || f.Level != LevelOK {
		t.Errorf("no-saturation finding missing: %+v", a.Findings)
	}
}

func TestCheckErrors(t *testing.T) {
	s, err := Decompose(sine(64, 8), 16, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(0)
	for name, g := range map[string][][]int{"none": nil, "empty group": {{}}, "out of range": {{16}}, "repeat": {{1, 1}}} {
		if _, err := a.Check(g); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAnalyzeMSSAOneChannelMatchesSSA(t *testing.T) {
	x := tones(1200, 120, 0.5, 7)
	s, err := Decompose(x, 240, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecomposeMulti([][]float64{x}, 240, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	as, am := s.Analyze(time.Minute), m.Analyze([]string{"cpu"}, time.Minute)
	if as.Noise.Removed != am.Noise.Removed || relDiff(am.Noise.Floor, as.Noise.Floor) > 1e-9 || !slices.Equal(as.Structure, am.Structure) {
		t.Errorf("noise ssa %+v mssa %+v", as.Noise, am.Noise)
	}
	if len(as.Components) != len(am.Components) {
		t.Fatalf("components ssa %d mssa %d", len(as.Components), len(am.Components))
	}
	for i := range as.Components {
		cs, cm := as.Components[i], am.Components[i]
		if !slices.Equal(cs.Comps, cm.Comps) || cs.Kind != cm.Kind || math.Abs(cs.Period-cm.Period) > 1e-6 {
			t.Errorf("component %d: ssa %+v mssa %+v", i, cs, cm)
		}
	}
	// The same findings, the MSSA ones naming the channel.
	if len(as.Findings) != len(am.Findings) {
		t.Errorf("findings ssa %d mssa %d", len(as.Findings), len(am.Findings))
	}
	if _, ok := findingWith(am.Findings, "Noise phi of cpu"); !ok {
		t.Errorf("mssa findings do not name the channel: %+v", am.Findings)
	}
}

// Two channels of very different scale: cpu (%) with a daily cycle and its
// own 8h cycle, net (bytes/s) with the daily cycle and a 12h harmonic.
func twoScales(n int) (cpu, net []float64) {
	rng := rand.New(rand.NewPCG(1, 2))
	cpu, net = make([]float64, n), make([]float64, n)
	for i := range n {
		tt := float64(i)
		cpu[i] = 30 + 10*math.Sin(2*math.Pi*tt/288) + 6*math.Sin(2*math.Pi*tt/96) + 2*rng.NormFloat64()
		net[i] = 2e8 + 5e7*math.Sin(2*math.Pi*tt/288+1) + 2e7*math.Sin(4*math.Pi*tt/288) + 1e7*rng.NormFloat64()
	}
	return cpu, net
}

func TestAnalyzeMSSAChannels(t *testing.T) {
	cpu, net := twoScales(2016)
	w := []float64{1 / stat.PopVariance(cpu, nil), 1 / stat.PopVariance(net, nil)}
	m, err := DecomposeMulti([][]float64{cpu, net}, 576, 30, 8, w)
	if err != nil {
		t.Fatal(err)
	}
	a := m.Analyze([]string{"cpu", "net"}, 5*time.Minute)
	period := func(T float64) *Component {
		for i, c := range a.Components {
			if c.Kind == KindHarmonic && math.Abs(c.Period-T) < 2 {
				return &a.Components[i]
			}
		}
		return nil
	}
	daily, h12, h8 := period(288), period(144), period(96)
	if daily == nil || h12 == nil || h8 == nil {
		t.Fatalf("harmonics not found: %+v", a.Components)
	}
	// Each cycle is carried by the channels that have it.
	if daily.Share[0] < 0.3 || daily.Share[1] < 0.3 {
		t.Errorf("daily shares %v: both channels have it", daily.Share)
	}
	if h8.Share[0] < 0.1 || h8.Share[1] > 0.01 {
		t.Errorf("8h shares %v: only cpu has it", h8.Share)
	}
	if h12.Share[1] < 0.03 || h12.Share[0] > 0.01 {
		t.Errorf("12h shares %v: only net has it", h12.Share)
	}
	// The noise is fitted per channel, in the channel's units.
	if sd := a.Noise.Channels[0].SD; math.Abs(sd-2) > 0.2 {
		t.Errorf("cpu noise sd %v, want ~2", sd)
	}
	if sd := a.Noise.Channels[1].SD; math.Abs(sd-1e7)/1e7 > 0.1 {
		t.Errorf("net noise sd %v, want ~1e7", sd)
	}
	var gs [][]int
	for _, g := range a.Coarse {
		gs = append(gs, g.Comps)
	}
	ck, err := a.Check(gs)
	if err != nil {
		t.Fatal(err)
	}
	for c, r := range ck.Residual {
		if r.Explained < 0.8 {
			t.Errorf("channel %d: the groups explain %.2f", c, r.Explained)
		}
	}
	if _, ok := findingWith(ck.Findings, "The groups explain cpu: ", "; net: "); !ok {
		t.Errorf("per-channel explained finding missing: %+v", ck.Findings)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:                "1d",
		20*time.Hour + 50*time.Minute: "20h50m",
		90 * time.Second:              "1m30s",
		1500 * time.Millisecond:       "1s500ms",
		0:                             "0s",
		26*time.Hour + 5*time.Minute:  "1d2h5m",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParseGroups(t *testing.T) {
	g, err := ParseGroups(" 0; 1-2 ;4,6-8;")
	if err != nil {
		t.Fatal(err)
	}
	if FormatGroups(g) != "0;1-2;4,6-8" || len(g) != 3 || !slices.Equal(g[2], []int{4, 6, 7, 8}) {
		t.Errorf("groups = %v", g)
	}
	if g, err := ParseGroups(""); g != nil || err != nil {
		t.Errorf("empty: %v %v", g, err)
	}
	for _, bad := range []string{"a", "3-1", "-1", "1,1", "0-2,2"} {
		if _, err := ParseGroups(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// An idle VM with rare bursts: the robust sigma sees only the quiet level.
// The command's noise model then removed every component, the residual
// vanished and the whole spectrum became "structure"; the floor is now
// simulated with the full variance of the residual.
func TestAnalyzeRareBursts(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 21))
	x := make([]float64, 1152)
	for i := range x {
		x[i] = 0.25 + 0.01*rng.NormFloat64()
		if rng.Float64() < 0.02 {
			x[i] = 20 + 50*rng.Float64()
		}
	}
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(5 * time.Minute)
	ch := a.Noise.Channels[0]
	if len(a.Structure) > 3 || a.Noise.Negligible || !ch.HeavyTailed() {
		t.Errorf("structure %v, noise %+v", a.Structure, a.Noise)
	}
	for _, c := range a.Components {
		if c.Kind == KindHarmonic && !c.Borderline() {
			t.Errorf("random bursts made a harmonic: %+v", c)
		}
	}
	if _, ok := findingWith(a.Findings, "Heavy-tailed noise: its standard deviation is"); !ok {
		t.Errorf("heavy-tail finding missing: %+v", a.Findings)
	}
}

// Bursts on top of a daily cycle: the cycle must still be found.
func TestAnalyzeBurstsOnACycle(t *testing.T) {
	rng := rand.New(rand.NewPCG(22, 22))
	x := make([]float64, 1152)
	for i := range x {
		x[i] = 5 + 3*math.Sin(2*math.Pi*float64(i)/288) + 0.2*rng.NormFloat64()
	}
	for range 12 { // hour-long bursts at random moments
		at := rng.IntN(len(x) - 12)
		for i := at; i < at+12; i++ {
			x[i] += 20
		}
	}
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(5 * time.Minute)
	daily := false
	for _, c := range a.Components {
		if c.Kind == KindHarmonic && !c.Borderline() && math.Abs(c.Period-288) < 29 {
			daily = true // the bursts bias the period by a few percent
		}
	}
	if !daily || !a.Noise.Channels[0].HeavyTailed() {
		t.Errorf("daily %v, structure %v, noise %+v, components %+v", daily, a.Structure, a.Noise.Channels[0], a.Components)
	}
}

// A weak pair whose amplitude grows within the history has a root above 1:
// a long default forecast leaves it out, a short one keeps it; a growing
// trend is kept (a level can really grow).
func TestForecastableGrowing(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 41))
	const n = 1692 // 5 days 21 hours at 5 minutes
	x := make([]float64, n)
	for i := range x {
		tt := float64(i)
		x[i] = 17*math.Pow(1.0004, tt) + 7*math.Sin(2*math.Pi*tt/288) + 2*math.Sin(4*math.Pi*tt/288) +
			0.08*math.Pow(1.0015, tt)*math.Sin(2*math.Pi*tt/92) + 0.3*rng.NormFloat64()
	}
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Analyze(5 * time.Minute)
	var weak, daily, trend *Component
	for i, c := range a.Components {
		switch {
		case c.Kind == KindHarmonic && math.Abs(c.Period-92) < 3:
			weak = &a.Components[i]
		case c.Kind == KindHarmonic && math.Abs(c.Period-288) < 10:
			daily = &a.Components[i]
		case c.Kind == KindTrend && trend == nil:
			trend = &a.Components[i]
		}
	}
	if weak == nil || daily == nil || trend == nil || weak.Borderline() {
		t.Fatalf("components %+v", a.Components)
	}
	if weak.Growth < 1.001 || math.Abs(daily.Growth-1) > 5e-4 || trend.Growth < 1.0002 {
		t.Errorf("growth: weak %v, daily %v, trend %v", weak.Growth, daily.Growth, trend.Growth)
	}
	group, left, growing := a.Forecastable(3 * 288)
	if !slices.Equal(growing, weak.Comps) || slices.Contains(group, weak.Comps[0]) || !slices.Contains(group, trend.Comps[0]) || !slices.Contains(group, daily.Comps[0]) {
		t.Errorf("3 days: group %v, left %v, growing %v", group, left, growing)
	}
	if group, _, growing := a.Forecastable(12); growing != nil || !slices.Contains(group, weak.Comps[0]) {
		t.Errorf("an hour: group %v, growing %v", group, growing)
	}
	if _, _, growing := a.Forecastable(0); growing != nil {
		t.Errorf("no horizon: growing %v", growing)
	}

	// The main daily pair growing within the history is kept: dropping it
	// would leave the trend alone.
	for i := range x {
		tt := float64(i)
		x[i] = 17 + 7*math.Pow(1.0005, tt)*math.Sin(2*math.Pi*tt/288) + 0.3*rng.NormFloat64()
	}
	if s, err = Decompose(x, 288, 0, 0); err != nil {
		t.Fatal(err)
	}
	a = s.Analyze(5 * time.Minute)
	found := false
	for _, c := range a.Components {
		if c.Kind == KindHarmonic && math.Abs(c.Period-288) < 10 {
			found = true
			if c.Growth < 1.0003 || c.Share[0] < 0.5 {
				t.Fatalf("daily pair %+v", c)
			}
			group, _, growing := a.Forecastable(7 * 288)
			if growing != nil || !slices.Contains(group, c.Comps[0]) {
				t.Errorf("a strong growing pair: group %v, growing %v", group, growing)
			}
		}
	}
	if !found {
		t.Errorf("no daily pair: %+v", a.Components)
	}
}
