# Ocellus AI MCP

**Ready-made DevOps tools for AI agents, with the data analysis done on the server.**

Ocellus AI is an [MCP](https://modelcontextprotocol.io) server written in Go. Each tool is one YAML file that says
where to get the data, how to shape and analyse it, and what the agent gets back. Instead of a raw PromQL endpoint
and thousands of samples, the agent calls a typed tool and reads a few lines of findings.

## Features

**Connect different sources**

- **Prometheus**: instant and range PromQL queries against Prometheus or any backend that serves its HTTP API.
- **Command-line tools** such as `kubectl` or `df`: started from an allowlist, without a shell, in an isolated
  environment.
- **Any JSON HTTP API**, such as Alertmanager or GitLab. The base URL and credentials stay in the server config.
- **Several instances of each source**: production and staging Prometheus side by side, a read-only API next to a
  writable one.
- **Multi-step tools**: call several sources in sequence and build the next request from earlier results. For
  example, list pods with `kubectl`, then ask Prometheus about exactly those pods.

**Process the data before the agent sees it**

- **Filter, aggregate and reshape** with jq at any step; the tool's parameters are available inside the expression.
- **Combine** the results of different calls into one answer, or join several metrics per host into one
  multivariate series.
- **Answer in prose**: a Go template turns the result into text for the agent, and the structured data comes along
  as `structuredContent`.
- **Typed parameters**: defaults, enums, ranges and patterns become the tool's JSON Schema and are checked on every
  call.

**Built-in analysis and anomaly detection**

- **Point anomalies and outliers**: z-score and IQR per series, outliers across a set of pods or disks, Mahalanobis
  distance across metrics.
- **Anomaly ensemble**: Matrix Profile, AR residual, level shift, seasonal residual, Isolation Forest and PCA, each
  with an adaptive threshold from extreme value theory, plus attribution of the metric to blame.
- **Noise floor in real units**: a rare but tiny change, such as iowait going from 0.1% to 1%, is left out.
- **Incidents across hosts**: simultaneous anomalies on many hosts are folded into one line.
- **Trend, cycles and forecast** by Singular Spectrum Analysis, with detection of regime changes and scheduled daily
  jobs.
- Everything runs in the Go binary: no Python runtime, no external service.

**A catalog to start from**

- A working catalog of 23 tools: an overview of the whole estate, alerts, Kubernetes pods, virtual machines,
  PostgreSQL, Kafka, HAProxy and blackbox probes, anomaly analysis and forecasts. Plus 18 smaller reference tools that
  the tests run and that show each feature on its own. See the [tool catalog](#tool-catalog).
- A new tool is a new YAML file, no code. The whole catalog is validated at startup, and every error names the file
  and the field.

**Simple to run**

- One static binary, stdio and Streamable HTTP transports on the official
  [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), a distroless image and a Kubernetes manifest.

## What the agent sees

A tool that joins CPU and memory per VM and runs the anomaly ensemble over them
([`vm_anomaly_ensemble.yaml`](internal/processor/ensemble/testdata/vm_anomaly_ensemble.yaml)). The agent calls it with
`{"instance": "vm-.*", "range": "12h"}` and receives:

```text
1 anomalous segment(s) over the last 12h (2 VM(s) analyzed, 0 skipped; window 1h, risk 0.0001):
vm-1:9100: 1 segment(s), max score 70.2
  - 2025-09-11T22:33:20Z .. 2025-09-11T22:53:20Z (21 samples, single, score 70.2; fired by pca(k=0)), driven by mem (own z 20.6, PCA share 0.49); mem 60.4% at the peak vs 44.9% usually
```

Two VMs, two metrics and 720 one-minute samples each went in; one line came out. It says when, which metric, how far
it moved from its usual level and which detector fired. The full report is returned as `structuredContent` for
agents that want to dig further. (This output comes from the repository's end-to-end test: on synthetic data,
vm-1's memory jumps by 12 points for 21 minutes while its CPU stays normal.)

## Tool catalog

The working catalog is [`tools/`](tools): the example config, the Docker image and the Kubernetes manifest load it.

| Area | Tools | Built on |
|---|---|---|
| Estate overview | `estate_overview`: targets down, failed probes, firing alerts, full filesystems, restarting pods and overloaded VMs in one call | Prometheus |
| Alerts | `alerts_firing` | Prometheus |
| Kubernetes workloads | `pod_memory_usage`, `pod_restarts`; outliers among pods: `pod_memory_outliers`, `pod_restart_outliers` | Prometheus, kube-state-metrics |
| Virtual machines | `vm_metrics` (a snapshot of one VM found by name), `vm_steal_time`, `node_cpu_anomalies`, `node_signal_window` (raw samples around a moment) | node_exporter |
| VM anomaly analysis | `node_deep_ensemble` (ten signals per VM analysed together), `node_cpu_mem_ensemble`, `node_cpu_mem_mv`, `node_disk_pressure_mv`, `node_cluster_events` (incidents across many VMs) | node_exporter + `anomaly_ensemble`, `anomaly_mv`, `cluster_events` |
| Trends and forecasts | `node_ssa`, `node_mssa`, `node_forecast` (a summary for an interval such as "tomorrow 09:00–18:00") | node_exporter + `ssa`, `ssa_mv` |
| PostgreSQL | `pg_slowdown_check` (host, database and replication signals against the same window a week earlier), `pg_xact_anomalies` | node_exporter, postgres_exporter |
| Kafka | `kafka_lag_anomalies` | kafka_exporter |
| HAProxy and probes | `haproxy_backend_health`, `probe_latency_anomalies` | haproxy_exporter, blackbox_exporter |

Tools that look at a time window take `at` (now by default), so an agent can examine a past incident the same way as
the present. The queries assume common exporter job names, such as `job="node_exporters"`; each file says what it
expects in its header comment. Copy what fits, adjust the selectors to your setup, and put the files in your
`tools_dir`.

[`tools-test/`](tools-test) holds the tools the test suite loads and calls against mocked Prometheus, Alertmanager and
`kubectl`. They work against the real ones too, and show each feature on a small scale:

| Area | Tools | Shows |
|---|---|---|
| Scrape targets | `targets_up`, `targets_down`, `targets_down_summary` | the three response levels: raw JSON, jq, jq + text |
| Kubernetes workloads | `pod_cpu_usage`, `namespace_cpu_timeseries`, `pods_status`, `pods_cpu_status`, `pod_cpu_anomalies`, `pod_cpu_outliers` | instant and range queries, `kubectl`, a two-step tool, `anomaly`, `outliers` |
| Disks | `disk_usage`, `disk_usage_outliers` | a CLI parsed line by line, jq into `outliers` |
| Alertmanager | `alertmanager_silences`, `alertmanager_silence_create`, `alertmanager_silence_expire` | the `rest` worker: GET, POST and DELETE |

Four more analysis tools sit next to the end-to-end tests that run them: `vm_anomaly_ensemble`
([`ensemble`](internal/processor/ensemble/testdata)), `vm_multivariate_anomalies`
([`anomalymv`](internal/processor/anomalymv/testdata)), `vm_cpu_forecast` and `vm_cpu_mem_mssa`
([`ssaproc`](internal/processor/ssaproc/testdata)).

## How it works

```
AI agent ──MCP──▶ ocellusai-mcp ──▶ tool.yaml
                                   params → [request → worker → jq]* → process* → jq → render
                                                         │
                                              prometheus | shell | rest
```

1. The arguments are validated against the tool's schema and defaults are filled in.
2. Each call renders its request from a template and runs it on a worker: `prometheus`, `shell` or `rest`. An
   optional jq expression shapes the result for the next call.
3. The `process` chain transforms or analyses the data: jq steps, joins, detectors, forecasts.
4. A final jq expression picks what matters, and a template renders the text the agent reads.

The analysis steps:

| Processor | Input | What it does |
|---|---|---|
| `jq` | anything | Reshapes data between steps (gojq, tool parameters available as `$params`). |
| `join` | several range queries | Stitches them into one multivariate series per host or label set. |
| `anomaly` | time series | Point anomalies per series: `zscore`, `iqr`. |
| `anomaly_mv` | multivariate series | Points where the usual relation between metrics breaks (Mahalanobis distance). |
| `outliers` | list of objects | Outliers in a set with no time axis: pods, disks, targets. |
| `anomaly_ensemble` | (multivariate) series | Anomalous segments found by an ensemble of detectors, with adaptive thresholds and attribution. |
| `cluster_events` | list of events | Folds simultaneous segments on many hosts into incidents. |
| `ssa`, `ssa_mv` | series, multivariate series | Trend, cycles, noise and a forecast by Singular Spectrum Analysis. |

### Anomaly ensemble

`anomaly_ensemble` finds anomalous *segments* (start, end, peak) in one metric or several metrics analysed together.
Detectors:

- **Matrix Profile** (STOMP, and mSTOMP across metrics): shapes unlike anything else in the window;
- **AR residual**: values an autoregressive model did not expect;
- **level shift**: a series that moved to a new level, even slowly;
- **seasonal residual**: deviation from the same phase of previous cycles;
- **Isolation Forest**: rare combinations of values;
- **PCA** (SPE and Hotelling T²): points that break the usual correlation between metrics or sit far from the usual
  state.

Each detector gets its own threshold from extreme value theory (Peaks Over Threshold) at a chosen false-positive
`risk` per sample, so a quiet host and a busy one are judged on their own scale. Every segment is attributed: its
kind (`single`: one metric misbehaves on its own; `correlation`: no metric is unusual alone but their relation
broke; `systemic`: several metrics at once), the metrics that drive it, the value at the peak against the usual
level and the detectors that fired.

A **noise floor in absolute units** (`min_delta`, `min_rel_delta`) keeps statistically rare but meaningless moves
out of the report. The same keys work in `anomaly`, `anomaly_mv` and `outliers`.

### Incidents across hosts

`cluster_events` takes the segments of many series and groups those that overlap in time, optionally only when they
share a signal. Seventy-odd segments on twelve database nodes become a handful of lines such as

```text
00:09..00:33 (24 min): net_tx(9), net_rx(7), cpu(6), iowait(6), disk(4) on 11 VM(s)
```

plus the events that happened on one host only.

### Singular Spectrum Analysis

`ssa` decomposes a series, and `ssa_mv` several metrics of one host together, into a trend, cycles and noise, and
forecasts the structure. On top of the textbook method they:

- estimate the noise robustly, so bursty series don't turn the whole spectrum into "structure";
- build a robust daily profile and flag stretches that fall outside it: long stretches in another regime (an
  outage and the catch-up after it), scheduled daily jobs, and bursts;
- call a period of 23 or 25 hours measured over a few days what it is, a daily cycle;
- leave weak cycles with a growing amplitude out of the default forecast, and warn when the forecast cannot be
  trusted.


## Quick start

You need Go 1.27.1 or newer and a Prometheus to query.

```bash
git clone https://github.com/ocellus-ai/ocellusai-mcp.git
cd ocellusai-mcp
make build                          # → bin/ocellusai-mcp
cp config.example.yaml config.yaml  # set workers.prometheus.url
```

Check that the catalog loads, then start the server:

```bash
./bin/ocellusai-mcp -config config.yaml -validate
```

```bash
./bin/ocellusai-mcp -config config.yaml
```

The example config serves Streamable HTTP on `:8080`: the MCP endpoint is `/mcp`, the health check is `/healthz`.

### Connect an MCP client

Over HTTP, for example from Claude Code:

```bash
claude mcp add --transport http ocellus http://localhost:8080/mcp
```

Over stdio, set `server.transport: stdio` in the config and let the client start the binary. Use absolute paths,
including `tools_dir` in the config, because the client decides the working directory:

```bash
claude mcp add ocellus -- /path/to/bin/ocellusai-mcp -config /path/to/config.yaml
```

Claude Desktop and other clients that take a JSON config:

```json
{
  "mcpServers": {
    "ocellus": {
      "command": "/path/to/bin/ocellusai-mcp",
      "args": ["-config", "/path/to/config.yaml"]
    }
  }
}
```

Without a client, [`scripts/mcp-call.py`](scripts/mcp-call.py) (Python standard library only) starts the server
over stdio and lists or calls tools. It needs a config with `server.transport: stdio`:

```bash
python3 scripts/mcp-call.py -config config.stdio.yaml call estate_overview
```

## Writing a tool

A complete tool from `tools-test/`, the top pods by CPU in a namespace:

```yaml
name: pod_cpu_usage
description: |
  CPU usage (cores) per pod in a namespace, averaged over the last window.
  Returns the top N pods sorted by usage, highest first.
worker: prometheus

params:
  namespace: {type: string, required: true, description: "Kubernetes namespace"}
  window:    {type: string, default: "5m", description: "Rate window such as 5m or 1h", pattern: "^[0-9]+[smhdwy]$"}
  top:       {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many pods to return"}

request:
  type: instant
  query: |
    topk({{ .top }}, sum by (pod) (
      rate(container_cpu_usage_seconds_total{namespace={{ promLabel .namespace }}, container!=""}[{{ promDuration .window }}])
    ))

response:
  jq: |
    [.result[] | {pod: .metric.pod, cores: (.value[1] | tonumber)}] | sort_by(-.cores)
  render: |
    {{ if not . }}No CPU samples for namespace {{ param "namespace" }} in the last {{ param "window" }}.{{ else }}
    Top {{ len . }} pods by CPU in {{ param "namespace" }} (last {{ param "window" }}):
    {{ range . }}{{ .pod }}: {{ printf "%.3f" (float64 .cores) }} cores
    {{ end }}{{ end }}
```

- `description` goes to the agent as is, so write it for a model: what the tool returns and when to use it.
- `request` strings are Go templates with [sprig](https://masterminds.github.io/sprig/) functions; the root is the
  tool's parameters. `promLabel`, `promRegex` and `promDuration` escape values for PromQL.
- `response` is optional. Without it the agent gets the worker's JSON; with `jq` only, the result of the expression.

Analysis is a `process` section between the request and the response. Here is the outline of the ensemble tool from
the top of this page, with a noise floor added:

```yaml
calls:                                  # two range queries, results named cpu and mem
  - name: cpu
    worker: prometheus
    request:
      type: range
      query: 100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))
      start: "now-{{ promDuration .range }}"
      end: now
      step: 60s
  - name: mem
    worker: prometheus
    request: {type: range, query: "...", start: "now-{{ promDuration .range }}", end: now, step: 60s}

process:
  - fn: join                            # {cpu, mem} → one (cpu, mem) series per instance
    with: {inputs: [cpu, mem], join_by: [instance]}
  - fn: anomaly_ensemble
    with:
      window: 1h                        # typical length of a shape to compare
      risk: "{{ .risk }}"               # false positives per sample and detector
      min_delta: {cpu: 5, mem: 3}       # smaller moves are noise, in percentage points
      max_ranges: 10
```

## Configuration

```yaml
server:
  transport: http                 # stdio | http
  listen: ":8080"
tools_dir: ./tools                # every *.yaml here is a tool

workers:                          # each entry is an instance; tools refer to it by name
  prometheus:                     # the type defaults to the entry name
    url: http://prometheus.monitoring:9090
    headers: {Authorization: "Bearer ${PROM_TOKEN}"}
  prom_staging:
    type: prometheus
    url: http://prometheus.staging:9090
  shell:
    allowlist: [kubectl, df]
    env: {KUBECONFIG: /etc/kube/config}   # the child process gets PATH plus these, nothing else
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
    methods: [GET]                # a read-only instance

log:
  level: info
  format: json                    # always to stderr
```

`${VAR}` is taken from the environment and an unset variable stops the start. Unknown fields in the config or in a
tool are errors too. [`config.example.yaml`](config.example.yaml) lists every option with comments.

## Security model

- **shell:** only binaries from the instance's allowlist, started directly without `sh -c`, so a parameter value
  cannot inject shell syntax. The child environment is `PATH` plus the variables you list. Timeouts and output
  limits apply. Constrain parameters that end up in arguments with `pattern` or `enum`.
- **rest:** the base URL, credentials and allowed methods are server config; a tool cannot choose a host, and its
  path cannot climb above the base path. Redirects are not followed, responses are size-limited, and headers and
  query strings are never logged.
- **Prometheus:** `promLabel` and `promRegex` escape parameter values inside PromQL.
- **Not yet:** the HTTP transport has **no authentication**. Keep it on a private network or behind an
  authenticating proxy.

## Deployment

```bash
make docker   # distroless, non-root, no CLI binaries inside
```

[`deploy/k8s.yaml`](deploy/k8s.yaml) has a Deployment, a Service and a ConfigMap with the config.
The tool catalog is a second ConfigMap built from the directory:

```bash
kubectl -n monitoring create configmap ocellusai-mcp-tools --from-file=tools/ --dry-run=client -o yaml | kubectl apply -f -
kubectl -n monitoring apply -f deploy/k8s.yaml
```

The catalog is read at startup, so restart the Deployment after changing it.

## Extending in Go

Two interfaces cover most extensions. A worker is a new data source:

```go
type Worker interface {
    Type() string
    Validate(req map[string]any) error                            // the raw request, at startup
    Execute(ctx context.Context, req map[string]any) (any, error) // the rendered request → JSON-compatible value
}
```

A processor is a new kind of analysis or transformation:

```go
type Processor interface {
    Name() string
    Validate(with map[string]any) error
    Run(ctx context.Context, with map[string]any, data any, params map[string]any) (any, error)
}
```

The detectors inside `anomaly` and `anomaly_mv` have their own small interface, so a new algorithm there is one file
and one line in a table.

## Development

```bash
make test         # go test ./...
make test-race    # with the race detector
make vet lint     # golangci-lint v2
make validate     # build and load the example catalog
```

## Status and roadmap

ocellusai-mcp is young. The core is covered by tests and has been run against a production Prometheus, but the config
and YAML formats may still change. Not done yet:

- authentication on the HTTP transport;
- MCP tool annotations (read-only and destructive hints);
- reloading the catalog without a restart;
- a Loki worker, `/metrics`, caching and rate limits.

---

*Ocellus* is Latin for "little eye", the simple eye insects use to notice light and movement.
