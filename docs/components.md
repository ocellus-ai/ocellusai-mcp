# Components

[Architecture](architecture.md) · **Components** · [Writing a worker](writing-workers.md) · [Writing a processor](writing-processors.md)

This guide walks through the packages of Ocellus AI one by one: what each is for, its main types and functions, the
rules it keeps and how it is tested. Read [Architecture](architecture.md) first for the picture they fit into. What
the workers and processors do from a user's point of view is in [Workers](workers.md) and
[Processors](processors.md); this guide is about how they are built.

## Contents

- [cmd/ocellusai-mcp](#cmdocellusai-mcp)
- [internal/config](#internalconfig)
- [internal/catalog](#internalcatalog)
- [internal/template](#internaltemplate)
- [internal/transform](#internaltransform)
- [internal/pipeline](#internalpipeline)
- [internal/server](#internalserver)
- [internal/worker](#internalworker)
  - [prometheus](#prometheus)
  - [shell](#shell)
  - [rest](#rest)
- [internal/processor](#internalprocessor)
  - [series](#series)
  - [jq](#jq)
  - [join](#join)
  - [anomaly](#anomaly)
  - [outliers](#outliers)
  - [anomaly_mv](#anomaly_mv)
  - [anomaly_ensemble](#anomaly_ensemble)
  - [cluster_events](#cluster_events)
  - [ssa and ssa_mv](#ssa-and-ssa_mv)
- [internal/tsanomaly](#internaltsanomaly)
- [internal/ssa](#internalssa)
- [Tool catalogs and test data](#tool-catalogs-and-test-data)
- [Scripts, images and the agent skill](#scripts-images-and-the-agent-skill)
- [Building, testing and linting](#building-testing-and-linting)
- [Where to look](#where-to-look)

## cmd/ocellusai-mcp

The entry point. `main.go` parses the flags, loads the config, builds the workers and processors, loads the
catalog and runs the server ([Startup](architecture.md#startup) lists the order).

| Function | Role |
|---|---|
| `run` | the whole startup; returns an error instead of exiting, so tests can call the parts |
| `buildWorkers` | one worker per enabled instance, registered under the instance name |
| `newWorker` | the factory: a `switch` on the instance type that calls the type's constructor. A new worker type adds one `case` here |
| `buildProcessors` | registers every processor. A new processor adds one `Register` call here |
| `callNames`, `stepNames` | the `calls=` and `process=` columns of the `-validate` table |
| `newLogger` | `slog` handler for stderr, JSON or text |

The version is set at build time: `-ldflags "-X main.version=…"` (the Makefile uses `git describe`).

Tests (`main_test.go`) check that instances of one type become distinct workers, that disabled entries are skipped,
that a config without instances fails, that a constructor error names the instance, and the exact list of
registered processors. That list is pinned on purpose: adding a processor means updating the test.

## internal/config

Loads `config.yaml` into a `Config`:

```go
type Config struct {
    Server   Server  // Transport (stdio | http), Listen
    ToolsDir string
    Workers  Workers // instance name → *Worker
    Log      Log     // Level, Format
}
```

`Load` reads the file and calls `Parse`, which:

1. decodes the YAML into a node tree and replaces `${VAR}` in scalar values (not in keys or comments) with
   environment variables (`expandNode`); a plain scalar is re-typed after the replacement, so `${PORT}` can become a
   number; unset variables are one error that lists all of them;
2. decodes the tree into `Default()` with `KnownFields(true)`, so an unknown field is an error;
3. applies per-type defaults (`applyDefaults`) and checks everything (`Validate`).

`Default()` uses the stdio transport, `:8080`, `./tools` and `info`/`json` logs. The example configs use HTTP.

**Worker instances.** `Workers` has its own `UnmarshalYAML`. For each entry it cuts out the `type` key (defaulting to
the entry name), picks the settings struct of that type (`Prometheus`, `Shell`, `Rest`) and decodes the remaining
keys into it. Because `yaml.Node.Decode` ignores unknown keys, `checkFields` compares them with the struct's `yaml`
tags first, which produces messages like
`workers.am: unknown field(s) allowlist (allowed: ca_file, headers, …) (type rest)`. An entry with no value is
disabled and skipped by `Names()`. `Worker.validate` checks each type's required fields; instance names must match
`^[A-Za-z0-9_-]+$`. Checks that need the type's own code (is the URL well formed, can the CA file be read) are left to
the worker's constructor.

The set of types is closed: `WorkerTypes`, one struct and one pointer field per type, one `case` in `decodeWorker`,
`applyDefaults` and `validate`. [Writing a worker](writing-workers.md#4-the-config-section) shows the change for a new
type.

Tests cover defaults, `${VAR}` (including unset variables and variables in comments), unknown fields at every
level, every validation error and each worker type's settings.

## internal/catalog

Turns tool files into compiled `Tool` values.

`Load(dir, workers, processors)` reads every `*.yaml` and `*.yml` file of the directory in name order (an empty
directory is an error), parses each with `Parse` and rejects two tools with one name, naming both files. `Parse`
prefixes every error with the file path.

**The file format** is `Spec`: `name`, `description`, `worker`, `params`, `request`, `calls`, `process`, `response`.
Decoding is strict at every level: `Params`, `Steps` and `Calls` have their own `UnmarshalYAML` that keeps the YAML
order of parameters and decodes each item with unknown fields rejected.

**Compilation** (`parse`):

| Step | Function | Checks |
|---|---|---|
| Name and description | `parse` | name matches `^[a-z0-9_]+$`; description is not empty |
| Call form | `callList` | either `worker` + `request` (one unnamed call) or `calls`, not both |
| Input schema | `buildSchema` | parameter names, types, `required` vs `default`, `pattern` only for strings, `minimum`/`maximum` only for numbers; the schema is resolved with defaults validated against it |
| Calls | `compileCalls` | call names (`^[A-Za-z_][A-Za-z0-9_]*$`, unique), the instance exists, `Worker.Validate(request)`, request templates, per-call jq |
| Process | `compileSteps` | the processor exists, `Processor.Validate(with)`, `with` templates |
| Response | `parse` | `response.jq` compiles, `response.render` parses |
| References | `checkResultRefs` | every literal `result "x"` names a call that runs earlier |

The result:

```go
type Tool struct {
    Spec   Spec
    File   string
    Schema *jsonschema.Schema   // the MCP inputSchema
    Calls  []CompiledCall       // Name ("" in the single-call form), WorkerName, Worker, Request, JQ
    Steps  []CompiledStep       // Fn, Processor, With
    JQ     *transform.Program   // response.jq, nil when absent
    Render *template.Template   // response.render, nil when absent
}
```

`Named()` tells the two call forms apart; `WorkerNames()` and `WorkerTypes()` list the instances and types a tool
uses, for logs and `-validate`.

**The input schema** has one property per parameter in YAML order, `required` for required parameters, defaults,
and `additionalProperties: {not: {}}`, so unknown arguments are rejected.

**`BindArgs(raw)`** is the start of every call: decode the JSON arguments, validate them against the resolved
schema, fill in defaults (deep copies, so a call cannot change a default), set declared parameters without a value
to `nil` and turn integer parameters from `float64` into `int`.

Tests use temporary directories and fake workers and processors whose `Validate` can be set per test; they check
every error message by substring, including the file name and the field.

## internal/template

Go `text/template` with the Ocellus helpers, used for requests, `with` sections and the answer text.

- `Parse(name, text)` compiles a template with `FuncMap()` and `missingkey=error`, so a reference to a key that does
  not exist fails the call instead of printing `<no value>`.
- `Template.Execute(data, params, results)` clones the compiled template and binds `param` and `result` to the
  call's values. Cloning keeps the shared template untouched, so concurrent calls do not interfere.
- `FuncMap()` is sprig's text function map plus `promLabel`, `promRegex`, `promDuration`, `promDurationSeconds`,
  `pathEscape`, and placeholders for `param` and `result` that fail outside execution. A new helper goes here, with a
  test and a line in [Templates and jq](templates-and-jq.md#helpers-added-by-ocellusai-mcp).
- `CompileTree(name, v)` compiles a request or `with` section: every string that contains `{{` becomes a template,
  other values stay literals. `Tree.Render(params, results)` returns a copy with every template executed; the root
  `.` of those templates is the parameter map.
- `Template.Refs(fn)` and `Tree.Refs(fn)` walk the parsed templates and collect literal first arguments of a function
  (`result "addrs"`), which the catalog uses to check references at startup. Dynamic arguments are not collected.

Tests cover each helper's escaping, `param` and `result` errors (with the list of available names), the reference
walk over every kind of template node, and rendering of nested trees.

## internal/transform

jq over Go values, with [gojq](https://github.com/itchyny/gojq).

- `Compile(expr)` parses and compiles once, with `$params` declared as a variable. A `Program` is safe for concurrent
  use.
- `Program.Run(ctx, input, params)` runs over normalized input with the arguments bound to `$params`. No output gives
  `nil`, one output is returned as is, several are collected into an array. A runtime error is returned as
  `jq: …`.
- `Normalize(v)` converts Go values into the set gojq accepts (see
  [Data between stages](architecture.md#data-between-stages)).

`response.jq`, `calls[i].jq` and the `jq` processor all go through this package.

## internal/pipeline

Runs one tool call ([A tool call](architecture.md#a-tool-call) describes the flow).

- `Pipeline.Run(ctx, tool, raw)` returns `Result{Text, Structured}` or a `*pipeline.Error`.
- `runCall` renders a request, executes the worker and the call's jq; in the `calls` form errors are prefixed with
  `calls[i] (name): `.
- `runStep` renders a step's `with` and runs the processor; errors are prefixed with `process[i] (fn): `.
- `Error{Stage, Err}` names the stage: `StageArguments`, `StageRequest`, `StageWorker`, `StageProcess`, `StageJQ`,
  `StageRender`, `StageEncode`.
- `PrettyJSON` prints the result with two-space indentation and without HTML escaping, so `<` and `&` in values stay
  readable.

At `debug` level it logs every rendered request and every finished step.

Tests use fake workers and a wrapping processor that tags its input, so the order of steps, the rendering of `with`
and the `{name: value}` object of the `calls` form are visible in the result. They also check that a failed call
stops the tool before the next worker is called.

## internal/server

The MCP side, on the official Go SDK.

- `New(tools, pipeline, Options)` creates an `mcp.Server` and registers one `mcp.Tool{Name, Description,
  InputSchema}` per catalog tool, each with its own handler.
- The handler runs the pipeline, builds the result (text content plus `structuredContent`, wrapped as
  `{"result": …}` when it is not an object) or an `isError` result with `tool <name> failed at stage <stage>: …`,
  and logs one line with a random `request_id`.
- `RunStdio(ctx, srv)` serves one session over stdin and stdout.
- `HTTPHandler(srv, logger)` serves Streamable HTTP at `/mcp` (every session uses the same server) and `/healthz`,
  which always answers `ok` and does not check the data sources.
- `RunHTTP(ctx, listen, handler)` runs the HTTP server with a 10-second header timeout and shuts it down gracefully,
  within 10 seconds, when the context ends.

The tests are the integration tests of the project. They load real tool files, connect a real SDK client through
in-memory transports and call tools end to end:

- `referenceWorkers` starts `httptest` servers for Prometheus (`promMock`) and Alertmanager (`amMock`) and creates real
  `prometheus`, `shell` and `rest` workers pointing at them; `fakeDig` puts a script named `dig` first on `PATH`
  (it must run before the shell worker is created, which captures `PATH`). `amMock` answers 401 to any
  request without its bearer token, and the `rest` instance sends it as an instance header.
- `TestRestInstanceToken` takes the token from `${AM_TOKEN}` in a parsed config through the `rest` instance to the
  mock, and checks that a missing or wrong token is a worker-stage error that does not echo the secret.
- `TestReferenceCatalogListTools` and `TestReferenceCatalogCallTools` load `tools-test/` and check the schemas, the
  three response levels, the analysis tools, the shell and rest tools and the error texts.
- `TestWorkingCatalogLoads` loads `tools/` with the same workers and `referenceProcessors()`, so every tool of the
  working catalog must compile.

`promMock` answers by matching the query text, and the order of its cases matters. A new reference tool needs a
branch there and a case in `TestReferenceCatalogCallTools`.

## internal/worker

The worker contract and what all workers share.

```go
type Worker interface {
    Type() string
    Validate(req map[string]any) error
    Execute(ctx context.Context, req map[string]any) (any, error)
}
```

- `Registry` maps instance names to workers; `Register` rejects duplicates, `Names` is sorted.
- `CheckKeys(req, allowed...)` reports unknown request keys with the list of allowed ones.
- `String`, `RequiredString` read string fields with `request.<key>: …` errors.
- `IsTemplate(s)` reports whether a string still contains `{{`, i.e. can only be checked after rendering.

[Writing a worker](writing-workers.md) explains the contract in detail.

### prometheus

Instant and range queries against the Prometheus HTTP API, with the official client (`client_golang`).

- `New(Config{URL, Timeout, Headers, Logger})`. Static headers are added by a small `http.RoundTripper`, so they are
  never in the request map or the logs.
- `Validate` allows `type`, `query`, `time`, `start`, `end`, `step` and rejects keys of the other query type.
- `Execute` applies the timeout per query. Times accept `now`, `now-5m`, `now+1h`, RFC 3339 and unix seconds
  (`parseTime`); `step` accepts a duration or plain seconds (`parseStep`). API errors become
  `prometheus <type> error: …`; Prometheus warnings are logged, not returned.
- `Normalize(model.Value)` turns the client's result into the JSON of the HTTP API: `resultType` plus `result`,
  timestamps as float seconds and values as strings, native histograms under `histogram`/`histograms`. Keeping the
  API's shape lets jq written for Prometheus work unchanged and makes the matrix the canonical time series.
- The unexported field `now` is replaced in tests to fix the clock.

Tests run against an `httptest` server that answers like the API (vector, matrix, scalar, errors, slow answers).

### shell

Runs a program from the instance's allowlist and turns its stdout into data.

- `New(Config{Allowlist, Timeout, MaxOutputBytes, Env, Logger})` builds the child environment once: `PATH` (the
  server's own unless `Env` sets it) plus `Env`, sorted.
- `Validate` allows `command`, `args`, `stdin`, `parse`, `timeout`; the command must be on the allowlist, both at
  startup and on every call.
- `Execute` uses `exec.CommandContext` without a shell, so every element of `args` is one argument. Stdout is read
  into a buffer that stops at `max_output_bytes` without failing the write (failing it would kill the program with
  SIGPIPE); stderr is kept up to 64 KiB for error messages. The context's deadline or cancellation kills the
  program, and `WaitDelay` (2 seconds) bounds the wait for its output pipes after that, so a child that keeps them
  open cannot hang the call.
- `Parse(mode, …)`: `json` (empty → `null`; not JSON → the text as a string and a warning; a truncated object gets
  `truncated: true`), `lines`, `raw`.

Tests use `sh`, `echo`, `printf`, `cat`, `sleep` and `true`, and check exit codes, timeouts, cancellation, truncation
and that variables of the server's environment do not leak into the child.

### rest

One HTTP request to a JSON API under the instance's base URL.

- `New(Config{URL, Timeout, Headers, MaxResponseBytes, Methods, CAFile, InsecureSkipVerify, Logger})` parses the base
  URL (`ParseBaseURL`: http or https, a host, no query or fragment), builds a transport with the TLS options and a
  client that does not follow redirects.
- `Validate` checks every literal: the method (a literal, allowed by the instance), the path (`checkPath`: no `?`,
  `#`, scheme or host), `query`, `headers`, `body` (not with `GET`; valid JSON when the content type is JSON),
  `parse`, `result`, `ok_status`, `timeout`.
- `Execute` joins the path to the base URL and refuses a result outside the base path (`buildURL`), encodes `query`
  (values that render empty are dropped), sets headers in the order defaults < tool < instance, reads at most
  `max_response_bytes + 1` bytes, checks the status (2xx or `ok_status`), parses the body and returns either the body
  or `{status, headers, body}`.
- `requestError` turns transport errors into `METHOD /path: …` messages without the full URL.

Tests start `httptest` servers (plain and TLS) that record the last request, and cover every request key, every
status path, the limits, timeouts and the absence of secrets in errors.

## internal/processor

The processor contract and the helpers for reading `with`.

```go
type Processor interface {
    Name() string
    Validate(with map[string]any) error
    Run(ctx context.Context, with map[string]any, data any, params map[string]any) (any, error)
}
```

`Registry` maps names to processors. The helpers:

| Helper | Reads |
|---|---|
| `CheckKeys(with, allowed...)` | reports unknown keys: `with: unknown field(s) …` |
| `IsTemplate(v)` | a string that still contains `{{` |
| `Float`, `Int`, `String`, `Bool` | a setting from a number or a rendered string; an unrendered template is an error |
| `Seconds` | seconds as a number or a Prometheus duration (`30s`, `5m`) |
| `Span`, `SpanOf` | a length in points or as a duration, resolved by the series' step |
| `ParseDimFloats`, `DimFloats` | one number for every dimension or `{dim: number}` |
| `Number` | one data value: a number or a numeric string |
| `JSONType` | the JSON type name of a value, for error messages |
| `NoiseFloorShare` | the share (¼) of the smallest meaningful change used as a noise floor |

The processors are listed below by package. [Processors](processors.md) describes their settings and reports for
users; [Writing a processor](writing-processors.md) explains how to add one.

| `fn` | Package | Kind | Input | Output |
|---|---|---|---|---|
| `jq` | `jq` | transformation | anything | anything |
| `join` | `join` | transformation | `{call: matrix}` | multivariate series |
| `anomaly` | `anomaly` | analysis | matrix | report |
| `outliers` | `outliers` | analysis | list of `{key, value, labels}` | report |
| `anomaly_mv` | `anomalymv` | analysis | multivariate series, ≥ 2 dimensions | report |
| `anomaly_ensemble` | `ensemble` | analysis | multivariate series, ≥ 1 dimension | report |
| `cluster_events` | `clusterevents` | analysis | list of events | report |
| `ssa`, `ssa_mv` | `ssaproc` | analysis | matrix; multivariate series | report |

### series

The canonical time-series shapes and their Go types. Every processor that works on time series uses them; none has
its own.

| Shape | JSON | Go | Read / write |
|---|---|---|---|
| Scalar series | Prometheus matrix: `{resultType: matrix, result: [{metric, values: [[ts, "v"]]}]}` | `[]Series{Labels, Points []Point{T, V}}` | `ParseMatrix` / `ToMatrix` |
| Multivariate series | `{dims: [...], series: [{labels, points: [[ts, v1, v2]], reason?}]}` | `Multi{Dims, Series []MultiSeries{Labels, Dims, Points []MultiPoint{T, V}, Reason}}` | `ParseMulti` / `ToMulti` |

- Values may be numbers or numeric strings; samples with `NaN` or infinite values are dropped.
- Errors name the place and the expected shape, and point to a `jq` step for other sources.
- `ToMatrix` writes values as strings, like the API, so jq written for Prometheus keeps working after a
  transformation.
- A multivariate series without points can carry a `reason`: a transformation that could not build an object says
  why, and an analysis reports it as skipped. Unknown keys of a series are ignored, so a transformation may add
  diagnostics (`join` adds `input_points`); `ToMulti` takes them as `extra`.
- `Regular(points, dims, step)` lays samples on a regular grid (the step is inferred from the smallest interval
  unless given), leaves empty slots as `NaN` and counts them, and refuses samples that are not on any grid. Both the
  ensemble and SSA use it. `ScalarPoints` adapts a scalar series to it.
- `FormatTime` (RFC 3339 UTC) and `LabelsToAny` are shared by the reports.

### jq

`with.expr` must be a literal: it is compiled by `Validate` at startup and cached by its text under a mutex, so the
cache is bounded by the number of `jq` steps in the catalog. Arguments are available as `$params`, as in
`response.jq`. Output follows `response.jq`.

### join

Stitches several matrices, one per call, into one multivariate series per object.

- `with.inputs` (call names; they become `dims`, in order) and `with.join_by` (labels that identify an object);
  templated list items are checked at call time.
- `indexInput` groups each input's series by the `join_by` labels (two series with one key in an input are an
  error that suggests `sum by (…)`); `joinGroups` and `joinPoints` do an inner join by exact timestamp in the order of
  the first input.
- Objects that cannot be joined are not lost: a series without a `join_by` label, or a key missing from some input,
  becomes a series without points and with a `reason`. Every series gets `input_points` with the number of samples
  each input had.

### anomaly

Point anomalies in every series of a matrix. The processor owns the common settings (`method`, `min_points`,
`direction`, `max_anomalies`, `min_delta`, `min_rel_delta`), parsing, filtering, the cap and the report; the
algorithms are `Detector` implementations in a table:

```go
var detectors = map[string]Detector{
    "zscore": zscore{},
    "iqr":    iqr{},
}
```

- `parseSettings(with, load)` is shared by `Validate` (with `load` true, templated values are skipped) and `Run`. Every
  key that is not a common setting goes to the detector, which validates it itself.
- `method` must be a literal: the detector has to be known at startup to check its keys.
- `MinDelta` (`mindelta.go`) drops anomalies whose distance from the detector's expected value is smaller than
  `max(min_delta, min_rel_delta·|expected|)`; `outliers` uses it too.
- `Lookup(method)` exposes the table to other processors.
- `values`, `mean`, `stddev`, `quantile`, `minMax` and `direction` are helpers for detectors.

### outliers

Outliers in a set without a time axis. `parseItems` reads the canonical list `[{key, value, labels}]`; `groupItems`
splits it by `group_by` labels (items missing a label become one skipped series); every group is handed to an
`anomaly` detector (`anomaly.Lookup`) as a series whose `T` is the item's index. The report lists anomalies by
severity. It inherits the limits of the detectors: the z-score of n items can never exceed (n−1)/√n.

### anomaly_mv

Samples that break the relation between metrics. The structure mirrors `anomaly` with a vector detector contract:

```go
type Detector interface {
    Name() string
    Validate(params map[string]any) error
    Detect(params map[string]any, s Series, floor []float64) (Verdict, error)
}
```

`Series` is `series.MultiSeries`. A `Verdict` has anomalies with a score, the expected vector and the deviation per
dimension, statistics, and `Skip`: a non-empty reason marks the series as skipped instead of failing the tool (a
singular covariance matrix is a property of the data, not an error). `floor` is the noise floor per dimension derived
from `min_delta` and `min_rel_delta`; `mahalanobis` adds its square to the diagonal of the covariance matrix.
`detector.go` has the linear-algebra helpers (`means`, `covariance`, `invert`, `quadratic`).

### anomaly_ensemble

Anomalous segments found by the ensemble of `internal/tsanomaly`, with attribution to metrics.

- Every series goes onto a regular grid (`series.Regular`); gaps stay `NaN` and are imputed and masked by the
  detectors.
- `settings` (`with`) map to `tsanomaly.Options`: `window`, `season`, `min_len` and `merge_gap` accept points or
  durations and are resolved by the step; `detectors` turns detectors off (`detectorNames`); `risk` or `z_threshold`
  choose the thresholding.
- The noise floor (`min_delta`, `min_rel_delta`) is applied as deterministic Gaussian noise added to the detector
  input (`grid.dithered`, seeded by the labels and the dimension); the report shows raw values.
- `analyze` runs `tsanomaly.Analyze` in a goroutine and returns when the context ends.
- The report converts library ranges into segments with times, values at the peak, baselines, the detectors that
  fired (`fired_by`) and, for several metrics, the kind and `top_metrics`.

### cluster_events

Folds events of many series that overlap in time into incidents. `parseEvents` reads the event list (times as unix
seconds or RFC 3339; the series key is the JSON of its labels); `cluster` sorts by start and links events that overlap
or lie at most `gap` apart with a union-find structure, optionally only when they share a signal; `capClusters` keeps the largest clusters and
returns them in time order. Fields it does not know are passed through.

### ssa and ssa_mv

Two processors in one package (`NewUni`, `NewMulti`) because they share settings, the grid and the report. For every
series: grid (`series.Regular`) → linear interpolation of gaps (`tsanomaly.Impute`; too many gaps → skipped) → window
and number of components → decomposition (`ssa.Decompose`, or `ssa.DecomposeMulti` with per-channel weights for
`ssa_mv`) → `Analyze` → check of the groups → optional forecast. Series are analysed in parallel (`parallel`, as many
goroutines as `GOMAXPROCS`), and the processor returns when the context ends.

- `settings.go`: the `with` keys.
- `ssaproc.go`: `Run`, `parallel`, the per-series `job`, the normalization scales of `ssa_mv`.
- `report.go`: the spectrum, the noise, the components, the groups and their check, the stretches outside the
  usual profile, the findings and the forecast. A field that has one value per channel is written through
  `job.perDim`: a scalar in `ssa`, `{dim: value}` in `ssa_mv`.

## internal/tsanomaly

A library for batch anomaly detection, used by `anomaly_ensemble`. It knows nothing about YAML or MCP.

| Piece | Files |
|---|---|
| Frames and gaps: `Frame`, `Align`, `Impute`, `ImputeMask` | `frame.go`, `util.go` |
| Detector interfaces: `UnivariateDetector` (`Score(x)`), `MultivariateDetector` (`ScoreMulti(cols)`); one score per point, higher is more anomalous | `detector.go` |
| Matrix Profile (STOMP) and the multidimensional one (mSTOMP) | `matrixprofile.go`, `mp_core.go` |
| AR residual, seasonal residual, level shift | `residual.go`, `seasonal.go`, `level.go` |
| Isolation Forest, PCA (SPE and Hotelling T²) | `iforest.go`, `pca.go` |
| Combining scores, peaks-over-threshold thresholds, ranges | `ensemble.go` |
| Attribution of a multivariate range to metrics | `attribution.go` |
| The whole analysis: `Analyze(frame, Options) → Report` | `pipeline.go` |

Two optional methods change how the ensemble treats a detector: `Calibrated() bool` says the score is already a
robust z (it is not rescaled), `Subsequence() bool` says the score is spread over a window (a plateau, where
extreme-value fitting does not apply, so a fixed floor is used). The set of detectors is assembled in `Analyze`, with
a `Skip…` option for each. Imputed points get score 0 (`maskScores`), so a gap is never an anomaly.

The library does not take a context and uses gonum for linear algebra; random numbers are seeded.

## internal/ssa

Singular Spectrum Analysis of one series and of several series together (MSSA), used by `ssa` and `ssa_mv`.

| Piece | Files |
|---|---|
| Decomposition: full (eigenvectors of the lag-covariance matrix) or the k leading components (randomized subspace iteration with products through FFT); reconstruction by diagonal averaging; w-correlations | `ssa.go`, `hankel.go` |
| MSSA with channel weights | `mssa.go` |
| Recurrent (L) and vector (K) forecasts, ESPRIT roots | `forecast.go`, `esprit.go` |
| `Analyze`: AR(1) noise model with a Monte Carlo floor, classification of components (trend, harmonic pairs with periods, slow cycles), suggested groups, findings, `Forecastable` | `analysis.go` |
| Stretches outside the usual daily profile: other regimes, daily jobs, bursts | `runs.go` |
| `Check`: separation of groups and what is left out | `check.go` |
| Group syntax (`"0;1-2;3,5-7"`), statistics | `groups.go`, `stats.go` |

One analysis serves both SSA and MSSA through the internal interface `decomp`: a single series is MSSA with one
channel of weight 1. A change to the analysis is therefore made once. `TestAnalyzeTones` pins the numbers on a
synthetic series (a noise floor of 29.6 and a noise σ of 0.4046); changes to the noise model are better judged on a
real fleet than on synthetic data, which does not reproduce bursty series.

## Tool catalogs and test data

| Place | What | Who loads it |
|---|---|---|
| `tools/` | the working catalog | `config.example.yaml`, the Docker image, the Kubernetes manifest, `TestWorkingCatalogLoads` |
| `tools-test/` | small reference tools, one feature each | `TestReferenceCatalogListTools`, `TestReferenceCatalogCallTools` against the mocks |
| `internal/processor/*/testdata/` | tools with `calls` that end-to-end tests run through `catalog` and `pipeline` with a fake worker | the processor's `e2e_test.go` |

Rules that follow from the tests:

- A tool in `tools/` must load with the instances `prometheus`, `shell` and `rest` and the processors of
  `referenceProcessors()` in `internal/server/server_test.go`. A tool that uses a new processor needs the processor
  added there; a tool that uses a new worker type needs an instance of it in `referenceWorkers`.
- A tool in `tools-test/` is called by the tests, so it needs a branch in the mock it queries and a case in
  `TestReferenceCatalogCallTools`.

## Scripts, images and the agent skill

- **`scripts/mcp-call.py`**: a stdio MCP client in the Python standard library. It starts the binary
  (`-bin`, default `./bin/ocellusai-mcp`) with a stdio config and lists tools (`list`) or calls one
  (`call <tool> '<json>'`, `--structured` to print `structuredContent`, `-quiet` to hide the server's logs). It is
  the quickest way to try a tool by hand.
- **`Dockerfile`**: a multi-stage build. The default target is a distroless static image running as non-root with
  `tools/` and `config.example.yaml`. It has no command-line programs, so the shell worker needs an image of your
  own.
- **`deploy/k8s.yaml`**: a ConfigMap with the config, a Deployment without a service account token and a
  Service. The tool catalog is a second ConfigMap built from `tools/`.
- **`skills/ocellus-tool-builder/`**: an Agent Skill that lets an AI agent interview a user, write a tool file,
  validate it and try it. Its `reference/` pages summarize the tool format and the processors, and its `templates/`
  are tool files that pass `-validate`. When the tool format or a processor's settings change, update the skill too.

## Building, testing and linting

| Command | Does |
|---|---|
| `make build` | a static binary in `bin/ocellusai-mcp`, version from `git describe` |
| `make validate` | build and load `config.example.yaml` with its catalog (`-validate`) |
| `make test` | `go test -count=1 ./...` |
| `make test-race` | the same with the race detector |
| `make vet`, `make lint` | `go vet`; `golangci-lint` v2 with `.golangci.yml` |
| `make fmt` | `gofmt` |
| `make docker` | the container image |

The linters are the `standard` set plus `errorlint`, `gocritic`, `misspell`, `revive`, `unconvert` and `unparam`;
imports are grouped as standard library, third party, then `github.com/ocellus-ai/ocellusai-mcp/...`. Before sending a
change, run:

```bash
make vet test-race lint validate
```

Most packages test in under a few seconds. Under the race detector the dense linear algebra of `internal/ssa` and
`internal/processor/ssaproc` makes each of them take about a minute.

Test patterns used across the project:

| Layer | Pattern |
|---|---|
| HTTP workers | `httptest` servers that answer like the real API and record what they received |
| `shell` | real small programs (`sh`, `printf`, `sleep`), and a fake `dig` script on `PATH` |
| Catalog, pipeline | temporary directories, fake workers and processors |
| Processors and detectors | synthetic series with known anomalies; the detector directly through `Detect`, the processor through `Run` |
| End to end | a tool file in `testdata/`, `catalog.Parse` or `Load`, `pipeline.Run` with a fake worker |
| Server | real tool files, mocks, an SDK client over in-memory transports |

## Where to look

| Question | Place |
|---|---|
| What happens during a call | `internal/pipeline/pipeline.go: Run`, then `internal/catalog/catalog.go: BindArgs` |
| How the input schema is built | `internal/catalog/catalog.go: buildSchema` |
| How a tool file is compiled | `internal/catalog/catalog.go: parse, compileCalls, compileSteps, checkResultRefs` |
| Template functions | `internal/template/template.go: FuncMap` |
| How errors reach the agent | `internal/server/server.go: handler` |
| `${VAR}` in the config | `internal/config/config.go: expandNode` |
| Worker instances and their types | `internal/config/config.go: Workers.UnmarshalYAML, decodeWorker`; `cmd/ocellusai-mcp/main.go: newWorker` |
| The Prometheus answer format | `internal/worker/prometheus/prometheus.go: Normalize` |
| How `shell` runs a program | `internal/worker/shell/shell.go: Execute, Parse` |
| How `rest` builds a URL | `internal/worker/rest/rest.go: buildURL, checkPath, queryValues` |
| Reading `with` | `internal/processor/processor.go` |
| Time-series shapes and the grid | `internal/processor/series/series.go`, `grid.go` |
| Detector tables | `internal/processor/anomaly/detector.go`, `internal/processor/anomalymv/detector.go` |
| The ensemble's detectors | `internal/tsanomaly/pipeline.go: Analyze` |
| The SSA analysis | `internal/ssa/analysis.go: Analyze`, `runs.go`, `check.go` |
