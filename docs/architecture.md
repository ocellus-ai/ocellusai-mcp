# Architecture

**Architecture** · [Components](components.md) · [Writing a worker](writing-workers.md) · [Writing a processor](writing-processors.md)

This guide is for people who want to work on Ocellus AI itself: fix a bug, connect a new kind of data source or add a
new kind of analysis. It explains how the server is put together, what happens when it starts and when an agent
calls a tool, and which rules hold the parts together. Each package is described in [Components](components.md);
the two extension points have their own guides, [Writing a worker](writing-workers.md) and
[Writing a processor](writing-processors.md).

The guide assumes you know what a tool file looks like. If not, start with the user documentation:
[User guide](user-guide.md) and [Writing tools](writing-tools.md).

## Contents

- [In one picture](#in-one-picture)
- [Design principles](#design-principles)
- [Startup](#startup)
- [A tool call](#a-tool-call)
- [Data between stages](#data-between-stages)
- [Load time and call time](#load-time-and-call-time)
- [Types, instances and registries](#types-instances-and-registries)
- [Errors](#errors)
- [Concurrency and cancellation](#concurrency-and-cancellation)
- [Trust and security boundaries](#trust-and-security-boundaries)
- [Logging](#logging)
- [Repository layout](#repository-layout)
- [Dependencies](#dependencies)
- [Decisions and their reasons](#decisions-and-their-reasons)
- [What is not there yet](#what-is-not-there-yet)

## In one picture

Ocellus AI is one Go binary. At startup it reads a config and a directory of tool files and compiles every tool.
After that it answers MCP requests: each tool call runs the compiled tool once and returns text plus structured
data.

```
startup (once)

  config.yaml ──▶ config.Load ──▶ buildWorkers ──▶ worker.Registry      (instances by name)
                                  buildProcessors ──▶ processor.Registry (processors by name)
  tools/*.yaml ─────────────────▶ catalog.Load ──▶ []*catalog.Tool      (compiled tools)
                                                         │
                                  server.New ◀───────────┘   one MCP tool per catalog tool
                                       │
                                  stdio | Streamable HTTP

a tool call (every time)

  AI agent ──MCP──▶ server handler ──▶ pipeline.Run(tool, arguments)
                                         │ BindArgs: schema, defaults
                                         │ for each call:  render request ─▶ Worker.Execute ─▶ call jq
                                         │ for each step:  render with ─▶ Processor.Run
                                         │ response.jq ─▶ response.render (or pretty JSON)
                                         ▼
                          CallToolResult {text, structuredContent}  or  {isError, text}
```

The layers, from the outside in:

| Layer | Packages | Lives |
|---|---|---|
| Transport | `internal/server` on top of the official MCP Go SDK | the whole process |
| Catalog | `internal/config`, `internal/catalog`, `internal/template`, `internal/transform` | built once at startup, read-only afterwards |
| Pipeline | `internal/pipeline` | one run per tool call |
| Extension points | `internal/worker/...` (data sources), `internal/processor/...` (analysis) | one value per instance or processor, shared by all calls |
| Analysis libraries | `internal/tsanomaly`, `internal/ssa` | called by processors |

## Design principles

These rules explain most of the code. A change that breaks one of them needs a good reason.

1. **A tool is data.** A new tool is a new YAML file, with no code and no rebuild. Code grows only at two extension
   points: worker types (where data comes from) and processors (what is done with it). Parameters, templates, jq,
   the schema, the answer and the transport are written once and shared by every tool.
2. **Fail fast at startup.** Every tool file, every request and every literal processor setting is checked when the
   server starts. An error names the file and the field and stops the server. Only what depends on the call's
   arguments is checked during the call.
3. **JSON between stages.** Workers return JSON-compatible Go values, and processors take and return them. This is
   the only contract between stages: jq, templates, processors and the MCP result all work on it.
4. **One input shape per processor.** A processor accepts exactly one documented shape and has no settings for
   picking fields out of arbitrary JSON. A `jq` step brings data into that shape. Time series travel in two
   canonical forms defined by the `series` package.
5. **Reach and secrets live in the config.** Base URLs, credentials, allowlists and limits belong to worker
   instances in the config. A tool names an instance and cannot choose a host or carry a secret. The agent controls
   only argument values, and those are validated against the tool's schema.
6. **No state between calls.** There is no database, no cache and no background job. Each call fetches its data,
   analyses it and answers. The catalog is read once; a change needs a restart.
7. **One static binary.** The analysis is pure Go (linear algebra from gonum). There is no cgo, no Python and no
   external analysis service, so the binary builds with `CGO_ENABLED=0` and runs in a distroless image.
8. **The same data gives the same answer.** The analyses that use random numbers (Isolation Forest, the noise
   floor of the anomaly ensemble, the Monte Carlo noise model and the randomized decomposition of SSA) seed them
   from constants or from the series' labels.

## Startup

`cmd/ocellusai-mcp/main.go` wires everything together, in this order:

| Step | Code | Fails when |
|---|---|---|
| 1. Flags | `-config` (default `config.yaml`), `-validate`, `-version` | an unknown flag |
| 2. Config | `config.Load`: read the file, replace `${VAR}` in values, decode strictly, apply defaults, `Validate` | the file is missing, a variable is unset, a field is unknown, a value is invalid |
| 3. Logger | `slog` to **stderr**, JSON or text | — |
| 4. Workers | `buildWorkers`: for every enabled instance, `newWorker` picks the constructor by type (`prometheus.New`, `shell.New`, `rest.New`) | a constructor rejects its settings (a bad URL, a missing `ca_file`); no instance at all |
| 5. Processors | `buildProcessors`: a fixed list registered by name | a duplicate name |
| 6. Catalog | `catalog.Load(tools_dir, workers, processors)`: every `*.yaml` and `*.yml` file, sorted by name, is parsed and compiled | any problem in any tool file; two tools with one name |
| 7. `-validate` | prints one line per tool (instances, types, calls, process chain, file) and exits | — |
| 8. Server | `server.New` registers one MCP tool per catalog tool | — |
| 9. Transport | `server.RunStdio` or `server.RunHTTP`, until SIGINT or SIGTERM | the listen address is taken |

Logs go to stderr because on the stdio transport stdout carries the protocol. Nothing in steps 1–7 contacts a data
source: constructors only check their settings, so `-validate` works offline and the server starts even when
Prometheus is down. Keep it that way in new workers.

## A tool call

The MCP SDK decodes a `tools/call` request and calls the handler registered for the tool (`server.handler`). The
handler runs the pipeline and turns its result into an MCP result. Inside `pipeline.Run`:

1. **Arguments.** `Tool.BindArgs` decodes the JSON arguments, validates them against the tool's JSON Schema
   (unknown keys are rejected), fills in defaults, sets every declared parameter that has neither a value nor a
   default to `nil` so templates can test it, and turns integer parameters into Go `int`.
2. **Calls.** For each call in order (`runCall`):
   - the request tree is rendered: every string that contains `{{` is executed as a template whose root is the
     parameters, with `param` and `result` available;
   - the worker instance executes the rendered request and returns a JSON-compatible value;
   - the call's own `jq`, if any, reshapes that value.

   A tool written as `worker:` + `request:` is a list of one unnamed call, and its value is passed on as is. A tool
   with `calls:` stores each value under the call's name, and the next stages receive the object
   `{name: value, …}`. Later calls read earlier values through `{{ result "name" }}`.
3. **Process.** Each step of `process` (`runStep`) renders its `with` section the same way and calls
   `Processor.Run(ctx, with, data, params)`. The output of a step is the input of the next one.
4. **Response.** `response.jq` runs over the data with the arguments bound to `$params`. Its result is the
   structured result of the call.
5. **Text.** `response.render` is executed with the structured result as its root, or, without it, the structured
   result is printed as indented JSON.

The handler then builds the MCP result: the text as one text content item, and the structured result as
`structuredContent`. An object goes there as is; an array or a scalar is wrapped as `{"result": value}`, because the
protocol versions before SEP-2106 (2025-06-18 and 2025-11-25) require an object and strict clients reject the whole
result otherwise; `null` leaves the field out. A failure becomes a result with `isError: true` and the text
`tool <name> failed at stage <stage>: <message>`. Either way the handler logs one line about the call.

The value that moves through the pipeline, in one picture:

```
arguments ──BindArgs──▶ params ─────────────────────────────┐ (templates, $params in jq, Processor.Run)
                                                            │
call 1: request ─render─▶ Execute ─▶ jq ─▶ value ──┐        │
call 2: request ─render─▶ Execute ─▶ jq ─▶ value ──┤ data = value (one unnamed call)
                                                   │      or {call name: value}
                                                   ▼
                     step 1: Processor.Run(with, data) ─▶ data
                     step 2: Processor.Run(with, data) ─▶ data
                                                   ▼
                              response.jq ─▶ structured ─▶ render ─▶ text
```

## Data between stages

Every value that moves between stages is made of these Go types:

| JSON | Go |
|---|---|
| object | `map[string]any` |
| array | `[]any` |
| string | `string` |
| number | `float64` or `int` |
| boolean | `bool` |
| null | `nil` |

Where values come from and what that means for code:

- **Worker results** reach processors as the worker built them; nothing converts them on the way. A worker must
  therefore return only these types. A `[]string`, a `map[string]float64` or a struct would break the first
  processor that reads it, even if jq would have coped.
- **jq** runs on a normalized copy: `transform.Normalize` turns other integer and float types into `int` and
  `float64`, `[]string` into `[]any`, `map[string]string` into `map[string]any`, and anything else through a JSON
  round trip. Its outputs are again the types above.
- **Numbers** can be `float64` (decoded JSON, Prometheus timestamps) or `int` (YAML literals, integer parameters,
  counts in reports). Code that reads a number from data accepts both, and usually numeric strings too: the
  Prometheus API writes sample values as strings. `processor.Number` does exactly that.
- **Rendered settings are strings.** In a request or a `with` section, a value written as a template is a string
  after rendering, even when it holds a number (`k: "{{ .sensitivity }}"` arrives as `"1.5"`). A literal written in
  YAML keeps its type (`k: 1.5` arrives as `1.5`). Code that reads settings accepts both; the helpers in
  `internal/worker` and `internal/processor` do.
- **Inputs are shared.** The value a step receives may also be read later by `{{ result "x" }}` in the answer
  template. Processors build new values and never modify their input.

## Load time and call time

Most mistakes in a tool file are found before the server starts serving. Work is split between the two moments like
this:

| Part of a tool | At startup | During a call |
|---|---|---|
| Tool fields | strict decoding: unknown fields are errors; `name` and `description` checked | — |
| `params` | the JSON Schema is built and resolved; defaults are validated against it | arguments validated, defaults filled in |
| `request` | `Worker.Validate` sees the raw section and checks its literals; templates are compiled | rendered, then `Worker.Execute` checks and uses the final values |
| `calls` | names checked; every literal `result "x"` must name an earlier call | dynamic names (`result .x`) are checked when executed |
| `process` | the processor must exist; `Processor.Validate` checks the raw `with`; templates are compiled | `with` rendered, then `Processor.Run` checks and uses it |
| `jq` (response, calls, `fn: jq`) | compiled | run |
| `response.render` | parsed: unknown functions and syntax errors are caught | executed; a missing key is an error (`missingkey=error`) |

For the code behind an extension point this means: `Validate` checks everything that does not depend on rendering
and skips values that still contain `{{`; `Execute` or `Run` checks the rendered values again, because a template can
produce anything.

## Types, instances and registries

**Workers** come in types and instances. A *type* is a Go package that implements `worker.Worker` and names itself
with a constant (`prometheus.Type = "prometheus"`). An *instance* is one entry under `workers` in the config: a type
plus its settings. `buildWorkers` creates one worker value per instance and registers it in `worker.Registry` under
the instance name, which is what tools refer to (`worker: prom_staging`). Two instances of one type are two
independent values with their own URLs, credentials and limits.

The config knows the set of types: `config.Worker` has one settings struct per type, so the config can be decoded
strictly and every unknown field reported. Adding a type therefore touches the `config` package and the factory in
`main.go` as well as the new package ([Writing a worker](writing-workers.md)).

**Processors** have no config. Each is registered once in `buildProcessors` under the name tools use in `fn:`, and
that one value serves every step of every tool that names it. A step's settings arrive in its `with` section on
every call.

**Detectors** are a level below processors. The `anomaly` processor keeps a table of single-series detectors
(`zscore`, `iqr`) that `outliers` reuses through `anomaly.Lookup`; `anomaly_mv` keeps its own table of multivariate
detectors (`mahalanobis`); the anomaly ensemble assembles its detectors in `tsanomaly.Analyze`. A new detector is a
file and a table entry ([Writing a processor](writing-processors.md)).

There is no plugin loading and no registration in `init()` functions. Every list is written out in one place:
`newWorker`, `buildProcessors`, the detector tables. That keeps the set of capabilities of a binary visible in the
code and the error messages that list available names complete.

## Errors

**At startup** an error stops the server and is printed with the path of the file and the field, for example:

```
ocellusai-mcp: tools/x.yaml: worker: unknown worker "rest" (configured: prometheus, shell)
ocellusai-mcp: tools/y.yaml: process[1] (anomaly): with.method: unknown detector "lof" (available: iqr, zscore)
```

The catalog adds the file name and the position (`calls[2] (cpu)`, `process[1] (anomaly)`); the worker or
processor names the key inside its section (`request.path`, `with.window`).

**During a call** every failure is a `pipeline.Error{Stage, Err}`. The stages are `arguments`, `request`, `worker`,
`process`, `jq`, `render` and `encode`. The pipeline prefixes the message with the call (`calls[1] (up): …`) or
the step (`process[0] (join): …`), and the server turns it into a tool result with `isError: true`:

```
tool dns_targets_up failed at stage worker: calls[1] (up): prometheus bad_data error: parse error: …
```

Errors are tool results rather than JSON-RPC errors so that the agent reads them and can correct its arguments. Only
a call to a tool that does not exist is a protocol error, raised by the SDK.

Rules for messages, in all code:

- English, one line, starting with the key or the position: `with.window: must be at least 4 points`.
- Say what was expected and, when there is one, how to fix it. Processors that receive the wrong shape say what
  they expected and suggest the `jq` step that would produce it.
- Never include secrets, full URLs with their query strings, or paths on the server. The `rest` worker unwraps
  transport errors so the URL does not appear; the `file` example in [Writing a worker](writing-workers.md) reports
  paths relative to its root.
- A problem with the data of one object is not an error. An analysis reports such a series as `skipped` with a
  `reason` and carries on, so one empty or odd host does not fail a fleet-wide tool.

## Concurrency and cancellation

The server holds one MCP server value with all tools. Over HTTP every session uses it, and tool calls run
concurrently. Everything built at startup is therefore shared by concurrent calls and must be safe for concurrent
use:

- **Catalog tools** are read-only after loading. Compiled templates clone themselves on every execution to bind
  `param` and `result`; compiled jq programs are safe to run in parallel.
- **Workers** hold only their configuration and clients that are safe for concurrent use (`http.Client`). Everything
  that belongs to one call lives in local variables of `Execute`.
- **Processors** hold no per-call state. A cache, like the compiled expressions of the `jq` processor, is guarded by
  a mutex.

Inside one call, calls run one after another and so do the steps. A processor may use goroutines of its own: `ssa`
and `ssa_mv` analyse series in parallel, and the anomaly ensemble analyses the metrics of a series in parallel.

The context of the request reaches every stage. The SDK cancels it when the client cancels the request (the MCP
`notifications/cancelled` message) and, over HTTP with recent protocol versions, when the HTTP request goes away
(`PropagateRequestCancellation`). Workers add their own timeout with `context.WithTimeout` and stop when
the context ends. Processors check `ctx.Err()` between series. The analysis libraries do not take a context, so their
processors run them in a goroutine and return as soon as the context ends; the computation finishes in the background
and its result is dropped.

On SIGINT or SIGTERM the root context is cancelled. The stdio transport stops; the HTTP server shuts down gracefully
with a 10-second limit.

## Trust and security boundaries

| Party | Controls | Trusted? |
|---|---|---|
| Operator | the config and the tool files | yes: a tool file can contain any PromQL, any allowed command, any path under a `rest` base URL |
| Agent | argument values only | no |
| Data sources | their responses | no: responses are parsed, size-limited and never executed |

What keeps an agent's arguments in their place:

- The input schema is generated from `params`: types, `enum`, `pattern`, `minimum`/`maximum`, and no unknown keys.
- Template helpers escape a value for the language it lands in: `promLabel` and `promRegex` for PromQL,
  `pathEscape` for one segment of a URL path.
- The `shell` worker starts the program directly, without `sh -c`; each argument is exactly one argument; the
  program must be on the instance's allowlist; the child gets only `PATH` and the variables the instance lists.
- The `rest` worker joins the path to the instance's base URL and refuses paths that climb above it; it encodes the
  query itself; the method is a literal in the tool file and must be allowed by the instance; the instance's headers
  override the tool's; redirects are not followed, so credentials cannot be sent to another host.
- Outputs are limited in size (`max_output_bytes`, `max_response_bytes`), and timeouts apply to every request.

Secrets are written in the config as `${VAR}` and taken from the environment. They are never logged: the
Prometheus headers are added inside an HTTP round tripper, the `rest` worker logs only the method, the path and the
status, and the `shell` worker never logs its environment.

New code keeps these properties: no new way for a tool or an argument to choose a host, a binary or a path outside
what the operator configured; limits on what is read; no secrets in logs or error messages. The HTTP transport has
no authentication yet, which is the most important missing piece ([What is not there yet](#what-is-not-there-yet)).

## Logging

Logs use `log/slog`, always to stderr, as JSON or text at the level set in the config. Every tool call writes one
line:

| Field | Meaning |
|---|---|
| `request_id` | 8 random bytes in hex, to find the call in the logs |
| `tool` | the tool name |
| `worker`, `worker_type` | the instances and their types, comma-separated for tools with `calls` |
| `session` | the MCP session id |
| `status` | `ok` or `error` |
| `stage`, `error` | for failures |
| `duration_ms` | the time of the whole call |

At `debug` level the pipeline also logs each rendered request (`request rendered`) and each finished process step,
and the workers log their own details (the `rest` worker logs the method, the path, the status and the size).
Rendered requests contain the arguments; workers must not put secrets into the request map, which is one more
reason why credentials belong to the instance.

## Repository layout

```
cmd/ocellusai-mcp/               main: flags, config → workers → processors → catalog → server
internal/config/              config.yaml: ${VAR}, strict decoding, defaults, validation
internal/catalog/             tool files → compiled tools: schema, calls, steps, jq, render; BindArgs
internal/template/            text/template with sprig and the Ocellus helpers; request trees
internal/transform/           jq (gojq): compile once, run with $params; value normalization
internal/pipeline/            one tool call: arguments → calls → process → jq → render
internal/server/              MCP tools and the handler; stdio and Streamable HTTP
internal/worker/              the Worker interface, the registry, helpers
  prometheus/ shell/ rest/    the three worker types
internal/processor/           the Processor interface, the registry, helpers for `with`
  series/                     canonical time-series shapes and the regular grid
  jq/ join/                   transformations
  anomaly/ outliers/ anomalymv/ ensemble/ clusterevents/ ssaproc/   analyses
internal/tsanomaly/           batch anomaly detection: Matrix Profile, AR, level shift, Isolation Forest, PCA, POT
internal/ssa/                 Singular Spectrum Analysis and MSSA: decomposition, analysis, forecasts
tools/                        the working catalog (example config, Docker image, Kubernetes)
tools-test/                   the reference tools the server tests load and call against mocks
docs/                         user and developer documentation
skills/ocellus-tool-builder/  an agent skill that interviews a user and writes a tool file
scripts/mcp-call.py           a stdio MCP client for trying tools by hand
deploy/k8s.yaml               a Kubernetes manifest
Dockerfile                    distroless static image
Makefile                      build, test, lint, validate, docker
config.example.yaml           a commented config with values written in place
config.env.yaml               the same config filled from environment variables
```

## Dependencies

| Module | Used for |
|---|---|
| Go 1.27.1 | the toolchain (`go.mod`) |
| `github.com/modelcontextprotocol/go-sdk` v1.7.0 | the MCP server and transports |
| `github.com/google/jsonschema-go` v0.4.3 | input schemas and argument validation (the same library the SDK uses) |
| `github.com/itchyny/gojq` v0.12.19 | jq |
| `github.com/Masterminds/sprig/v3` v3.3.0 | template functions |
| `github.com/prometheus/client_golang` v1.23.2 | the Prometheus API client |
| `github.com/prometheus/common` v0.71.0 | Prometheus durations and model types |
| `gopkg.in/yaml.v3` | the config and the tool files |
| `gonum.org/v1/gonum` v0.17.0 | linear algebra and FFT for the analysis libraries |

The list is short on purpose. A new dependency needs a reason that the standard library and these modules cannot
cover, must build without cgo, and adds to what every user downloads and audits.

## Decisions and their reasons

**The Prometheus matrix is the canonical scalar time series.** It is the wire format of Prometheus, VictoriaMetrics,
Thanos, Mimir and Loki metric queries, so the main source needs no conversion, and jq written against the HTTP API
keeps working after a transformation. Other sources are brought into it by a `jq` step. Several metrics per object
travel in a neutral multivariate form built by `join`.

**Processors have no selectors.** A key such as `items: ".result[]"` in a processor's settings would grow a small
query language inside every processor. Shaping is the job of the `jq` step, so each processor documents one input
and keeps its settings about the analysis.

**Calls are sequential, without loops.** A tool can chain calls and feed one into the next, but cannot fan out "one
call per item" or run calls in parallel. That keeps a tool's cost and failure modes visible in its file; work that
needs fan-out is left to the agent, which can call tools repeatedly.

**Errors are tool results.** A failed call returns `isError` with a readable message, so the agent sees what went
wrong and can retry with other arguments instead of seeing a generic protocol failure.

**`structuredContent` is always an object.** Arrays and scalars are wrapped as `{"result": …}` because strict clients
reject a non-object and drop the whole answer.

**Strict decoding everywhere.** Unknown fields in the config, in a tool, in a call, in a step or in a worker's
settings are errors. A typo that silently disables a setting is worse than a server that does not start.

**The `rest` worker does not retry or follow redirects.** Actions such as creating a silence are not idempotent, and a
redirect could send the instance's credentials to a host the operator did not configure.

**Integer parameters arrive as `int`.** JSON numbers decode as `float64`, which Go prints as `1e+06` once it is large
and which `{{ if eq .top 10 }}` refuses to compare with an integer literal. Converting declared integers keeps both
working.

## What is not there yet

Good places to start contributing:

- **Authentication on the HTTP transport.** Today the HTTP endpoint must sit on a private network or behind an
  authenticating proxy.
- **Tool annotations.** MCP lets a server mark tools as read-only, destructive or idempotent
  (`readOnlyHint`, `destructiveHint`, `idempotentHint`). The server registers tools without them (`server.New`). A
  natural design is an `annotations` block in the tool file, with defaults derived from the method for `rest` tools.
- **Reloading the catalog** without a restart.
- **More sources:** a Loki worker for log queries.
- **Operations:** `/metrics`, caching of identical queries, rate limits.
- **MCP resources and prompts.**
