# User guide

[For DevOps](for-devops.md) · [Quick start](quick-start.md) · **User guide** · [Workers](workers.md) · [Templates and jq](templates-and-jq.md) · [Processors](processors.md) · [Writing tools](writing-tools.md)

This guide is for the people who install, configure and run Ocellus AI, and for those who use its tools through an
AI agent and want to understand what they get back. It assumes you have been through the
[Quick start](quick-start.md) or know how to build the binary.

## Contents

- [How Ocellus AI works](#how-ocellusai-mcp-works)
- [Installation](#installation)
- [Configuration](#configuration)
- [Running the server](#running-the-server)
- [Transports](#transports)
- [The tool catalog](#the-tool-catalog)
- [What the agent receives](#what-the-agent-receives)
- [The bundled tools](#the-bundled-tools)
- [Time, windows and history](#time-windows-and-history)
- [Performance and limits](#performance-and-limits)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Security model](#security-model)
- [Troubleshooting](#troubleshooting)

## How Ocellus AI works

Ocellus AI is an [MCP](https://modelcontextprotocol.io) server. An AI agent connected to it sees a list of tools, each
with a name, a description and a JSON Schema of its parameters, and calls them when it needs data. Behind every tool
is a YAML file that says where the data comes from, how to analyse it and what to answer.

```
AI agent ──MCP──▶ ocellusai-mcp ──▶ tools/<name>.yaml
                                  params → [request → worker → jq]* → process* → jq → render
                                                       │
                                            prometheus | shell | rest
```

When the agent calls a tool:

1. **Arguments** are checked against the tool's schema; defaults are filled in.
2. **Calls.** The tool renders a request from a template and sends it to a **worker**: a Prometheus query, a
   command such as `df`, or an HTTP request to a JSON API. A tool can make several calls in sequence and use
   the result of one in the next.
3. **Processing.** An optional chain of **processors** reshapes or analyses the data: anomaly detection, outliers,
   incidents across hosts, trend and forecast.
4. **Answer.** A final jq expression picks what matters, and a text template turns it into a few lines the model
   reads. The structured data goes along as `structuredContent`.

The terms used throughout the documentation:

| Term | Meaning |
|---|---|
| **Tool** | One YAML file in the catalog; what the agent calls by name. |
| **Catalog** | The directory of tool files (`tools_dir`), loaded once at startup. |
| **Worker type** | The code that talks to a kind of source: `prometheus`, `shell`, `rest`. |
| **Worker instance** | One configured connection of a type, under `workers.<name>` in the config: a URL, credentials, an allowlist. Tools refer to instances by name. |
| **Processor** | A step of the `process` chain: `jq`, `join`, `anomaly`, `outliers`, `anomaly_mv`, `anomaly_ensemble`, `cluster_events`, `ssa`, `ssa_mv`. |
| **Template** | A Go `text/template` that builds requests and the answer text. |
| **jq** | Expressions (the [gojq](https://github.com/itchyny/gojq) implementation) that reshape JSON between steps. |

Everything runs inside one Go binary. There is no database, no Python runtime and no external analysis service; the
server keeps no state between calls.

## Installation

### From source

```bash
git clone https://github.com/ocellus-ai/ocellusai-mcp.git
cd ocellusai-mcp
make build                 # → bin/ocellusai-mcp, a static binary (CGO disabled)
```

You need Go 1.27.1 or newer. The binary has no runtime dependencies and can be copied to any host of the same OS and
architecture. Cross-compile with the usual Go variables, for example
`GOOS=linux GOARCH=arm64 make build`.

Other `make` targets:

| Target | Does |
|---|---|
| `make build` | builds `bin/ocellusai-mcp` with the version from `git describe` |
| `make validate` | builds and loads `config.example.yaml` with its catalog |
| `make run` | builds and starts with `config.example.yaml` (`make run CONFIG=config.yaml` for yours) |
| `make docker` | builds a container image (see below) |
| `make test`, `make test-race` | runs the test suite |

### Container image

The [`Dockerfile`](../Dockerfile) builds a distroless static image that runs as non-root (uid 65532) and holds the
binary, `/app/tools` (the working catalog) and `/app/config.yaml` (the example config):

```bash
docker build -t ocellusai-mcp:latest .
```

The `prometheus` and `rest` workers work in it as is. There are no command-line programs inside, so a `shell` tool
needs an image of your own with the programs it runs; see [Workers](workers.md#in-a-container).

The image starts `/app/ocellusai-mcp -config /app/config.yaml` from `/app`, so the default `tools_dir: ./tools` points at
`/app/tools`. Mount your own config over `/app/config.yaml`, and your own catalog over `/app/tools` (or point
`tools_dir` elsewhere):

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v "$PWD/tools:/app/tools:ro" \
  ocellusai-mcp:latest
```

The image has no shell, so `docker exec … sh` does not work; debug with the logs and the health endpoint.

## Configuration

The server reads one YAML file, `config.yaml` by default (`-config <path>` to change it).

```yaml
server:
  transport: http                 # stdio | http
  listen: ":8080"                 # http only

tools_dir: ./tools                # the catalog

workers:                          # data sources; at least one instance
  prometheus:
    url: http://prometheus.monitoring:9090
    timeout: 15s
    headers: {Authorization: "Bearer ${PROM_TOKEN}"}
  shell:
    allowlist: [df, dig]
    env: {TZ: UTC}
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
    methods: [GET]

log:
  level: info                     # debug | info | warn | error
  format: json                    # json | text
```

### Reference

| Key | Default | Meaning |
|---|---|---|
| `server.transport` | `stdio` | `stdio`: the client starts the binary and talks through stdin/stdout. `http`: Streamable HTTP on `listen`. |
| `server.listen` | `:8080` | Address for `http`: `:8080`, `127.0.0.1:8080`. The MCP endpoint is `/mcp`, the health check `/healthz`. |
| `tools_dir` | `./tools` | Directory with the tool files, relative to the **working directory of the process**, not to the config file. |
| `workers.<name>` | — | One worker instance. The keys depend on its type; see [Workers](workers.md). At least one instance is required. |
| `workers.<name>.type` | the entry name | `prometheus`, `shell` or `rest`. Can be omitted when the entry is named after its type. |
| `log.level` | `info` | `debug` adds the rendered request of every call and a line per processing step. |
| `log.format` | `json` | `json` or `text`. Logs always go to stderr. |

The example config ([`config.example.yaml`](../config.example.yaml)) sets `transport: http`; a config that omits
`server` entirely gets `stdio`.

### Environment variables in the config

`${NAME}` in any value is replaced with the environment variable `NAME` when the server starts:

```yaml
workers:
  prometheus:
    url: ${PROM_URL}
    headers:
      Authorization: "Bearer ${PROM_TOKEN}"   # inside a longer string
```

- Substitution works in values only, not in keys or comments.
- An unset variable stops the start, and the error lists every missing name:
  `config.yaml: environment variables not set: PROM_TOKEN, PROM_URL`. There are no default values; an empty but set
  variable is accepted.
- A plain value is re-read after substitution, so `listen: ${PORT}` with `PORT=8080` becomes a number or a string as
  YAML would read it. Inside flow collections (`[...]`, `{...}`) put the reference in quotes.
- A list cannot come from one variable: write lists such as `allowlist` in the file.

[`config.env.yaml`](../config.env.yaml) is a complete config where every setting comes from the environment, for
container platforms and Helm charts.

### Strict parsing

Unknown fields are errors, both in the config and in tool files, so a typo cannot silently disable a setting:

```text
ocellusai-mcp: config.yaml: …
  line 3: field listn not found in type config.Server
```

## Running the server

```bash
./bin/ocellusai-mcp -config config.yaml            # serve
./bin/ocellusai-mcp -config config.yaml -validate  # load everything, print the catalog, exit
./bin/ocellusai-mcp -version                       # print the version, exit
```

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `config.yaml` | Path to the config file. |
| `-validate` | off | Load the config, create the workers, load and compile the whole catalog, print one line per tool and exit with 0. Nothing is contacted. |
| `-version` | off | Print the build version (`git describe` at build time, `dev` otherwise). |

### Startup

At startup the server reads the config, substitutes environment variables, creates every worker instance, loads every
tool file and compiles its templates and jq expressions. **Any error stops the start** with a message that names the
file and the field. A server that is running has therefore loaded every tool successfully.

The catalog is read only at startup. To add or change a tool, restart the server.

### Logs

Logs are written to **stderr** (stdout carries the protocol on the stdio transport). Every tool call produces one
line:

```text
time=… level=INFO msg="tool call ok" request_id=4f2a9c1e0b7d3a65 tool=node_ssa worker=prometheus worker_type=prometheus session=… status=ok duration_ms=431
time=… level=ERROR msg="tool call failed" request_id=… tool=vm_metrics … status=error stage=worker duration_ms=15002 error="prometheus: request timed out: …"
```

With `log.level: debug` you also get `request rendered` with the final request of every call (the PromQL, the command
line, the URL path) and `process step done` per processing step. Credentials from worker headers and environment
variables of commands are never logged.

### Shutdown

`SIGINT` or `SIGTERM` stops the server. On the HTTP transport it stops accepting connections and gives running
requests up to 10 seconds to finish.

## Transports

### stdio

The MCP client starts the binary as a child process and talks to it through stdin and stdout. Use it for a single
user on a workstation: Claude Code, Claude Desktop, IDE assistants.

```yaml
server:
  transport: stdio
tools_dir: /home/me/ocellusai-mcp/tools   # absolute: the client decides the working directory
```

```bash
claude mcp add ocellus -- /home/me/ocellusai-mcp/bin/ocellusai-mcp -config /home/me/ocellusai-mcp/config.yaml
```

The server lives as long as the client session. Its logs go to wherever the client sends the child's stderr.

### Streamable HTTP

One long-running server shared by many clients and agents, for example in Kubernetes.

```yaml
server:
  transport: http
  listen: ":8080"
```

| Path | Purpose |
|---|---|
| `/mcp` | MCP over Streamable HTTP. Clients keep a session (`Mcp-Session-Id` header); answers arrive as server-sent events. When a client cancels a request, the cancellation reaches the running worker. |
| `/healthz` | Always `ok` while the process is up. It does not check Prometheus or other sources. |

To call a tool by hand, initialise a session, then send requests with its id:

```bash
curl -s -D headers.txt -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

```bash
SID=$(grep -i '^mcp-session-id' headers.txt | awk '{print $2}' | tr -d '\r')
```

```bash
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
```

```bash
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vm_metrics","arguments":{"name":"db-02"}}}'
```

For everyday testing [`scripts/mcp-call.py`](../scripts/mcp-call.py) is simpler; it uses stdio (see the
[Quick start](quick-start.md#calling-tools-without-an-agent)).

### The HTTP transport has no authentication

Anyone who can reach `/mcp` can list and call every tool. Until authentication is built in:

- bind to `127.0.0.1` when only local clients need it;
- in Kubernetes, keep the Service `ClusterIP`, restrict it with a `NetworkPolicy`, and reach it with
  `kubectl port-forward` or from agents inside the cluster;
- to expose it further, put an authenticating reverse proxy in front (OAuth2 Proxy, an API gateway, a service mesh
  with mTLS);
- make worker instances read-only where possible (`methods: [GET]` for REST APIs, read-only commands for `shell`).

## The tool catalog

`tools_dir` holds one YAML file per tool.

- Files ending in `.yaml` or `.yml` are loaded, in name order; other files are ignored.
- Every `name` must be unique; a duplicate is an error naming both files.
- An empty directory is an error.
- Every tool must pass validation: its fields, its parameter schema, the worker instances it uses, the keys of its
  requests and processors, the syntax of its templates and jq. **One broken file stops the whole server.**

Run `-validate` after every change:

```text
node_deep_ensemble   worker=prometheus  type=prometheus  calls=cpu>iowait>steal>mem>disk>net_rx>net_tx>load>tcp>majfault  process=join>anomaly_ensemble  file=tools/node_deep_ensemble.yaml
```

| Column | Meaning |
|---|---|
| `worker` | the worker instances the tool uses |
| `type` | their types |
| `calls` | the names of its calls in order, `-` for a single request |
| `process` | the processing chain, `-` when there is none |
| `file` | the file it came from |

Typical startup errors:

| Message | Cause |
|---|---|
| `tools/x.yaml: worker: unknown worker "rest" (configured: prometheus)` | the tool uses an instance that is not in the config: add it or remove the tool |
| `tools/x.yaml: request.command: "dig" is not in workers.shell.allowlist` | a `shell` tool runs a command that is not allowed |
| `… field descripton not found in type catalog.Spec` | a typo or an unsupported field |
| `tools/x.yaml: name: "Pod-CPU" must match ^[a-z0-9_]+$` | tool names are lower-case letters, digits and `_` |
| `tools/x.yaml: process[0] (anomaly): with.method: …` | a processor setting is wrong |
| `tools/x.yaml: response.jq: jq parse: …` | a jq syntax error |
| `… template request.query: … function "promLable" not defined` | a typo in a template function |
| a YAML error with a line number | usually an unquoted value that starts with `{{` |

`-validate` checks structure and syntax. It does not run queries, so it cannot find a wrong metric name, a
misspelt field inside a template or a failing jq expression on real data. A test call does; see
[Templates and jq](templates-and-jq.md#debugging).

### Where tools come from

The repository has three sets:

| Directory | What it is |
|---|---|
| [`tools/`](../tools) | **The working catalog**: 23 tools for a monitored estate. The example config, the image and the Kubernetes manifest load it. |
| [`tools-test/`](../tools-test) | 13 small reference tools, one feature each. The test suite runs them against mocked Prometheus, Alertmanager and `dig`; they also work against the real ones. |
| `internal/processor/*/testdata/` | Four analysis examples used by end-to-end tests: `vm_anomaly_ensemble`, `vm_multivariate_anomalies`, `vm_cpu_forecast`, `vm_cpu_mem_mssa`. |

To serve tools from several places, copy the files you want into one directory: a server loads exactly one
`tools_dir`. To write your own, see [Writing tools](writing-tools.md).

## What the agent receives

### The tool list

For each tool the agent sees:

- `name`;
- `description`, written for a model: what the tool returns, from which source, in which units, what an empty
  answer means and when to use it;
- `inputSchema`: a JSON Schema built from the tool's parameters, with types, defaults, enums, bounds and patterns.

Arguments the schema does not declare are rejected, so a misspelt parameter name is an error instead of being
ignored.

### A successful call

A result has two parts:

- **`content`**: one text block. Most tools render a short report written for the model. A tool without a text
  template returns its data as pretty-printed JSON.
- **`structuredContent`**: the same data as JSON, for agents and programs that work with structure. It is always an
  object; a list or a number is wrapped as `{"result": …}`.

Most clients show the model only the text, so the bundled tools put everything that matters into it, including the
parameters used ("over the 12h ending at …, risk 0.001"), the counts of analysed and skipped objects and the
thresholds below which changes were ignored.

### A failed call

A failure is a normal tool result with `isError: true` and a message that names the stage:

```text
tool node_ssa failed at stage arguments: validating root: validating /properties/range: pattern: "4 days" does not match regular expression "^([0-9]+[smhdwy])+$"
tool node_ssa failed at stage arguments: validating root: unexpected additional properties ["windw"]
tool node_cpu_mem_ensemble failed at stage worker: calls[0] (cpu): prometheus: Post "http://prometheus:9090/api/v1/query_range": dial tcp …: connect: connection refused
```

| Stage | What failed |
|---|---|
| `arguments` | the arguments do not match the schema: a missing required parameter, an unknown one, a wrong type, a value outside the allowed range or pattern |
| `request` | building the request: usually a template error in the tool, or a value that a helper rejects (an invalid duration) |
| `worker` | the source: a PromQL error, a timeout, an unreachable host, a command that exited with an error, an HTTP error status |
| `process` | a processing step, named as `process[i] (name)`: usually data of an unexpected shape or a setting that is invalid for this data |
| `jq` | the final jq expression |
| `render` | the text template |

In tools with several calls, errors of a call carry its name: `calls[1] (cpu): …`. An unknown tool name is a protocol
error, not a tool result.

An agent can act on these messages: it can fix an argument after an `arguments` error, and report a `worker` error
to you. Errors at the `request`, `process`, `jq` or `render` stage are bugs in the tool or a mismatch with your data,
for whoever maintains the catalog.

## The bundled tools

The working catalog in [`tools/`](../tools) covers a typical estate: virtual machines with node_exporter, Kubernetes,
PostgreSQL, Kafka, HAProxy and blackbox probes. Every file starts with a comment that says what the tool assumes about
your data; the [Quick start](quick-start.md#4-match-the-catalog-to-your-labels) lists the label names to check.

### Overview and snapshots

| Tool | Answers | Key parameters |
|---|---|---|
| `estate_overview` | Is anything wrong right now? Targets down, failed probes, firing alerts, full filesystems, restarting pods and overloaded VMs in one call. | `at`, `fs_threshold` (80%), `restart_range` (1d), `top` |
| `alerts_firing` | Which alerts are firing? | `severity` |
| `vm_metrics` | A snapshot of one VM found by name or address: CPU, memory, filesystems, disks, network, TCP. | `name`, `window` (5m) |
| `vm_steal_time` | Which VMs lose CPU time to the hypervisor (noisy neighbours)? | `window` (1h), `threshold` (1%), `step` |
| `node_signal_window` | The raw samples of one signal around a moment, to look at a segment another tool reported. | `signal`, `instance`, `around`, `span` (30m), `step` |
| `pod_memory_usage` | Top pods by working-set memory in a namespace. | `namespace`, `pod`, `top` |
| `pod_restarts` | Containers that restarted within a window. | `namespace`, `window` (1h) |
| `pg_slowdown_check` | Why is this PostgreSQL server slow? Host, database and replication signals against the same window a week earlier, with reading hints. | `host`, `window` (1h), `compare` (7d) |
| `haproxy_backend_health` | Which HAProxy backend has errors, queues or slow responses? | `instance`, `proxy`, `window` (5m) |

### Anomalies in each object's own history

| Tool | Answers | Analysis | Key parameters |
|---|---|---|---|
| `node_cpu_anomalies` | Which VMs had CPU samples unusual for themselves? | `anomaly` (IQR) | `range` (6h), `sensitivity` (3), `min_delta` (5 points) |
| `pg_xact_anomalies` | Which databases had unusual transaction rates? | `anomaly` (z-score) | `range` (6h), `threshold` (3), `min_delta` (1 tx/s) |
| `kafka_lag_anomalies` | Which consumer groups had lag spikes? | `anomaly` (IQR, upward) | `range` (6h), `sensitivity` (3), `min_lag` (1000 messages) |
| `probe_latency_anomalies` | Which services got slower, and which probes failed? | `anomaly` (z-score, upward) | `range` (12h), `threshold` (3), `min_ms` (50 ms) |
| `node_cpu_mem_mv` | Where did the usual relation between CPU and memory break? | `anomaly_mv` | `range` (6h), `threshold` (4), `min_delta` (3 points) |
| `node_disk_pressure_mv` | Where did iowait, disk utilisation and latency move unusually together? | `anomaly_mv` | `range` (6h), `threshold` (4), `min_iowait`, `min_util`, `min_await_ms` |
| `node_cpu_mem_ensemble` | Anomalous time segments in CPU and memory, with the metric to blame. | `anomaly_ensemble` | `range` (1d), `window` (1h), `risk` (0.001) |
| `node_deep_ensemble` | The deep check of VMs: ten signals per VM analysed together, segments with the signals that moved, level shifts. | `anomaly_ensemble` | `range` (12h), `window` (1h), `risk` (0.001), `min_effect` |

### Outliers among peers

| Tool | Answers | Key parameters |
|---|---|---|
| `pod_memory_outliers` | Which pods use much more memory than the other pods of their namespace? | `namespace`, `cluster`, `sensitivity` (1.5), `min_mib` (100) |
| `pod_restart_outliers` | Which pods restarted unusually often compared with the rest of the cluster? | `cluster`, `range` (1d), `min_restarts` (1) |

### Incidents across hosts

| Tool | Answers | Key parameters |
|---|---|---|
| `node_cluster_events` | Did many VMs misbehave at the same time? Folds the segments of `node_deep_ensemble` across VMs into incidents. | `instance`, `range` (12h), `gap` (5m), `min_series` (2) |

### Trends, cycles and forecasts

| Tool | Answers | Key parameters |
|---|---|---|
| `node_ssa` | The trend, daily cycle and noise of one signal per VM, stretches outside the usual daily profile, and a forecast. | `signal`, `range` (4d), `window` (1d), `horizon` (1d) |
| `node_mssa` | The same for five signals of a VM together: which cycles they share. | `range` (4d), `window` (1d), `horizon` (1d) |
| `node_forecast` | A summary of the forecast for an interval such as "tomorrow 09:00–18:00": mean, extremes, profile, time above a threshold. | `signal`, `from`, `to`, `above`, `buckets` |

### Conventions shared by the tools

| Parameter | Meaning |
|---|---|
| `at` | The end of the window (or the moment of a snapshot): `now`, `now-<duration>` such as `now-3d`, or an RFC 3339 time such as `2026-03-12T02:00:00Z`. |
| `range` | How far back from `at` to look: `6h`, `1d`. The forecast tools accept compound durations: `3d12h`. |
| `step` | Sample resolution of range queries: `60s` for anomalies, `5m` for forecasts. |
| `instance` | A regular expression over the `instance` label: `web-.*`, `db-0[1-3].*`. Prometheus anchors it, so `db-01` does not match `db-01:9100`; add `.*`. |
| `top` | How many objects to list. The text says how many were analysed in total. |
| `min_*`, `min_delta` | The smallest change worth reporting, in the metric's own units: points of a percentage, MiB, messages, milliseconds. Smaller moves are not reported even when they are statistically unusual (see [Processors](processors.md#the-noise-floor-min_delta)). |
| `sensitivity`, `threshold`, `risk` | How strict the detector is. Higher `sensitivity`/`threshold` and lower `risk` report fewer, stronger anomalies. |

### The reference tools

[`tools-test/`](../tools-test) shows each feature on a small scale. Some need more than Prometheus:

| Tools | Need |
|---|---|
| `targets_up`, `targets_down`, `targets_down_summary` | Prometheus; the three answer levels: raw JSON, jq, jq + text |
| `pod_cpu_usage`, `namespace_cpu_timeseries`, `pod_cpu_anomalies`, `pod_cpu_outliers` | Prometheus with cAdvisor CPU metrics |
| `dns_targets_up` | `dig` and Prometheus in one tool (`calls`) |
| `disk_usage`, `disk_usage_outliers` | the `shell` worker with `df` (runs on the server's host) |
| `alertmanager_silences`, `alertmanager_silence_create`, `alertmanager_silence_expire` | a `rest` instance named `rest` pointing at the Alertmanager v2 API; the last two **change** silences |

## Time, windows and history

Every tool that looks at a period takes `at` and a length (`range`, `window` or `span`), so an agent examines a past
incident the same way as the present: "what happened on db-01 between 01:00 and 03:00 UTC on 12 March" becomes
`at: 2026-03-12T03:00:00Z, range: 2h`. All times in answers are UTC.

Things to keep in mind:

- **Retention.** Prometheus keeps data for a limited time (15 days by default). A window outside it returns
  nothing, and the tools say "no VMs" or "nothing found", not an error.
- **Points per series.** A range query returns `range / step` points per series: a day at `60s` is 1440 points. The
  point detectors need tens to hundreds of points; the ensemble needs at least 120 at one-minute resolution; the
  forecasts need at least three periods of the cycle, which is four days for a daily one at `5m`.
- **Resolution versus reach.** For a week, use `step: 5m` to find what happened, then call again with `step: 60s`
  around the found moment to see exactly when.
- **Gaps** (a host down, a scrape hole) are handled by the analysis tools: they are reported as gaps or skipped
  objects with a reason, not as anomalies.

## Performance and limits

| What | Default | Where to change |
|---|---|---|
| Prometheus query timeout | 15 s | `timeout` of the instance |
| Command timeout | 30 s | `timeout` of the `shell` instance, or per request |
| Command output | 1 MiB, larger is truncated | `max_output_bytes` |
| HTTP API timeout | 15 s | `timeout` of the `rest` instance, or per request |
| HTTP API response | 1 MiB, larger is an error | `max_response_bytes` |

Fleet-wide analysis tools are the heaviest calls. As a guide, ensemble analysis of a hundred VMs over a day at one
minute takes seconds, and the ten-signal version issues ten range queries that may each return over a megabyte. If
such calls time out, raise the Prometheus `timeout` (60 s is reasonable), lower the resolution (`step: 5m`), or narrow
the call (`instance`). Each call holds its data in memory only while it runs; size the memory limit by running your
heaviest tool on your largest fleet.

There is no cache and no rate limit: every call queries the source again, so an agent that calls a heavy tool in a
loop loads your Prometheus accordingly.

## Deploying to Kubernetes

[`deploy/k8s.yaml`](../deploy/k8s.yaml) contains:

- a `ConfigMap` `ocellusai-mcp-config` with `config.yaml` (HTTP on `:8080`, Prometheus, an Alertmanager `rest`
  instance);
- a `Deployment` running as non-root with a read-only root filesystem, no capabilities and no service account token
  (the server does not talk to the Kubernetes API), probes on `/healthz`;
- a `ClusterIP` `Service` on port 8080.

The tool catalog is a second ConfigMap, built from the directory:

```bash
kubectl -n monitoring create configmap ocellusai-mcp-tools --from-file=tools/ --dry-run=client -o yaml | kubectl apply -f -
```

```bash
kubectl -n monitoring apply -f deploy/k8s.yaml
```

After changing the config or the catalog, restart the pods; the server reads both only at startup:

```bash
kubectl -n monitoring rollout restart deployment/ocellusai-mcp
```

Agents inside the cluster reach the server at `http://ocellusai-mcp.monitoring.svc:8080/mcp`.

Before you apply it:

- **Image.** The manifest uses `ocellusai-mcp:dev`; build and push your own image. For `shell` tools, use an image
  with their programs and add a `shell` instance to the config ([Workers](workers.md#in-a-container)).
- **Workers you don't use.** Remove the `rest` instance if you don't need it. A tool that refers to a removed
  instance stops the start, so keep the catalog consistent.
- **Secrets.** Put tokens in a `Secret`, expose them to the container as environment variables and reference them as
  `${VAR}` in the config.
- **The catalog ConfigMap.** Keep only YAML files in the directory (no editor backups or archives): every file
  becomes a key. Key names allow only `[-._a-zA-Z0-9]`, and a ConfigMap is limited to 1 MiB.
- **Exposure.** The Service has no authentication; see [Transports](#the-http-transport-has-no-authentication).

## Security model

What is trusted and what is not:

| Who | Controls | Trust |
|---|---|---|
| The operator | the config: worker instances, URLs, credentials, allowlists, limits | trusted |
| Whoever writes tool files | requests, commands within the allowlist, processing, answers | trusted, like code |
| The agent (and whoever talks to it) | only the arguments of the tools it calls | untrusted |

What protects you from the agent's arguments:

- **Schemas.** Every argument is checked against the tool's parameter schema before anything runs; extra arguments
  are rejected. Good tools constrain every parameter that reaches a query or a command with a pattern, an enum or
  bounds.
- **PromQL.** Tools pass values through `promLabel`, `promRegex` and `promDuration`, which escape them, so a value
  cannot close a string and inject PromQL.
- **Commands.** The `shell` worker runs only binaries from the instance's allowlist, directly, without `sh -c`: a value
  cannot add a pipe, a redirection or a second command. Each value is exactly one argument. The child process gets
  `PATH` plus the variables you list, nothing else from the server's environment.
- **HTTP APIs.** The base URL, credentials and allowed methods are config. A tool gives only a path relative to the
  base URL, values in the path are escaped, and a path cannot climb above the base path. Redirects are not followed,
  so credentials cannot be sent to another host.
- **Size and time.** Every source has a timeout and a size limit.

What you must protect yourself:

- **The tool files.** A tool file can run any allowlisted command with any arguments, call any path and method an
  instance allows, and its templates can read the server's environment variables (the template function `env`).
  Whoever can change `tools_dir` can read the secrets you put in the environment. Review tool changes like code and
  keep the directory writable only by the people who maintain the catalog.
- **Actions.** Tools that change something (a REST `POST`/`PUT`/`PATCH`/`DELETE`, a mutating command) run every
  time an agent calls them. Their descriptions say "This is an action". Use read-only instances (`methods: [GET]`)
  wherever actions are not needed.
- **Allowlists.** Never allow a shell (`sh`, `bash`) or an interpreter (`python`, `perl`) in a `shell` instance: it
  would turn every tool argument into code.
- **The network.** The HTTP transport has no authentication.

Logs never contain the headers of worker instances or the environment of commands, and error messages of HTTP calls
leave out the URL. At `debug` level the rendered request of every call is logged, with the argument values
substituted into it.

## Troubleshooting

### The server does not start

| Message | Fix |
|---|---|
| `read config: open config.yaml: no such file or directory` | Pass `-config` with the right path. |
| `environment variables not set: …` | Export the variables, or remove the references. |
| `… field X not found in type …` | A typo or an unsupported key in the config or in a tool. |
| `no workers configured` | Add at least one instance under `workers`. |
| `workers.<name>.url: required` | A worker instance without its URL. |
| `tools/x.yaml: worker: unknown worker "…"` | A tool uses an instance that is not configured: add the instance or remove the tool. |
| `tools_dir: open …: no such file or directory` or `tools_dir …: no *.yaml files found` | `tools_dir` is wrong; it is relative to the working directory of the process, not to the config file. |

### A tool answers nothing

1. **Is there data at that time?** Check retention and `at`.
2. **Do the labels match?** Open the tool file, copy the query, replace the template parts with real values and run
   it in the Prometheus UI. Or set `log.level: debug`, call the tool and copy the `request rendered` line.
3. **Is the threshold too high?** Detector tools ignore changes smaller than `min_delta` (or `min_*`). Their text
   says how many raw findings were below it.
4. **Is the object idle?** `node_deep_ensemble` skips VMs whose CPU and network stay near zero unless
   `min_activity: 0`.

### A tool fails

| Stage | Look at |
|---|---|
| `arguments` | The message names the parameter and the rule. The agent usually corrects itself. |
| `worker` | Reachability, credentials and timeouts of the source; for PromQL errors, the rendered request. |
| `process` | The processor and its message; often "expected …, got …" when a query returns a different shape than the tool expects, for example an instant result where a range is needed. |
| `request`, `jq`, `render` | A bug in the tool file or data the tool did not expect (a metric that is missing on some hosts). See [Templates and jq](templates-and-jq.md#debugging). |

### Calls are slow or time out

- Narrow the call (`instance`, a shorter `range`) or lower the resolution (`step`).
- Raise the instance `timeout`.
- Check the query cost in Prometheus: fleet-wide subqueries and regex selectors over thousands of series are
  expensive.

### The agent picks the wrong tool or wrong arguments

The agent decides from the tool descriptions. If it keeps misusing a tool, the description is the place to fix:
say when to use the tool, how it differs from its neighbours and what its parameters mean. See
[Writing tools](writing-tools.md#writing-the-description).
