package ssa

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// start17 is the first point of the synthetic histories: four days at 5
// minutes (1152 points) from 2026-09-17 04:00 UTC, like the live SSA tools.
var start17 = time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)

// at returns the index of time t in a series starting at start17 with a
// 5-minute step.
func at(t time.Time) int { return int(t.Sub(start17) / (5 * time.Minute)) }

// ingest is received traffic with a daily profile (low at midnight UTC) and
// noise; with an outage it drops to zero 2026-09-20 00:00-13:00 and catches
// up at 2.5 times the usual rate until 23:00, as a Kafka broker did once.
func ingest(outage bool) []float64 {
	rng := rand.New(rand.NewPCG(31, 31))
	x := make([]float64, 1152)
	for i := range x {
		tod := math.Mod(float64(i)+48, 288) // 04:00 is point 48 of a day
		x[i] = 170 - 110*math.Cos(2*math.Pi*tod/288) + 5*rng.NormFloat64()
	}
	if outage {
		down, up, end := at(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)), at(time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC)), at(time.Date(2026, 9, 20, 23, 0, 0, 0, time.UTC))
		for i := down; i < up; i++ {
			x[i] = 0.03
		}
		for i := up; i < end; i++ {
			x[i] *= 2.5
		}
	}
	return x
}

func TestRunsRegimeBreak(t *testing.T) {
	s, err := Decompose(ingest(true), 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if !a.Broken() {
		t.Fatalf("outage not found: runs %+v", a.Runs)
	}
	f, ok := findingWith(a.Findings, "stretch(es) of 2h or longer leave the usual daily profile", "2026-09-20 00:00-12:55 UTC (13h, below it by", "2026-09-20 13:00-22:55 UTC (10h, above it by")
	if !ok || f.Level != LevelWarn {
		t.Errorf("regime finding missing: %+v", a.Findings)
	}
	// The period is distorted: no advice to fit the window to it.
	if f, ok := findingWith(a.Findings, "is estimated on a history with stretches in another regime", "daily cycle"); !ok || f.Level != LevelWarn {
		t.Errorf("distorted-period finding missing: %+v", a.Findings)
	}
	for _, f := range a.Findings {
		if strings.Contains(f.Text, "a multiple of the period, e.g.") || strings.Contains(f.Text, ", a multiple of the period.") {
			t.Errorf("window advice on a broken history: %s", f.Text)
		}
	}
	// The same traffic without the outage: one daily cycle, nothing broken.
	s, err = Decompose(ingest(false), 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a = s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if a.Broken() || len(a.Runs) > 1 {
		t.Errorf("clean traffic: runs %+v", a.Runs)
	}
}

// A day that runs the usual profile an hour late (a weekend morning) is not
// away from it.
func TestRunsPhaseShift(t *testing.T) {
	rng := rand.New(rand.NewPCG(33, 33))
	x := make([]float64, 1152)
	for i := range x {
		tod := math.Mod(float64(i)+48, 288)
		if i >= 288*2 && i < 288*3 { // 2026-09-19 04:00 to 09-20 04:00: an hour late
			tod = math.Mod(tod-12+288, 288)
		}
		x[i] = 170 - 110*math.Cos(2*math.Pi*tod/288) + 5*rng.NormFloat64()
	}
	s, err := Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if a.Broken() {
		t.Errorf("an hour-late day is another regime: %+v", a.Runs)
	}
	// Without the tolerance the slopes of that day are 25 below or above the
	// profile (4 sigma is 20): the test would catch its removal.
	ref := usual(x, 288, 864, nil)
	off := 0
	for _, v := range beyond(x, ref, 0, 0) {
		if math.Abs(v) > 20 {
			off++
		}
	}
	if off < 50 {
		t.Errorf("the shifted day is not off the profile without the tolerance: %d points", off)
	}
}

// memJob is memory at 8% with a job every day at about 07:00 UTC (starting
// between 06:50 and 07:45) that raises it to 33% and then 58% for 15-25
// minutes, as on a Kafka broker.
func memJob() []float64 {
	rng := rand.New(rand.NewPCG(32, 32))
	x := make([]float64, 1152)
	for i := range x {
		x[i] = 8 + 0.05*rng.NormFloat64()
	}
	for d, off := range []int{35, 45, 10, -10} { // minutes after 07:00
		i := at(time.Date(2026, 9, 17+d, 7, 0, 0, 0, time.UTC).Add(time.Duration(off) * time.Minute))
		for k := range 3 + d%3 {
			x[i+k] = 33 + 25*float64(min(k, 1))
		}
	}
	return x
}

func TestRunsDailyRecurrence(t *testing.T) {
	s, err := Decompose(memJob(), 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if len(a.Recurring) != 1 {
		t.Fatalf("recurrences %+v, runs %+v", a.Recurring, a.Runs)
	}
	rec := a.Recurring[0]
	if rec.Days != 4 || rec.Of != 4 || rec.At < 7*time.Hour || rec.At > 7*time.Hour+30*time.Minute {
		t.Errorf("recurrence %+v", rec)
	}
	if a.Broken() {
		t.Errorf("a daily job is not another regime: %+v", a.Runs)
	}
	if f, ok := findingWith(a.Findings, "recur daily at about 07:", "on 4 of 4 days", "above it by", "scheduled job"); !ok || f.Level != LevelInfo {
		t.Errorf("recurrence finding missing: %+v", a.Findings)
	}
	// Without the start time the time of day is counted from the first point.
	a = s.Analyze(5 * time.Minute)
	if _, ok := findingWith(a.Findings, "into each day counted from the first point"); !ok {
		t.Errorf("recurrence without a start: %+v", a.Findings)
	}
}

// Gaussian noise leaves no stretch, and random half-hour bursts are neither a
// regime break nor a daily job.
func TestRunsNoise(t *testing.T) {
	s, err := Decompose(tones(2016, 288, 0.4, 2), 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if len(a.Runs) > 1 || a.Broken() {
		t.Errorf("gaussian noise: runs %+v", a.Runs)
	}
	rng := rand.New(rand.NewPCG(22, 22))
	x := make([]float64, 1152)
	for i := range x {
		x[i] = 5 + 3*math.Sin(2*math.Pi*float64(i)/288) + 0.2*rng.NormFloat64()
	}
	for range 12 { // half-hour bursts: even two overlapping stay under 2 h
		at := rng.IntN(len(x) - 6)
		for i := at; i < at+6; i++ {
			x[i] += 20
		}
	}
	s, err = Decompose(x, 288, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a = s.AnalyzeWith(Options{Start: start17, Step: 5 * time.Minute})
	if a.Broken() || len(a.Recurring) > 0 {
		t.Errorf("random bursts: runs %+v, recurring %+v", a.Runs, a.Recurring)
	}
	if _, ok := findingWith(a.Findings, "short burst(s) (under 2h)"); !ok {
		t.Errorf("burst finding missing: %+v", a.Findings)
	}
}

func TestOffRuns(t *testing.T) {
	r := []float64{0, 5, 0, 0, 6, 0, 0, 0, 7, -5, -6, 0}
	got := offRuns(r, 0, 1)
	// 5 and 6 are 2 points apart (join), 7 is 3 points after 6 (new), -5 -6 below (new).
	want := []Run{{From: 1, To: 4}, {From: 8, To: 8}, {From: 9, To: 10}}
	if len(got) != len(want) {
		t.Fatalf("runs %+v", got)
	}
	for i := range want {
		if got[i].From != want[i].From || got[i].To != want[i].To {
			t.Errorf("run %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
