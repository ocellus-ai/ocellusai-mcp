# Writing a processor

[Architecture](architecture.md) · [Components](components.md) · [Writing a worker](writing-workers.md) · **Writing a processor**

Processors are the analysis part of Ocellus AI: the steps of a tool's `process` section that reshape data, find
anomalies, compare peers and forecast. This guide explains how to add to them. Often the right addition is smaller
than a processor: a new detector inside `anomaly` or `anomaly_mv` is one file and one line. The guide covers each
level with a complete example:

| Example | What it is |
|---|---|
| [`smooth`](#tutorial-a-transformation-smooth) | a transformation: a moving average over every series |
| [`trend`](#tutorial-an-analysis-trend) | an analysis: a fitted line per series and when it reaches a limit |
| [`hampel`](#a-detector-for-anomaly-hampel) | a detector for `anomaly` (and `outliers`): the Hampel identifier |
| [`zdist`](#a-detector-for-anomaly_mv-zdist) | a detector for `anomaly_mv`: the length of the z-score vector |

Every code block of these examples compiles, passes its tests and `golangci-lint`, and the example tools pass
`-validate`. Read [Architecture](architecture.md) first; what the existing processors do is described for users in
[Processors](processors.md).

## Contents

- [What to build](#what-to-build)
- [How the process stage calls a processor](#how-the-process-stage-calls-a-processor)
- [Rules for processor code](#rules-for-processor-code)
- [Data shapes](#data-shapes)
- [Transformations and analyses](#transformations-and-analyses)
- [Tutorial: a transformation, smooth](#tutorial-a-transformation-smooth)
- [Tutorial: an analysis, trend](#tutorial-an-analysis-trend)
- [A detector for anomaly: hampel](#a-detector-for-anomaly-hampel)
- [A detector for anomaly_mv: zdist](#a-detector-for-anomaly_mv-zdist)
- [A detector for the anomaly ensemble](#a-detector-for-the-anomaly-ensemble)
- [Changing SSA](#changing-ssa)
- [Documentation](#documentation)
- [Checklist](#checklist)
- [Common mistakes](#common-mistakes)

## What to build

| You want | Build | Where | Registered in |
|---|---|---|---|
| another way to find unusual samples in one metric: robust statistics, seasonality, smoothing residuals | a detector for `anomaly`; `outliers` gets it too | `internal/processor/anomaly/<method>.go` | the `detectors` table of `anomaly` |
| another way to judge a sample across several metrics at once | a detector for `anomaly_mv` | `internal/processor/anomalymv/<method>.go` | the `detectors` table of `anomaly_mv` |
| another detector in the anomaly ensemble | a `tsanomaly` detector | `internal/tsanomaly/<name>.go` | `tsanomaly.Analyze` and the ensemble's settings |
| another noise model, grouping or forecast in SSA | a change in the library | `internal/ssa` | — |
| a new transformation of data, or an analysis with its own report | a processor | `internal/processor/<name>/` | `buildProcessors` in `cmd/ocellusai-mcp/main.go` |
| to pick a call's result, rename fields or turn a payload into a processor's input | nothing: a `fn: jq` step | — | — |

A detector is cheaper than a processor. Its processor already parses the input, skips short series, applies
`direction`, `min_delta` and `max_anomalies` and writes the report; the detector receives clean numbers and returns
the indices of the anomalous samples.

A processor never fetches data. It works only on its input: sources are workers, and a tool that needs two sources
makes two calls.

## How the process stage calls a processor

```go
type Processor interface {
    Name() string
    Validate(with map[string]any) error
    Run(ctx context.Context, with map[string]any, data any, params map[string]any) (any, error)
}
```

- **`Name`** is what tools write in `fn:`.
- **At startup** the catalog finds the processor of every step and calls `Validate(with)` with the step's `with`
  section exactly as written: strings may still contain templates. An error stops the server as
  `tools/x.yaml: process[1] (trend): with.min_points: must be at least 2, got 1`.
- **On every call** the pipeline renders `with` with the tool's arguments and calls `Run`. `data` is the output of the
  previous step, or the worker's result for the first step (for a tool with `calls`, the object
  `{call name: result}`). Whatever `Run` returns replaces `data` for the next step and finally goes to
  `response.jq`. An error is reported to the agent as `failed at stage process: process[1] (trend): …`.
- **`params`** are the tool's arguments after defaults, the same values the `with` templates see. Most processors
  ignore them, because their settings arrive through `with`; the `jq` processor exposes them as `$params`.
- **One value** of the processor serves every step of every tool that names it, concurrently. Settings belong to the
  call, not to the struct.

## Rules for processor code

1. **One input shape.** A processor accepts exactly one shape, described in its package comment. When the input
   has another shape, the error says what was expected and suggests the `jq` step that would produce it. There are
   no keys in `with` for picking fields out of arbitrary JSON (`items: ".result[]"`): shaping is the job of `jq`.
2. **`with` belongs to the processor.** Reject unknown keys with `processor.CheckKeys`. Check literals in
   `Validate`; skip values that are still templates (`processor.IsTemplate`) and check them in `Run`. The simplest
   way is one parser for both, `parseSettings(with, load bool)`, as `trend` and `anomaly` do.
3. **Read settings with the helpers.** A templated setting arrives as a string (`"1.5"`), a literal as a number.
   `processor.Float`, `Int`, `Bool`, `String`, `Seconds` and `SpanOf` accept both and report an unrendered template.
4. **Keys that choose the algorithm are literals.** `method` of `anomaly` cannot be a template, because the detector's
   own keys can only be checked once it is known.
5. **JSON-compatible output, untouched input.** Return `map[string]any`, `[]any`, strings, `float64`, `int`, `bool`
   and `nil`. Build new values; the input may still be read by `{{ result "x" }}` in the answer template.
6. **No `NaN` or infinities in the output.** JSON cannot encode them, so the call would fail at stage `encode`. Write
   `nil` instead (the ensemble's `finiteOrNil`).
7. **Respect the context.** Check `ctx.Err()` between series. A library call that does not take a context runs in a
   goroutine, and the processor returns on `ctx.Done()` (see `analyze` in the `ensemble` package and `parallel` in
   `ssaproc`).
8. **A problem with the data is not an error.** A series that is too short, empty, irregular or degenerate is
   reported as skipped with a `reason`, and the rest of the series are analysed. Errors are for wrong settings and
   a wrong input shape.
9. **Numbers stay numbers.** Do not round in the report; formatting is the answer template's job. Times go out as
   unix seconds plus an RFC 3339 string (`series.FormatTime`).
10. **Use the shared helpers** (`processor.Number`, `processor.JSONType`, `series.LabelsToAny`, `series.Regular`)
    instead of copying them.

## Data shapes

Time series travel in two canonical shapes, defined with their Go types in `internal/processor/series`. Every
processor that works on time series uses them; do not invent a third.

| Shape | JSON | Read | Write |
|---|---|---|---|
| Scalar series: a Prometheus matrix | `{resultType: matrix, result: [{metric, values: [[ts, "v"]]}]}` | `series.ParseMatrix(data) ([]Series, error)` | `series.ToMatrix([]Series)` |
| Multivariate series | `{dims: [...], series: [{labels, points: [[ts, v1, v2]], reason?}]}` | `series.ParseMulti(data) (Multi, error)` | `series.ToMulti(Multi, extra)` |

`ParseMatrix` and `ParseMulti` accept numbers and numeric strings, drop non-finite samples and return errors that
name the place and the expected shape. A multivariate series can be empty with a `reason` (an object `join` could not
build); analyses report it as skipped with that reason.

Data without a time axis has its own documented shape: `outliers` takes `[{key, value, labels}]`, `cluster_events`
takes `[{start, end, labels, signals, score}]`. A processor for a new kind of data defines its shape the same way: in
the package comment, with a strict parser and errors that name the item and the field.

`series.Regular` lays samples on a regular grid with gaps as `NaN`, for algorithms that need equal steps.

## Transformations and analyses

Every processor is one of two kinds. Say which in the package comment.

**A transformation** returns data in a canonical shape, usually the shape it received, so steps can be chained:
`join` turns matrices into a multivariate series, `smooth` below returns a smoothed matrix that `anomaly` can read.
Series a transformation cannot handle are passed on with a `reason` rather than dropped.

**An analysis** returns a report and usually ends the chain, or is reshaped by a `jq` step for the next analysis (as
tools do before `cluster_events`). Reports are not one fixed schema, but they share names, and a new report should
follow them:

| Field | Meaning |
|---|---|
| `series_total`, `series_analyzed`, `series_skipped` | counts over the input |
| `series` | one entry per input series, in input order |
| `metric` (matrix input) or `labels` (other inputs) | which object the entry is about |
| `points` | how many samples the series had |
| `skipped`, `reason` | `true` and why, for a series that was not analysed |
| `stats` | the method's numbers (`mean`, `median`, `threshold`, …) |
| `timestamp` and `time` | unix seconds and RFC 3339 for a moment |
| `anomaly_count`, `total_anomalies` | counts before a cap such as `max_anomalies` cuts the list |

## Tutorial: a transformation, smooth

`smooth` replaces every sample by the mean of the last `window` samples of its series. Input and output are a
matrix, so it can stand before `anomaly` to suppress sampling noise:

```yaml
process:
  - fn: smooth
    with: {window: "{{ .window }}"}
  - fn: anomaly
    with: {method: iqr}
```

### The package

`internal/processor/smooth/smooth.go`:

```go
// Package smooth is the `smooth` processor: a trailing moving average over
// every series of a Prometheus range result. Input and output are both a
// matrix in the Prometheus HTTP API shape, so the step can be chained before
// `anomaly` to suppress sampling noise.
package smooth

import (
	"context"
	"fmt"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: smooth).
const Name = "smooth"

// DefaultWindow is the number of samples averaged when `window` is absent.
const DefaultWindow = 5

// Processor implements processor.Processor.
type Processor struct{}

// New creates the smooth processor.
func New() *Processor { return &Processor{} }

// Name returns "smooth".
func (*Processor) Name() string { return Name }

// Validate checks the raw `with` section at catalog load time.
func (*Processor) Validate(with map[string]any) error {
	if err := processor.CheckKeys(with, "window"); err != nil {
		return err
	}
	if processor.IsTemplate(with["window"]) {
		return nil // rendered per call; checked again in Run
	}
	_, err := window(with)
	return err
}

func window(with map[string]any) (int, error) {
	w, err := processor.Int(with, "window", DefaultWindow)
	if err != nil {
		return 0, err
	}
	if w < 1 {
		return 0, fmt.Errorf("with.window: must be >= 1, got %d", w)
	}
	return w, nil
}

// Run replaces every sample by the mean of the last `window` samples of its
// series (fewer at the start). Labels and timestamps are kept. The tool
// parameters are not needed: window already arrives through `with`.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	w, err := window(with)
	if err != nil {
		return nil, err
	}
	all, err := series.ParseMatrix(data) // same input checks and messages as the anomaly processor
	if err != nil {
		return nil, err
	}
	out := make([]series.Series, 0, len(all))
	for _, s := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		smoothed := series.Series{Labels: s.Labels, Points: make([]series.Point, 0, len(s.Points))}
		sum := 0.0
		for i, p := range s.Points {
			sum += p.V
			if i >= w {
				sum -= s.Points[i-w].V
			}
			n := min(i+1, w)
			smoothed.Points = append(smoothed.Points, series.Point{T: p.T, V: sum / float64(n)})
		}
		out = append(out, smoothed)
	}
	return series.ToMatrix(out), nil
}
```

- `Validate` rejects unknown keys and checks a literal `window`; a templated one is left to `Run`.
- `processor.Int` reads `window` whether it is `3` or the rendered string `"3"`, and turns an unrendered template into
  an error.
- `series.ParseMatrix` gives the same input checks and messages as every other matrix processor; `series.ToMatrix`
  writes values as strings, like the Prometheus API, so the next step and jq see the same shape as from the worker.
- The input is not modified: the output is built from new series.

### Unit tests

`internal/processor/smooth/smooth_test.go`:

```go
package smooth

import (
	"context"
	"strings"
	"testing"
)

func matrix(vals ...float64) map[string]any {
	values := make([]any, 0, len(vals))
	for i, v := range vals {
		values = append(values, []any{1757600000 + float64(60*i), v})
	}
	return map[string]any{"resultType": "matrix", "result": []any{
		map[string]any{"metric": map[string]any{"pod": "a"}, "values": values},
	}}
}

func TestRun(t *testing.T) {
	out, err := New().Run(context.Background(), map[string]any{"window": "2"}, matrix(1, 3, 5, 7), nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := out.(map[string]any)["result"].([]any)[0].(map[string]any)
	got := entry["values"].([]any)
	for i, want := range []string{"1", "2", "4", "6"} {
		if v := got[i].([]any)[1]; v != want {
			t.Errorf("values[%d] = %v, want %s", i, v, want)
		}
	}
	if entry["metric"].(map[string]any)["pod"] != "a" {
		t.Error("labels must be kept")
	}
}

func TestValidateAndErrors(t *testing.T) {
	p := New()
	if err := p.Validate(map[string]any{"window": "{{ .w }}"}); err != nil {
		t.Errorf("template at load: %v", err)
	}
	for name, with := range map[string]map[string]any{"zero": {"window": 0}, "unknown": {"size": 3}, "text": {"window": "wide"}} {
		if err := p.Validate(with); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := p.Run(context.Background(), map[string]any{}, map[string]any{"resultType": "vector"}, nil); err == nil || !strings.Contains(err.Error(), "matrix") {
		t.Errorf("vector input: %v", err)
	}
	if _, err := p.Run(context.Background(), map[string]any{"window": "{{ .w }}"}, matrix(1), nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
		t.Errorf("unrendered with: %v", err)
	}
}
```

### Register it

In `cmd/ocellusai-mcp/main.go`, import the package and register it in `buildProcessors`:

```go
	if err := reg.Register(smooth.New()); err != nil {
		return nil, err
	}
```

`TestBuildProcessors` in `cmd/ocellusai-mcp/main_test.go` pins the sorted list of processors. Add the new name:

```
anomaly,anomaly_ensemble,anomaly_mv,cluster_events,join,jq,outliers,smooth,ssa,ssa_mv
```

If a tool in `tools/` or `tools-test/` uses the processor, also add `smooth.New()` to `referenceProcessors()` in
`internal/server/server_test.go`: the server tests load those catalogs with that registry.

### A tool test

A tool test runs a tool file through the catalog and the pipeline with a fake worker. It shows that the step works
with templates in `with` and that a bad literal stops the catalog with the file and the step:

```go
package smooth_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/catalog"
	"github.com/ocellus-ai/ocellusai-mcp/internal/pipeline"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/smooth"
	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// promFake stands in for a prometheus instance: every query gets one series
// 1, 3, 5, 7 at a one-minute step.
type promFake struct{}

func (promFake) Type() string                  { return "prometheus" }
func (promFake) Validate(map[string]any) error { return nil }
func (promFake) Execute(context.Context, map[string]any) (any, error) {
	values := []any{}
	for i, v := range []string{"1", "3", "5", "7"} {
		values = append(values, []any{1757600000 + float64(60*i), v})
	}
	return map[string]any{"resultType": "matrix", "result": []any{
		map[string]any{"metric": map[string]any{"pod": "api-1"}, "values": values},
	}}, nil
}

const tool = `
name: smoothed_cpu
description: CPU per pod, smoothed.
worker: prom
params:
  window: {type: integer, default: 2, minimum: 1}
request: {type: range, query: 'sum by (pod) (rate(container_cpu_usage_seconds_total[5m]))'}
process:
  - fn: smooth
    with: {window: "{{ .window }}"}
response:
  jq: '[.result[0].values[][1]]'
`

func load(t *testing.T, yml string) (*catalog.Tool, error) {
	t.Helper()
	procs := processor.Registry{}
	if err := procs.Register(smooth.New()); err != nil {
		t.Fatal(err)
	}
	return catalog.Parse("smoothed_cpu.yaml", []byte(yml), worker.Registry{"prom": promFake{}}, procs)
}

func TestToolEndToEnd(t *testing.T) {
	tl, err := load(t, tool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.New(nil).Run(context.Background(), tl, json.RawMessage(`{"window": 2}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{"1", "2", "4", "6"}; !reflect.DeepEqual(res.Structured, want) {
		t.Errorf("structured = %#v, want %v", res.Structured, want)
	}
	// A literal that the processor rejects stops the catalog at load time.
	_, err = load(t, strings.Replace(tool, `"{{ .window }}"`, "0", 1))
	if err == nil || !strings.Contains(err.Error(), "smoothed_cpu.yaml: process[0] (smooth): with.window: must be >= 1, got 0") {
		t.Errorf("load error = %v", err)
	}
}
```

The processors with larger example tools keep them in `testdata/` next to such a test (`anomalymv`, `ensemble`,
`ssaproc`) and load them with `catalog.Load`.

### Use it in a tool

A tool that smooths CPU per pod before looking for anomalies:

```yaml
name: pod_cpu_smoothed_anomalies
description: |
  CPU of every pod in a namespace over the last hours, smoothed and checked
  for unusual stretches.
worker: prometheus
params:
  namespace: {type: string, required: true}
  range:     {type: string, default: "6h", pattern: "^[0-9]+[mhd]$"}
  window:    {type: integer, default: 5, minimum: 1, maximum: 60}
request:
  type: range
  query: sum by (pod) (rate(container_cpu_usage_seconds_total{namespace={{ promLabel .namespace }}, container!=""}[5m]))
  start: "now-{{ promDuration .range }}"
  end: now
  step: 60s
process:
  - fn: smooth
    with: {window: "{{ .window }}"}
  - fn: anomaly
    with: {method: iqr, min_delta: 0.05}
```

`-validate` shows the chain:

```
pod_cpu_smoothed_anomalies       worker=prometheus   type=prometheus calls=-            process=smooth>anomaly file=tools/pod_cpu_smoothed_anomalies.yaml
```

A misspelt `fn` stops the start and lists what exists:

```
ocellusai-mcp: tools/pod_cpu_smoothed_anomalies.yaml: process[0].fn: unknown processor "smoth" (available: anomaly, anomaly_ensemble, anomaly_mv, cluster_events, join, jq, outliers, smooth, ssa, ssa_mv)
```

## Tutorial: an analysis, trend

`trend` fits a least-squares line through every series of a matrix and reports its slope, how well a line describes
the series and, if a limit is given, when the line reaches it: "the disk is full in three days at this rate". It
shows what an analysis adds to a transformation: settings parsed the same way at startup and at call time, series
skipped with a reason, and a report.

### The package

`internal/processor/trend/trend.go`:

```go
// Package trend is the `trend` processor: a least-squares line through every
// series of a Prometheus range result and, on request, the time the line
// reaches a limit ("the disk is full in 3 days at this rate").
//
// Analysis: the input is the scalar series form (a matrix, see package
// series); the output is a report.
//
// with:
//
//	min_points  series with fewer samples are skipped (default 10, at least 2)
//	limit       optional: report when the line reaches this value
//
// Output:
//
//	{series_total, series_analyzed, series_skipped,
//	 series: [{metric, points, skipped, [reason], slope_per_hour, r2, last_value,
//	           [limit_at, limit_time]}]}
//
// slope_per_hour is in the units of the series per hour, r2 tells how well a
// line describes the series (1 = exactly). limit_at (unix seconds) and
// limit_time are present when the line reaches the limit after the last
// sample.
package trend

import (
	"context"
	"fmt"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
	"github.com/ocellus-ai/ocellusai-mcp/internal/processor/series"
)

// Name is the processor name referenced from YAML (fn: trend).
const Name = "trend"

// DefaultMinPoints is the shortest series analysed when min_points is absent.
const DefaultMinPoints = 10

// Processor implements processor.Processor.
type Processor struct{}

// New creates the trend processor.
func New() *Processor { return &Processor{} }

// Name returns "trend".
func (*Processor) Name() string { return Name }

type settings struct {
	minPoints int
	limit     *float64 // nil when not set
}

// Validate checks the raw `with` section at catalog load time.
func (*Processor) Validate(with map[string]any) error {
	_, err := parseSettings(with, true)
	return err
}

// parseSettings reads `with`. At load time (load = true) values that are
// still templates are skipped; at run time every value must be final.
func parseSettings(with map[string]any, load bool) (settings, error) {
	if err := processor.CheckKeys(with, "min_points", "limit"); err != nil {
		return settings{}, err
	}
	st := settings{minPoints: DefaultMinPoints}
	deferred := func(key string) bool { return load && processor.IsTemplate(with[key]) }
	if !deferred("min_points") {
		n, err := processor.Int(with, "min_points", DefaultMinPoints)
		if err != nil {
			return settings{}, err
		}
		if n < 2 {
			return settings{}, fmt.Errorf("with.min_points: must be at least 2, got %d", n)
		}
		st.minPoints = n
	}
	if with["limit"] != nil && !deferred("limit") {
		f, err := processor.Float(with, "limit", 0)
		if err != nil {
			return settings{}, err
		}
		st.limit = &f
	}
	return st, nil
}

// Run fits a line to every series and reports it.
func (*Processor) Run(ctx context.Context, with map[string]any, data any, _ map[string]any) (any, error) {
	st, err := parseSettings(with, false)
	if err != nil {
		return nil, err
	}
	all, err := series.ParseMatrix(data)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(all))
	analyzed := 0
	for _, s := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := map[string]any{"metric": series.LabelsToAny(s.Labels), "points": len(s.Points), "skipped": false}
		if len(s.Points) < st.minPoints {
			entry["skipped"] = true
			entry["reason"] = fmt.Sprintf("fewer than %d points (min_points)", st.minPoints)
			out = append(out, entry)
			continue
		}
		analyzed++
		slope, intercept, r2 := fit(s.Points)
		last := s.Points[len(s.Points)-1]
		entry["slope_per_hour"] = slope * 3600
		entry["r2"] = r2
		entry["last_value"] = last.V
		if st.limit != nil && slope != 0 {
			if at := (*st.limit - intercept) / slope; at > last.T {
				entry["limit_at"] = at
				entry["limit_time"] = series.FormatTime(at)
			}
		}
		out = append(out, entry)
	}
	return map[string]any{
		"series_total":    len(all),
		"series_analyzed": analyzed,
		"series_skipped":  len(all) - analyzed,
		"series":          out,
	}, nil
}

// fit returns the least-squares line v = slope·t + intercept through the
// points and its coefficient of determination. A series without spread in
// time has slope 0; a constant series is described exactly (r2 = 1).
func fit(points []series.Point) (slope, intercept, r2 float64) {
	n := float64(len(points))
	var sumT, sumV float64
	for _, p := range points {
		sumT += p.T
		sumV += p.V
	}
	meanT, meanV := sumT/n, sumV/n
	var stt, stv, svv float64
	for _, p := range points {
		dt, dv := p.T-meanT, p.V-meanV
		stt += dt * dt
		stv += dt * dv
		svv += dv * dv
	}
	if stt == 0 {
		return 0, meanV, 0
	}
	slope = stv / stt
	intercept = meanV - slope*meanT
	if svv == 0 {
		return slope, intercept, 1
	}
	return slope, intercept, stv * stv / (stt * svv)
}
```

What to notice:

- **One parser, two moments.** `parseSettings(with, true)` at startup skips templated values; `parseSettings(with,
  false)` in `Run` requires final values. The checks cannot drift apart.
- **Optional settings** are a pointer (`limit *float64`), so "not set" differs from 0.
- **A short series is skipped**, with a reason that names the setting to change, and does not fail the tool.
- **The report** follows the shared names (`series_total`, `metric`, `points`, `skipped`, `reason`) and gives a time
  both as unix seconds and as RFC 3339. Values are not rounded.
- **Numerical care**: the fit subtracts the means before multiplying, which keeps unix timestamps around 1.7·10⁹ from
  swallowing the precision.

### Tests

`internal/processor/trend/trend_test.go` checks a known line exactly (one more unit a minute is 60 an hour, and from
50 it reaches 100 fifty minutes later), a falling series that never reaches the limit, a skipped series, the
validation of literals and templates, and the errors:

```go
package trend

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

const baseTS = 1757600000.0 // 2025-09-11T14:13:20Z

// matrix builds a Prometheus range result with one series per mount, one
// sample a minute, values as strings like the API writes them.
func matrix(series map[string][]float64) map[string]any {
	result := []any{}
	for mount, vals := range series {
		values := make([]any, 0, len(vals))
		for i, v := range vals {
			values = append(values, []any{baseTS + float64(60*i), strconv.FormatFloat(v, 'g', -1, 64)})
		}
		result = append(result, map[string]any{"metric": map[string]any{"mountpoint": mount}, "values": values})
	}
	return map[string]any{"resultType": "matrix", "result": result}
}

// line returns n values from start, changing by step every sample.
func line(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	return out
}

func entryFor(t *testing.T, report any, mount string) map[string]any {
	t.Helper()
	for _, s := range report.(map[string]any)["series"].([]any) {
		e := s.(map[string]any)
		if e["metric"].(map[string]any)["mountpoint"] == mount {
			return e
		}
	}
	t.Fatalf("no series for %s", mount)
	return nil
}

func TestRun(t *testing.T) {
	data := matrix(map[string][]float64{
		"/data": line(20, 50, 1),    // +1 a minute: 69 at the end, 100 at sample 50
		"/var":  line(20, 40, -0.5), // falling: never reaches the limit
		"/tmp":  line(3, 10, 1),     // too short
	})
	out, err := New().Run(context.Background(), map[string]any{"limit": "100"}, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := out.(map[string]any)
	if rep["series_total"] != 3 || rep["series_analyzed"] != 2 || rep["series_skipped"] != 1 {
		t.Errorf("counts = %v %v %v", rep["series_total"], rep["series_analyzed"], rep["series_skipped"])
	}
	d := entryFor(t, out, "/data")
	if math.Abs(d["slope_per_hour"].(float64)-60) > 1e-9 || math.Abs(d["r2"].(float64)-1) > 1e-9 || d["last_value"] != 69.0 {
		t.Errorf("/data = %v", d)
	}
	if d["limit_time"] != "2025-09-11T15:03:20Z" {
		t.Errorf("/data reaches 100 at %v", d["limit_time"])
	}
	if v := entryFor(t, out, "/var"); v["limit_at"] != nil || v["slope_per_hour"].(float64) >= 0 {
		t.Errorf("/var = %v", v)
	}
	if tmp := entryFor(t, out, "/tmp"); tmp["skipped"] != true || !strings.Contains(tmp["reason"].(string), "min_points") {
		t.Errorf("/tmp = %v", tmp)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("the report must be JSON: %v", err)
	}
}

func TestValidate(t *testing.T) {
	p := New()
	for name, with := range map[string]map[string]any{
		"empty":     {},
		"literals":  {"min_points": 5, "limit": 90},
		"templates": {"min_points": "{{ .n }}", "limit": "{{ .limit }}"},
	} {
		if err := p.Validate(with); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, with := range map[string]map[string]any{
		"unknown":    {"window": 5},
		"one point":  {"min_points": 1},
		"text limit": {"limit": "full"},
	} {
		if err := p.Validate(with); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRunErrors(t *testing.T) {
	p := New()
	ctx := context.Background()
	if _, err := p.Run(ctx, map[string]any{}, map[string]any{"resultType": "vector", "result": []any{}}, nil); err == nil || !strings.Contains(err.Error(), `expected resultType "matrix"`) {
		t.Errorf("vector input: %v", err)
	}
	if _, err := p.Run(ctx, map[string]any{"limit": "{{ .limit }}"}, matrix(nil), nil); err == nil || !strings.Contains(err.Error(), "unrendered template") {
		t.Errorf("unrendered with: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Run(cancelled, map[string]any{}, matrix(map[string][]float64{"/": line(20, 1, 1)}), nil); err == nil {
		t.Error("a cancelled context must stop the run")
	}
}
```

Register it like `smooth`, and add `trend` to the list in `TestBuildProcessors`.

### A tool

Filesystems that will fill up, soonest first. The analysis reports every filesystem; `response.jq` keeps those that
reach the limit and picks the fields the agent needs:

```yaml
name: disk_fill_trend
description: |
  Filesystems that are filling up: the growth per hour over the last hours
  and, for those growing, when they reach the given usage.
worker: prometheus
params:
  range: {type: string, default: "6h", pattern: "^[0-9]+[mhd]$"}
  limit: {type: number, default: 95, minimum: 1, maximum: 100, description: "Usage in percent"}
request:
  type: range
  query: 100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes)
  start: "now-{{ promDuration .range }}"
  end: now
  step: 5m
process:
  - fn: trend
    with: {limit: "{{ .limit }}", min_points: 12}
response:
  jq: '[.series[] | select(.limit_time) | {instance: .metric.instance, mountpoint: .metric.mountpoint, used: .last_value, per_hour: .slope_per_hour, full_at: .limit_time}] | sort_by(.full_at)'
```

## A detector for anomaly: hampel

The `anomaly` processor runs a detector over every series of a matrix; `outliers` runs the same detectors over a
list of objects. A new detector serves both. The contract, in `internal/processor/anomaly/detector.go`:

```go
type Point = series.Point     // {T, V float64}
type Series = series.Series   // {Labels map[string]string; Points []Point}

type Anomaly struct {
    Index     int     // position in Series.Points
    Score     float64 // larger = more anomalous, never negative
    Expected  float64 // the baseline the sample was compared with, in the metric's units
    Direction string  // DirectionUp or DirectionDown
}

type Verdict struct {
    Anomalies []Anomaly          // in index order
    Stats     map[string]float64 // reported as is under "stats"
    Expected  []float64          // optional per-point baseline, len == len(Points)
}

type Detector interface {
    Name() string                                            // the value of with.method
    Validate(params map[string]any) error                    // only the detector's own keys
    Detect(params map[string]any, s Series) (Verdict, error) // called with at least one point
}
```

What the processor does around a detector, so the detector does not:

- `params` is `with` without the processor's own keys (`method`, `min_points`, `direction`, `max_anomalies`,
  `min_delta`, `min_rel_delta`). Validate only your keys.
- Series shorter than `min_points` never reach `Detect`; samples are finite and in time order. In `outliers`, `T` is
  the item's index, not a time: do not assume timestamps.
- `Expected` must be in the metric's units: `min_delta` compares `|value − expected|` with it. `Direction` feeds the
  `direction` filter. `Score` decides what `max_anomalies` keeps.
- The package has helpers for detectors: `values`, `mean`, `stddev`, `quantile`, `minMax`, `direction`.

The example is the Hampel identifier: a sample is anomalous when it lies farther from the median than `threshold`
times the scaled median absolute deviation. Unlike the z-score it is robust, so several large spikes do not hide each
other.

`internal/processor/anomaly/hampel.go`:

```go
package anomaly

import (
	"fmt"
	"math"
	"sort"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// hampel flags samples farther from the median than `threshold` scaled
// median absolute deviations (the Hampel identifier; 1.4826·MAD estimates σ
// for normal data). It is robust: a few large spikes move neither the median
// nor the MAD, so they do not mask each other the way they do for zscore.
//
// Params: threshold (> 0, default 3).
// Stats: median, mad (already scaled), threshold. Score: |v − median| / mad.
// A series whose MAD is zero has no anomalies.
type hampel struct{}

func (hampel) Name() string { return "hampel" }

func (hampel) Validate(params map[string]any) error {
	if err := processor.CheckKeys(params, "threshold"); err != nil {
		return err
	}
	if processor.IsTemplate(params["threshold"]) {
		return nil // only known after rendering; checked again in Detect
	}
	_, err := hampelThreshold(params)
	return err
}

func hampelThreshold(params map[string]any) (float64, error) {
	t, err := processor.Float(params, "threshold", 3)
	if err != nil {
		return 0, err
	}
	if t <= 0 {
		return 0, fmt.Errorf("with.threshold: must be > 0, got %v", t)
	}
	return t, nil
}

func (hampel) Detect(params map[string]any, s Series) (Verdict, error) {
	t, err := hampelThreshold(params)
	if err != nil {
		return Verdict{}, err
	}
	v := values(s)
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	med := quantile(sorted, 0.5)
	dev := make([]float64, len(v))
	for i, x := range v {
		dev[i] = math.Abs(x - med)
	}
	sort.Float64s(dev)
	scale := 1.4826 * quantile(dev, 0.5)
	out := Verdict{Stats: map[string]float64{"median": med, "mad": scale, "threshold": t}}
	if scale == 0 {
		return out, nil
	}
	for i, x := range v {
		score := math.Abs(x-med) / scale
		if score > t {
			out.Anomalies = append(out.Anomalies, Anomaly{Index: i, Score: score, Expected: med, Direction: direction(x, med)})
		}
	}
	return out, nil
}
```

Register it with one line in the `detectors` table of `detector.go`. There is no `init()` and nothing to change in
`main.go`: `method: hampel` works in `anomaly` and `outliers` at once.

```go
var detectors = map[string]Detector{
	"zscore": zscore{},
	"iqr":    iqr{},
	"hampel": hampel{},
}
```

`internal/processor/anomaly/hampel_test.go` shows what the method catches and what it does not:

```go
package anomaly

import "testing"

func TestHampelDetect(t *testing.T) {
	d := hampel{}
	// A cycle 0.9, 1.0, 1.1 (median 1, MAD 0.1) with three spikes: the
	// median and the MAD ignore them, so all three are caught.
	vals := make([]float64, 30)
	for i := range vals {
		vals[i] = 0.9 + 0.1*float64(i%3)
	}
	vals[3], vals[17], vals[25] = 10, 12, -8
	v, err := d.Detect(map[string]any{}, Series{Points: toPoints(vals)})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Anomalies) != 3 || v.Anomalies[0].Index != 3 || v.Anomalies[2].Direction != DirectionDown {
		t.Fatalf("anomalies = %+v", v.Anomalies)
	}
	if v.Stats["median"] != 1 || v.Stats["threshold"] != 3 || v.Stats["mad"] <= 0 {
		t.Errorf("stats = %v", v.Stats)
	}
	// Constant series: MAD is zero, nothing is flagged.
	v, _ = d.Detect(map[string]any{}, Series{Points: toPoints(flat(10, 2, nil))})
	if len(v.Anomalies) != 0 || v.Stats["mad"] != 0 {
		t.Errorf("constant: %+v %v", v.Anomalies, v.Stats)
	}
	// Threshold from a rendered string and validation of the raw section.
	if v, _ = d.Detect(map[string]any{"threshold": "100"}, Series{Points: toPoints(vals)}); len(v.Anomalies) != 0 {
		t.Errorf("high threshold: %+v", v.Anomalies)
	}
	if err := d.Validate(map[string]any{"threshold": "{{ .s }}"}); err != nil {
		t.Errorf("template must be accepted at load: %v", err)
	}
	for name, p := range map[string]map[string]any{"zero": {"threshold": 0}, "text": {"threshold": "big"}, "unknown": {"k": 1}} {
		if err := d.Validate(p); err == nil {
			t.Errorf("%s: expected validate error", name)
		}
	}
}
```

`toPoints` and `flat` are helpers of the package's existing tests. Some existing tests pin the list of methods,
because users read it in error messages (`with.method: required (one of: hampel, iqr, zscore)`). They fail until you
add the new name:

- `TestDetectorsTable` and two messages in `TestValidate` in `internal/processor/anomaly/anomaly_test.go`;
- two messages in `TestValidate` in `internal/processor/outliers/outliers_test.go`.

## A detector for anomaly_mv: zdist

`anomaly_mv` judges each sample of a multivariate series as a whole. Its detectors have their own contract in
`internal/processor/anomalymv/detector.go`:

```go
type Point = series.MultiPoint     // {T float64; V []float64}: V[i] belongs to Dims[i]
type Series = series.MultiSeries   // {Labels; Dims []string; Points []Point; Reason}

type Anomaly struct {
    Index     int
    Score     float64
    Expected  []float64 // per dimension
    Deviation []float64 // per dimension, signed, in the method's units
}

type Verdict struct {
    Anomalies []Anomaly
    Stats     map[string]float64
    Skip      string    // non-empty: the series cannot be scored; reported as skipped
}

type Detector interface {
    Name() string
    Validate(params map[string]any) error
    Detect(params map[string]any, s Series, floor []float64) (Verdict, error)
}
```

The differences from single-series detectors:

- `Detect` is called with at least one sample and at least two dimensions. Series that `join` could not build, and
  short series, never reach it.
- `Expected` and `Deviation` have one value per dimension. The processor names the dimension with the largest
  `|Deviation|` as `dominant` in the report.
- **`Skip`** is for data the method cannot score: a singular covariance matrix, every dimension constant. Return a
  reason in `Skip` instead of an error, so one odd host does not fail a fleet-wide tool. Errors are for bad `params`.
- **`floor`** is the noise floor: `nil`, or one standard deviation per dimension that the detector must assume
  whatever the data shows. The processor derives it from `min_delta` and `min_rel_delta`. Add its square to each
  dimension's variance; then a nearly constant dimension no longer turns a negligible move into a huge deviation.
- Helpers in the package: `column`, `means`, `covariance`, `invert`, `quadratic`. Stats are named `mean_<dim>`,
  `stddev_<dim>` and `corr_<a>_<b>` by convention.

The example scores a sample by the length of its vector of per-dimension z-scores. It is the Mahalanobis distance
with the correlations left out: it misses a sample that only breaks the relation between metrics, but it never
degenerates on collinear inputs.

`internal/processor/anomalymv/zdist.go`:

```go
package anomalymv

import (
	"fmt"
	"math"

	"github.com/ocellus-ai/ocellusai-mcp/internal/processor"
)

// zdist scores every sample by the length of its per-dimension z-score
// vector: sqrt(Σ ((x_i − μ_i)/σ_i)²). It ignores the correlation between
// the inputs (a Mahalanobis distance with a diagonal covariance), so it only
// catches samples that are far on at least one axis, but it never degenerates
// on collinear inputs. A constant dimension contributes 0; when every
// dimension is constant the series is skipped. A noise floor is added to
// every dimension's variance, so a floored constant dimension is scored.
//
// Params: threshold (> 0, default 3).
// Stats: mean_<dim>, stddev_<dim>, threshold, max_distance.
type zdist struct{}

func (zdist) Name() string { return "zdist" }

func (zdist) Validate(params map[string]any) error {
	if err := processor.CheckKeys(params, "threshold"); err != nil {
		return err
	}
	if processor.IsTemplate(params["threshold"]) {
		return nil
	}
	_, err := zdistThreshold(params)
	return err
}

func zdistThreshold(params map[string]any) (float64, error) {
	t, err := processor.Float(params, "threshold", 3)
	if err != nil {
		return 0, err
	}
	if t <= 0 {
		return 0, fmt.Errorf("with.threshold: must be > 0, got %v", t)
	}
	return t, nil
}

func (zdist) Detect(params map[string]any, s Series, floor []float64) (Verdict, error) {
	t, err := zdistThreshold(params)
	if err != nil {
		return Verdict{}, err
	}
	n := len(s.Dims)
	mu := means(s)
	cov := covariance(s, mu)
	sigma := make([]float64, n)
	stats := map[string]float64{"threshold": t}
	live := 0
	for i, dim := range s.Dims {
		stats["mean_"+dim] = mu[i]
		stats["stddev_"+dim] = math.Sqrt(cov[i][i])
		f := 0.0
		if i < len(floor) {
			f = floor[i]
		}
		sigma[i] = math.Sqrt(cov[i][i] + f*f)
		if sigma[i] > 0 {
			live++
		}
	}
	if live == 0 {
		return Verdict{Stats: stats, Skip: "every input is constant"}, nil
	}
	v := Verdict{Stats: stats}
	maxD := 0.0
	for idx, p := range s.Points {
		dev := make([]float64, n)
		sum := 0.0
		for i := range dev {
			if sigma[i] > 0 {
				dev[i] = (p.V[i] - mu[i]) / sigma[i]
				sum += dev[i] * dev[i]
			}
		}
		d := math.Sqrt(sum)
		maxD = math.Max(maxD, d)
		if d > t {
			v.Anomalies = append(v.Anomalies, Anomaly{Index: idx, Score: d, Expected: append([]float64(nil), mu...), Deviation: dev})
		}
	}
	stats["max_distance"] = maxD
	return v, nil
}
```

The table entry in `anomalymv/detector.go`:

```go
var detectors = map[string]Detector{
	"mahalanobis": mahalanobis{},
	"zdist":       zdist{},
}
```

The test documents the trade-off against `mahalanobis` (`correlated` and `multi` are helpers of the package's
tests):

```go
package anomalymv

import (
	"strings"
	"testing"
)

func TestZDistDetect(t *testing.T) {
	xs, ys := correlated(40)
	xs = append(xs, 0.5, 2.0) // (0.5, 0.8) breaks the correlation but is in range on both axes
	ys = append(ys, 0.8, 2.0) // (2.0, 2.0) is far on both axes
	s := multi(map[string]string{"instance": "vm-1"}, []string{"cpu", "mem"}, xs, ys)
	v, err := (zdist{}).Detect(map[string]any{}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unlike mahalanobis, zdist sees only the second sample: that is the trade-off it documents.
	if len(v.Anomalies) != 1 || v.Anomalies[0].Index != 41 {
		t.Fatalf("anomalies = %+v", v.Anomalies)
	}
	a := v.Anomalies[0]
	if a.Score <= 3 || len(a.Deviation) != 2 || len(a.Expected) != 2 || a.Deviation[0] <= 0 {
		t.Errorf("anomaly = %+v", a)
	}
	for _, k := range []string{"mean_cpu", "stddev_mem", "threshold", "max_distance"} {
		if _, ok := v.Stats[k]; !ok {
			t.Errorf("stats missing %q", k)
		}
	}
	// Every dimension constant: skipped, not an error.
	flat := make([]float64, 20)
	v, err = (zdist{}).Detect(map[string]any{}, multi(nil, []string{"a", "b"}, flat, flat), nil)
	if err != nil || !strings.Contains(v.Skip, "constant") {
		t.Errorf("constant inputs: skip=%q err=%v", v.Skip, err)
	}
	// With a noise floor the same constant inputs are scored and have no anomalies.
	v, err = (zdist{}).Detect(map[string]any{}, multi(nil, []string{"a", "b"}, flat, flat), []float64{1, 1})
	if err != nil || v.Skip != "" || len(v.Anomalies) != 0 {
		t.Errorf("floored constant inputs: skip=%q anomalies=%v err=%v", v.Skip, v.Anomalies, err)
	}
	if err := (zdist{}).Validate(map[string]any{"threshold": "{{ .t }}"}); err != nil {
		t.Errorf("template must be accepted at load: %v", err)
	}
	if err := (zdist{}).Validate(map[string]any{"k": 1}); err == nil {
		t.Error("unknown key must fail")
	}
}
```

Update the pinned lists: `TestDetectorsTable` and the message `available: mahalanobis, zdist` in `TestValidate` of
`internal/processor/anomalymv/anomalymv_test.go`.

## A detector for the anomaly ensemble

`anomaly_ensemble` runs a set of detectors from `internal/tsanomaly` over every metric and over all metrics together,
gives each its own threshold and merges the flags into segments. A detector there scores points; it does not decide
what is anomalous:

```go
type UnivariateDetector interface {
    Name() string
    Score(x []float64) ([]float64, error)        // one score per point; higher = more anomalous
}

type MultivariateDetector interface {
    Name() string
    ScoreMulti(cols [][]float64) ([]float64, error) // cols[j] is metric j; one score per time index
}
```

To add one:

1. **Score.** Write the type in `internal/tsanomaly/<name>.go`. The input may contain `NaN` for gaps: fill them with
   `ImputeMask`, keep imputed points out of any window you compute, and give them score 0 with `maskScores`, so a
   gap never becomes an anomaly. `LevelShift` (`level.go`) is a compact model to follow. `Name()` is what the report
   shows in `detectors` and `fired_by`, by convention with its main parameter: `level(w=60)`.
2. **Say how to threshold it.** Implement `Calibrated() bool` returning `true` when the score is already a robust z
   (the ensemble will not rescale it). Implement `Subsequence() bool` returning `true` when the score is spread over
   a window, as for the Matrix Profile: such scores form plateaus, extreme-value fitting does not apply, and a fixed
   floor is used. A point-wise score that also forms plateaus is a judgement call; the comment on `LevelShift` records
   why it stayed point-wise.
3. **Add it to the ensemble.** In `Analyze` (`pipeline.go`), append it to the per-metric or the multivariate list,
   behind a new `Skip<Name>` field of `Options`.
4. **Expose it in the processor.** In `internal/processor/ensemble/ensemble.go`, add its short name to
   `detectorNames` (the values of `with.detectors`) and map it to the `Skip` field in `settings.options`.
5. **Test** the detector on synthetic data in `tsanomaly` (what it finds, what it ignores, gaps, short series) and
   the processor's report in `ensemble`.
6. **Measure on real data.** Thresholds come from the distribution of scores, so a detector that looks right on
   synthetic data can flag hundreds of segments on bursty real series. Compare the number and the kinds of segments
   with and without it on a real fleet before making it a default.

## Changing SSA

`ssa` and `ssa_mv` are thin processors over the library `internal/ssa`. A new way to model the noise, group
components or forecast belongs in the library, and one change serves both: a single series is analysed as MSSA with
one channel through the internal interface `decomp`.

- A new field in the report goes into `internal/processor/ssaproc/report.go`. A field with one value per channel is
  written through `job.perDim`, which gives a scalar in `ssa` and `{dim: value}` in `ssa_mv`.
- `TestAnalyzeTones` pins the numbers of the analysis on a synthetic series. If a change moves them, explain why in
  the change.
- Judge changes to the noise model and to the default forecast on real series: synthetic data does not reproduce
  bursts and outages, and those are where a noise model fails.

## Documentation

Update:

- [`docs/processors.md`](processors.md): a section for a new processor (what it does, its settings, its report, how
  to read it, its limits), a row in *Choosing a processor* and in *Cost*; for a new detector, its entry in the
  processor's *Methods* and settings;
- [`README.md`](../README.md): the processors table and the feature list;
- the processor list in [User guide](user-guide.md) and in [Components](components.md);
- [`skills/ocellus-tool-builder/reference/processors.md`](../skills/ocellus-tool-builder/reference/processors.md),
  so that agents writing tools know about it.

## Checklist

- [ ] The package comment names the kind (transformation or analysis), the input shape, the `with` keys and the
      output.
- [ ] `Validate` rejects unknown keys, checks literals and skips templates; `Run` checks the rendered values (one
      `parseSettings(with, load)` for both).
- [ ] Settings are read with `processor.Float`, `Int`, `Bool`, `String`, `Seconds`, `SpanOf`.
- [ ] Time series are read and written with the `series` package.
- [ ] The output is JSON-compatible, contains no `NaN` or infinities, and the input is not modified (a test
      `json.Marshal`s the result).
- [ ] Data problems are skipped series with a reason; wrong settings and wrong shapes are errors that name the key
      or the expected shape.
- [ ] `ctx` is checked between series.
- [ ] Registered: `buildProcessors` for a processor, the `detectors` table for a detector; pinned lists in tests
      updated (`TestBuildProcessors`, `TestDetectorsTable`, the method lists in `TestValidate`).
- [ ] `referenceProcessors()` in `internal/server/server_test.go` includes the processor if a catalog tool uses it.
- [ ] An example tool passes `-validate`, and a tool test runs it through the catalog and the pipeline.
- [ ] Documentation updated.
- [ ] `make vet test-race lint validate` passes.

## Common mistakes

- **Reading a number with a type assertion.** `with["k"].(float64)` fails when the tool writes
  `k: "{{ .sensitivity }}"`, which arrives as the string `"1.5"`.
- **Checking a template in `Validate`.** A valid tool is rejected at startup. Skip templated values at load time.
- **A selector in `with`.** Keys such as `items` or `inputs` that pick parts of the input turn into a small query
  language in every processor. Shape the input with a `jq` step, or with `join` for several matrices.
- **Go types in the output.** `[]float64`, `map[string]float64` or a struct breaks jq and the next processor. Build
  `[]any` and `map[string]any`.
- **`NaN` in the report.** A division by zero on a constant series ends as a failed call at stage `encode`. Guard
  degenerate cases and write `nil` for undefined numbers.
- **An error instead of a skip.** One constant or empty series fails the whole tool for every host. Report it as
  skipped with a reason, or return `Verdict.Skip` from a multivariate detector.
- **A composite literal in an `if`.** `if v, err := zdist{}.Detect(…); …` does not compile; write `(zdist{}).Detect`.
- **Forgetting the pinned lists.** `TestBuildProcessors` and the detector tests list every name on purpose; update
  them with the new name rather than loosening them.
- **Masking.** The mean and the standard deviation absorb a large spike, so a classic method may miss one outlier
  among thirty samples. That is a property of the method, not a bug: test with longer series, use a robust method,
  and say it in the detector's comment.
- **The z-score on a handful of items.** In `outliers` the z-score of n items cannot exceed (n−1)/√n, so with six
  items a threshold of 3 never fires. Use `iqr`, or a robust method, for small sets.
