# Writing tools

[For DevOps](for-devops.md) · [Quick start](quick-start.md) · [User guide](user-guide.md) · [Workers](workers.md) · [Templates and jq](templates-and-jq.md) · [Processors](processors.md) · **Writing tools**

A tool is one YAML file: it says where the data comes from, how to analyse it and what the agent reads back. A new
tool needs no code and no rebuild, only a file in the catalog and a restart. This guide shows how to design and
write your own tools, from the question you want an agent to answer to a validated file in production.

It assumes a running server ([Quick start](quick-start.md)). The languages inside a tool file are explained in
[Templates and jq](templates-and-jq.md), the sources in [Workers](workers.md), the analysis steps in
[Processors](processors.md); this guide shows how to put them together.

## Contents

- [The workflow](#the-workflow)
- [Design before you write](#design-before-you-write)
- [Anatomy of a tool file](#anatomy-of-a-tool-file)
- [Tutorial: your first tool](#tutorial-your-first-tool)
- [Patterns](#patterns)
- [Writing the description](#writing-the-description)
- [Designing parameters](#designing-parameters)
- [Designing the answer](#designing-the-answer)
- [Thresholds for detectors](#thresholds-for-detectors)
- [Safety](#safety)
- [Validate and test](#validate-and-test)
- [Common mistakes](#common-mistakes)
- [Deploy](#deploy)
- [Checklist](#checklist)
- [Let an AI agent write the tool](#let-an-ai-agent-write-the-tool)

## The workflow

1. **Start from the question** the agent should be able to answer, in the words a person would use.
2. **Find the data**: which source holds it, which metric or command or endpoint, which labels identify the object.
3. **Pick a pattern**: a snapshot, anomalies in history, outliers among peers, segments across metrics, a forecast, a
   command, an API call.
4. **Write the file**, starting from the closest example.
5. **Validate** it with `-validate`.
6. **Call it**: with the defaults, with a filter that matches nothing, with a bad argument.
7. **Deploy**: put the file in the catalog and restart the server.

Most of the time goes into steps 1–3. A tool that answers a clear question with a compact answer is used well by
agents; a tool that dumps raw data is not.

## Design before you write

Answer these questions first. Writing them down as a short spec saves rewrites, and it is the best thing to show a
colleague before the YAML exists.

**The question.** One or two example questions, phrased as an agent or a person would ask them: "Which VMs are
busiest right now?", "Did any VM's memory jump in the last six hours?". If you cannot phrase the question, the tool
will be vague too.

**The objects and how they are identified.** VMs by `instance`, pods by `namespace` and `pod`, databases by
`datname`, filesystems by `instance` and `mountpoint`. How many there are matters: five objects can be listed, five
hundred must be filtered, ranked and capped.

**The source.** A Prometheus metric, a command such as `df`, or an endpoint of a JSON API, and the
[worker instance](workers.md#types-and-instances) that reaches it. If the instance does not exist yet (a new API, a
command not in the allowlist), the operator has to add it to the config first.

**The time scope.**

| The question is about | Use |
|---|---|
| the current state, a top-N, peers compared now | an `instant` query |
| an average, a peak or time above a threshold over a window | an `instant` query with `rate`, `max_over_time` or a subquery |
| what happened over time: anomalies, segments, trends, forecasts | a `range` query |

Every tool that looks at time should accept `at`, so that the agent can examine a past incident the same way as the
present.

**The analysis.**

| The question | Pattern |
|---|---|
| "Show me the values, sorted" | no analysis: jq sorts and caps |
| "Above a fixed threshold" | a filter in PromQL: `> {{ .threshold }}` |
| "Unusual compared with its own history" | a `range` query → [`anomaly`](processors.md#anomaly) |
| "Stands out from the others" | an `instant` query → jq → [`outliers`](processors.md#outliers) |
| "When did the relation between X and Y break" | `calls` → `join` → [`anomaly_mv`](processors.md#anomaly_mv) |
| "Anomalous periods, and which metric caused them" | `calls` → `join` → [`anomaly_ensemble`](processors.md#anomaly_ensemble) |
| "The same problem on many hosts at once" | ensemble → jq → [`cluster_events`](processors.md#cluster_events) |
| "Trend, daily pattern, what to expect tomorrow" | a `range` query → [`ssa`](processors.md#ssa-and-ssa_mv) |
| "List from one system, numbers from another" | [`calls`](templates-and-jq.md#results-of-earlier-calls) |

**The answer.** Which fields per object, in which units, sorted how, how many rows. What the answer says when there
is nothing to report. Write two or three lines of the text you would like the agent to read; it is the fastest way to
agree on the output.

**The parameters.** What the caller may change (filters, window, threshold, how many rows, `at`) and what is fixed in
the tool (the metric, the method).

A spec can be this short:

```text
Tool: vm_memory_jumps (tools/vm_memory_jumps.yaml)
Answers: did any VM's memory use jump unusually in the last hours, and when
Source: Prometheus "prometheus", node_exporter MemAvailable / MemTotal, per instance
Parameters: instance (regex, .*), at (now), range (6h), min_points (3), top (10)
Analysis: anomaly, IQR k=3, upward only, at least min_points above the VM's median
Answer: "31 memory jump sample(s) on 1 of 3 VM(s) over the 6h ending at …"
        "web-2:9100: 31 sample(s), usually 42.8%"
        "  - 2026-03-12T08:24:00Z: 72.4% (score 14.3)"
Empty: "No memory jumps of 3+ points among 3 VM(s) over the 6h ending at …"
```

## Anatomy of a tool file

| Field | Required | Purpose |
|---|---|---|
| `name` | yes | the name the agent calls: `a-z`, `0-9`, `_`; unique in the catalog |
| `description` | yes | what the agent reads to decide whether and how to call the tool |
| `worker` | yes, unless `calls` | the worker instance that answers |
| `params` | no | the parameters, which become the tool's JSON Schema |
| `request` | yes, unless `calls` | the request for the worker; its keys depend on the worker type |
| `calls` | instead of `worker` + `request` | several named requests in sequence |
| `process` | no | the analysis chain: a list of `{fn, with}` steps |
| `response.jq` | no | the final reshaping of the data |
| `response.render` | no | the text the agent reads |

Conventions used across the catalog:

- The file is named after the tool: `tools/<name>.yaml`.
- Names follow `<object>_<what>`: `vm_cpu_top`, `pod_restarts`, `kafka_lag_anomalies`. No `get_` or `list_`.
- The fields come in this order: `name`, `description`, `worker`, `params`, `request`, `process`, `response` (with
  `calls`: `name`, `description`, `params`, `calls`, `process`, `response`).
- The file starts with a `#` comment that says what the tool does and what it assumes about the data (exporter,
  `job` label), so the next person can adapt it.
- Descriptions and texts are in English unless your agents work in another language.

Parsing is strict: a field that does not exist (`title`, `annotations`, a misspelt `descripton`) is an error, and
**one broken file stops the whole server**. Validate every file before it reaches a running server.

## Tutorial: your first tool

The goal: a tool that answers "Which VMs are busiest right now?".

### Step 1: find the data

CPU load comes from node_exporter. Check what your Prometheus calls the scrape job and which labels the metric has:

```bash
curl -s http://prometheus:9090/api/v1/query --data-urlencode 'query=count by (job) (up)'
```

```bash
curl -s http://prometheus:9090/api/v1/query --data-urlencode 'query=topk(3, node_cpu_seconds_total{mode="idle"})'
```

The busy share of a VM's CPU is one minus the idle share, averaged over its cores:

```promql
100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))
```

Run it in the Prometheus UI first. A query that works there is half the tool.

### Step 2: the smallest tool that works

Create `tools/vm_cpu_top.yaml`:

```yaml
name: vm_cpu_top
description: Busiest VMs by CPU.
worker: prometheus
request:
  type: instant
  query: 100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))
```

Validate the catalog:

```bash
./bin/ocellusai-mcp -config config.yaml -validate
```

```text
vm_cpu_top                       worker=prometheus   type=prometheus calls=-            process=-            file=tools/vm_cpu_top.yaml
```

Call it (with a config that has `server.transport: stdio`, see [Validate and test](#validate-and-test)):

```bash
python3 scripts/mcp-call.py -quiet -config config.stdio.yaml call vm_cpu_top '{}'
```

Without a `response` section the agent gets the worker's answer as it is, the Prometheus vector:

```json
{
  "result": [
    {"metric": {"instance": "web-1:9100"}, "value": [1791050275.424, "30.4953"]},
    {"metric": {"instance": "web-2:9100"}, "value": [1791050275.424, "29.8038"]},
    {"metric": {"instance": "db-1:9100"}, "value": [1791050275.424, "20.7505"]}
  ],
  "resultType": "vector"
}
```

It works, but the agent has to dig through labels, string values and timestamps.

### Step 3: shape the answer with jq

```yaml
response:
  jq: |
    [ .result[] | {instance: .metric.instance, cpu_pct: (.value[1] | tonumber? // null)} ]
    | sort_by(-(.cpu_pct // -1))
```

```json
[
  {"cpu_pct": 34.1419, "instance": "web-1:9100"},
  {"cpu_pct": 31.3392, "instance": "web-2:9100"},
  {"cpu_pct": 22.5508, "instance": "db-1:9100"}
]
```

`tonumber? // null` turns Prometheus' string values into numbers and a `NaN` into `null`; `sort_by` puts the busiest
first. The agent now gets a clean list, and `structuredContent` carries the same data (wrapped as `{"result": […]}`
because it is a list).

### Step 4: parameters, text and description

The finished tool:

```yaml
# The busiest VMs by CPU at a moment. Assumes node_exporter; add a job="..."
# filter to the selector for your setup.
name: vm_cpu_top
description: |
  The busiest virtual machines by CPU (node_exporter) at time `at`: busy %
  of all cores (100 minus idle), averaged over `window`. Returns the `top`
  VMs, busiest first, and how many VMs matched `instance` (a regex over the
  instance label). Busy % includes iowait and steal. An empty answer means
  that no VM matched the filter at that time, not that CPU is idle. Use it
  for a quick look at CPU load across the fleet; for a VM's history use an
  anomaly tool.
worker: prometheus

params:
  instance: {type: string, default: ".*", description: "Regex over the instance label, e.g. web-.* or db-0[1-3].*"}
  at:       {type: string, default: "now", description: "Evaluation time: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  window:   {type: string, default: "5m", description: "Averaging window such as 5m or 1h", pattern: "^[0-9]+[smhdwy]$"}
  top:      {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many VMs to list"}

request:
  type: instant
  time: "{{ .at }}"
  query: |
    100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle", instance=~{{ promLabel .instance }}}[{{ promDuration .window }}])))

response:
  jq: |
    [ .result[] | {instance: .metric.instance, cpu_pct: (.value[1] | tonumber? // null)} ] as $vms
    | {
        at: $params.at,
        matched: ($vms | length),
        vms: ($vms | sort_by(-(.cpu_pct // -1)) | .[:$params.top])
      }
  render: |
    {{- if eq (int .matched) 0 }}No VMs matching {{ param "instance" }} reported CPU at {{ .at }}.{{ else -}}
    Top {{ len .vms }} of {{ .matched }} VM(s) by CPU busy % at {{ .at }} ({{ param "window" }} average):
    {{ range .vms }}- {{ .instance }}: {{ if kindIs "invalid" .cpu_pct }}n/a{{ else }}{{ printf "%.1f%%" (float64 .cpu_pct) }}{{ end }}
    {{ end }}{{ end }}
```

What changed:

- **Parameters.** `instance` filters the fleet by regex, `at` looks at a past moment, `window` sets the averaging,
  `top` caps the list. Each has a default, so the agent can call the tool with no arguments, and each is
  constrained: the pattern of `at` and `window`, the bounds of `top`.
- **Safe templates.** The regex goes into PromQL through `promLabel`, the window through `promDuration`, so no value
  can break the query. `time: "{{ .at }}"` is quoted because it starts with `{{`.
- **A summary in jq.** The answer is an object: the moment, how many VMs matched, and the capped list. The number of
  matches tells the agent whether `top` hid anything.
- **A text for the model.** `render` handles the empty case first, names the parameters in the text, formats
  numbers (`float64` before `printf`) and prints `n/a` for a missing value.
- **A real description**: what is returned, from where, in which units, what an empty answer means and when to use
  the tool.

### Step 5: try it

A call with arguments:

```bash
python3 scripts/mcp-call.py -quiet -config config.stdio.yaml call vm_cpu_top '{"at": "2026-03-12T09:40:00Z", "top": 2}'
```

```text
Top 2 of 3 VM(s) by CPU busy % at 2026-03-12T09:40:00Z (5m average):
- web-1:9100: 49.4%
- web-2:9100: 30.9%
```

A filter that matches nothing must give a clear sentence, not an error:

```text
No VMs matching mail-.* reported CPU at now.
```

A bad argument must be rejected before anything runs:

```text
tool vm_cpu_top failed at stage arguments: validating root: validating /properties/window: pattern: "five minutes" does not match regular expression "^[0-9]+[smhdwy]$"
```

That is a complete tool. The rest of this guide shows other patterns and the rules behind the choices made here.

## Patterns

Each example below is a complete file that passes `-validate`. The outputs come from calls against test data with
three VMs (web-1, web-2, db-1); adapt the selectors (add your `job` label) before using them.

### Anomalies in each object's own history

"Did any VM's memory jump in the last six hours?" A `range` query gives each VM's history; the `anomaly` processor
compares every sample with the rest of that VM's history.

```yaml
# Memory jumps per VM against the VM's own history (anomaly, IQR). Assumes
# node_exporter; add a job="..." filter for your setup.
name: vm_memory_jumps
description: |
  Unusual memory-use increases per virtual machine (node_exporter) over the
  `range` ending at `at`: samples above the IQR fences of the VM's own
  history in that window, kept only if memory rose at least `min_points`
  percentage points above the VM's median. Returns how many VMs were
  analysed and, per VM with jumps, the usual (median) use and the strongest
  jumps with time, value and score. An empty list means no jump of that size,
  not that memory is fine. `instance` is a regex over the instance label.
worker: prometheus

params:
  instance:   {type: string, default: ".*", description: "Regex over the instance label, e.g. web-.*"}
  at:         {type: string, default: "now", description: "End of the window: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  range:      {type: string, default: "6h", description: "Look-back range ending at `at`, e.g. 2h, 1d", pattern: "^[0-9]+[smhdwy]$"}
  min_points: {type: number, default: 3, minimum: 0, maximum: 100, description: "Smallest rise above the usual level worth reporting, percentage points"}
  top:        {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many VMs to list"}

request:
  type: range
  query: |
    max by (instance) (100 * (1 - node_memory_MemAvailable_bytes{instance=~{{ promLabel .instance }}}
                                / node_memory_MemTotal_bytes{instance=~{{ promLabel .instance }}}))
  start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
  end: "{{ .at }}"
  step: 60s

process:
  - fn: anomaly
    with:
      method: iqr
      k: 3                          # only clear jumps
      direction: up
      min_points: 60                # at least an hour of samples
      min_delta: "{{ .min_points }}"
      max_anomalies: 3

response:
  jq: |
    [ .series[] | select(.skipped | not) | select(.anomaly_count > 0)
      | {instance: .metric.instance, count: .anomaly_count, median: .stats.median,
         jumps: [.anomalies[] | {time, value, score}]} ] as $vms
    | {
        at: $params.at, range: $params.range,
        analyzed: .series_analyzed, skipped: .series_skipped,
        raw_total: (.total_anomalies + ([.series[].below_min_delta // 0] | add // 0)),
        total: .total_anomalies,
        vms: ($vms | sort_by(-.count) | .[:$params.top])
      }
  render: |
    {{- if eq (int .total) 0 }}No memory jumps of {{ param "min_points" }}+ points among {{ .analyzed }} VM(s) over the {{ param "range" }} ending at {{ .at }} ({{ .raw_total }} smaller one(s) ignored, {{ .skipped }} VM(s) skipped).{{ else -}}
    {{ .total }} memory jump sample(s) on {{ len .vms }} of {{ .analyzed }} VM(s) over the {{ param "range" }} ending at {{ .at }} (at least {{ param "min_points" }} points above the usual level; {{ sub (int .raw_total) (int .total) }} smaller ignored):
    {{ range .vms }}{{ .instance }}: {{ .count }} sample(s), usually {{ printf "%.1f%%" (float64 .median) }}
    {{ range .jumps }}  - {{ .time }}: {{ printf "%.1f%%" (float64 .value) }} (score {{ printf "%.1f" (float64 .score) }})
    {{ end }}{{ end }}{{ end }}
```

```text
31 memory jump sample(s) on 1 of 3 VM(s) over the 6h ending at 2026-03-12T12:00:00Z (at least 3 points above the usual level; 0 smaller ignored):
web-2:9100: 31 sample(s), usually 42.8%
  - 2026-03-12T08:24:00Z: 72.4% (score 14.3)
  - 2026-03-12T08:29:00Z: 72.1% (score 14.1)
  - 2026-03-12T08:37:00Z: 72.1% (score 14.1)
```

Points to note:

- The **start of the range** is computed from `at` and `range` by a template that every window-based tool in the
  catalog uses; copy it as it is ([how it works](templates-and-jq.md#a-window-that-ends-at-at)).
- `max by (instance)` guarantees one series per VM even if a label changes inside the window.
- `min_delta` is the [noise floor](#thresholds-for-detectors): the smallest rise worth reporting, given to the caller
  in percentage points. `max_anomalies: 3` keeps the strongest three per VM; the count still covers all of them.
- `raw_total` counts the findings before the noise floor, so the text can say how many smaller ones were ignored.

### Outliers among peers

"Is any filesystem much fuller than the others?" An `instant` query gives one value per object; a `jq` step turns it
into the list `outliers` expects; `outliers` compares the objects with each other.

```yaml
# Filesystems much fuller than the others (outliers, IQR) at a moment.
# Assumes node_exporter; add a job="..." filter for your setup.
name: fs_fill_outliers
description: |
  Filesystems (node_exporter) that are much fuller than the other
  filesystems of the matching VMs at time `at`: used % compared across all
  of them with Tukey (IQR) fences, kept only if at least `min_points`
  percentage points above the median. tmpfs and overlay are excluded.
  Returns the median fill, the upper fence and the outliers with VM,
  mountpoint and used %. Use it to find a filesystem that fills up while the
  rest of the fleet does not; for a fixed threshold ("above 90%") a plain
  query is enough.
worker: prometheus

params:
  instance:   {type: string, default: ".*", description: "Regex over the instance label, e.g. web-.*"}
  at:         {type: string, default: "now", description: "Evaluation time: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  min_points: {type: number, default: 20, minimum: 0, maximum: 100, description: "Smallest distance above the median fill worth reporting, percentage points"}

request:
  type: instant
  time: "{{ .at }}"
  query: |
    100 * (1 - node_filesystem_avail_bytes{instance=~{{ promLabel .instance }}, fstype!~"tmpfs|overlay"}
               / node_filesystem_size_bytes{instance=~{{ promLabel .instance }}, fstype!~"tmpfs|overlay"})

process:
  - fn: jq                           # vector → the list outliers expects
    with:
      expr: '[.result[] | {key: "\(.metric.instance) \(.metric.mountpoint)", value: .value[1]}]'
  - fn: outliers
    with:
      method: iqr
      k: 1.5
      direction: up
      min_points: 5
      min_delta: "{{ .min_points }}"
      max_anomalies: 10

response:
  jq: |
    .series[0] as $s
    | {
        at: $params.at, filesystems: $s.points, skipped: $s.skipped, reason: $s.reason,
        median: $s.stats.median, upper: $s.stats.upper,
        outliers: [$s.anomalies[] | {filesystem: .key, used_pct: .value}]
      }
  render: |
    {{- if .skipped }}Too few filesystems to compare at {{ .at }}: {{ .reason }}.{{ else -}}
    {{ .filesystems }} filesystem(s) at {{ .at }}: median {{ printf "%.1f%%" (float64 .median) }} used, upper fence {{ printf "%.1f%%" (float64 .upper) }}.
    {{ if not .outliers }}None is {{ param "min_points" }}+ points fuller than the median.{{ else }}Much fuller than the rest:
    {{ range .outliers }}- {{ .filesystem }}: {{ printf "%.1f%%" (float64 .used_pct) }}
    {{ end }}{{ end }}{{ end }}
```

```text
12 filesystem(s) at 2026-03-12T12:00:00Z: median 41.0% used, upper fence 57.9%.
Much fuller than the rest:
- web-2:9100 /var: 92.5%
```

With a filter that leaves too few objects to compare:

```text
Too few filesystems to compare at now: fewer than 5 items (min_points).
```

- The `jq` step builds `{key, value}` items; `key` names the object in the report.
- Without `group_by` there is exactly one group, so `response.jq` can read `.series[0]` safely.
- Use `iqr` for small sets: a z-score of a handful of objects can never reach 3
  ([why](processors.md#outliers)). Add `group_by` to compare objects only within their group (pods within a
  namespace, filesystems within a host).

### Several metrics: segments and the metric to blame

"When did CPU behave unusually, and was it the VM's own load or a noisy neighbour?" One range call per metric, `join`
stitches them per VM, `anomaly_ensemble` finds anomalous segments and attributes them.

```yaml
# CPU busy % and steal % per VM, joined and analysed by the anomaly ensemble:
# segments, their kind, the metric to blame and the values at the peak.
# Assumes node_exporter; add a job="..." filter for your setup.
name: vm_cpu_steal_segments
description: |
  Anomalous time segments in CPU load and CPU steal (time taken by the
  hypervisor) of each virtual machine (node_exporter) over the `range` ending
  at `at`. Both signals are analysed together by an ensemble of detectors
  with thresholds adapted to each VM's own noise; moves smaller than 5 points
  of CPU or 1 point of steal are treated as noise. Returns, per VM with
  segments, their start and end, the kind (single: one signal alone;
  correlation: the usual relation broke; systemic: both), the signal to blame
  and the values at the peak against the usual level. A segment led by steal
  is a noisy neighbour on the hypervisor; one led by cpu is the VM's own load.

params:
  instance: {type: string, default: ".*", description: "Regex over the instance label, e.g. web-.*"}
  at:       {type: string, default: "now", description: "End of the window: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  range:    {type: string, default: "1d", description: "Look-back range ending at `at`, e.g. 12h, 2d", pattern: "^[0-9]+[smhdwy]$"}
  risk:     {type: number, default: 0.001, minimum: 0.000001, maximum: 0.1, description: "Expected false positives per sample and detector; lower is stricter, 0.01 looks closely"}

calls:
  - name: cpu
    worker: prometheus
    request:
      type: range
      query: 100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle", instance=~{{ promLabel .instance }}}[5m])))
      start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
      end: "{{ .at }}"
      step: 60s
  - name: steal
    worker: prometheus
    request:
      type: range
      query: 100 * avg by (instance) (rate(node_cpu_seconds_total{mode="steal", instance=~{{ promLabel .instance }}}[5m]))
      start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
      end: "{{ .at }}"
      step: 60s

process:
  - fn: join                         # {cpu, steal} → one (cpu, steal) series per VM
    with: {inputs: [cpu, steal], join_by: [instance]}
  - fn: anomaly_ensemble
    with:
      window: 1h
      risk: "{{ .risk }}"
      min_len: 2m
      min_points: 120
      max_ranges: 10
      top_metrics: 2
      min_delta: {cpu: 5, steal: 1}

response:
  jq: |
    [ .series[] | select(.skipped | not) | select(.anomaly_count > 0)
      | .baseline as $b
      | {instance: .labels.instance, count: .anomaly_count,
         segments: [.anomalies[] | {start_time, end_time, kind, score, lead: .top_metrics[0].metric,
                                    cpu: .values.cpu, steal: .values.steal}],
         usual_cpu: $b.cpu, usual_steal: $b.steal} ] as $vms
    | {
        at: $params.at, range: $params.range,
        analyzed: .series_analyzed,
        skipped: [.series[] | select(.skipped) | {instance: .labels.instance, reason}],
        vms: ($vms | sort_by(-.count))
      }
  render: |
    {{- if not .vms }}No anomalous CPU/steal segments among {{ .analyzed }} VM(s) over the {{ param "range" }} ending at {{ .at }} (risk {{ param "risk" }}).{{ else -}}
    CPU/steal segments over the {{ param "range" }} ending at {{ .at }} ({{ .analyzed }} VM(s) analysed, risk {{ param "risk" }}):
    {{ range .vms }}{{ .instance }} (usually cpu {{ printf "%.1f%%" (float64 .usual_cpu) }}, steal {{ printf "%.2f%%" (float64 .usual_steal) }}):
    {{ range .segments }}  - {{ .start_time }} .. {{ .end_time }}: {{ .kind }}, led by {{ .lead }}; at the peak cpu {{ printf "%.1f%%" (float64 .cpu) }}, steal {{ printf "%.1f%%" (float64 .steal) }} (score {{ printf "%.1f" (float64 .score) }})
    {{ end }}{{ end }}{{ end }}
    {{- range .skipped }}
    Skipped {{ .instance }}: {{ .reason }}{{ end }}
```

```text
CPU/steal segments over the 1d ending at 2026-03-12T12:00:00Z (3 VM(s) analysed, risk 0.001):
web-1:9100 (usually cpu 25.1%, steal 0.07%):
  - 2026-03-12T09:30:00Z .. 2026-03-12T09:51:00Z: systemic, led by steal; at the peak cpu 49.7%, steal 14.1% (score 91.2)
```

- A tool with `calls` has **no top-level `worker`**: each call names its own.
- Every call repeats the same `start`, `end` and `step`: `join` matches samples by exact timestamp.
- `min_delta` per metric (`{cpu: 5, steal: 1}`) keeps the near-zero steal of a quiet VM from turning noise into
  segments.
- The answer reads the segment in the metric's units (`values` at the peak, `baseline` as the usual level) and names
  the leading metric from `top_metrics`.
- The bundled [`node_deep_ensemble`](../tools/node_deep_ensemble.yaml) takes the same approach to ten signals.

### Trend and forecast

"What will inbound traffic look like tomorrow?" A `range` query over several days; `ssa` separates the trend and the
daily cycle from noise and continues them.

```yaml
# Forecast of inbound network traffic per VM by Singular Spectrum Analysis,
# summarised as the low, the peak with its time and the value at the end.
# Assumes node_exporter; add a job="..." filter for your setup.
name: vm_traffic_forecast
description: |
  Forecast of inbound network traffic (Mbit/s, node_exporter, all physical
  interfaces) of each virtual machine for `horizon` after `at`, from `range`
  of history, by Singular Spectrum Analysis: the trend and the daily cycle are
  continued, noise and one-off events are not. Returns per VM the forecast
  low, peak (with its time) and end value, the typical noise around it, and
  warnings. A VM whose history holds another regime (an outage, a catch-up) is
  listed last with the stretch in UTC: re-run with a history outside it. The
  history needs at least three days for a daily cycle.
worker: prometheus

params:
  instance: {type: string, default: ".*", description: "Regex over the instance label, e.g. web-.*"}
  at:       {type: string, default: "now", description: "End of the history: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  range:    {type: string, default: "4d", description: "History ending at `at`, at least 3 days, e.g. 4d or 7d", pattern: "^([0-9]+[smhdwy])+$"}
  horizon:  {type: string, default: "1d", description: "How far ahead to forecast, e.g. 6h or 1d", pattern: "^([0-9]+[smhdwy])+$"}

request:
  type: range
  query: |
    8 * sum by (instance) (rate(node_network_receive_bytes_total{instance=~{{ promLabel .instance }}, device!~"lo|veth.*|docker.*|br.*"}[5m])) / 1e6
  start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
  end: "{{ .at }}"
  step: 5m

process:
  - fn: ssa
    with:
      window: 1d                     # a multiple of the daily cycle
      forecast: "{{ .horizon }}"
      clip: "0,1e15"                 # traffic is never negative
      min_points: 864                # three days of 5m samples
      min_delta: 5                   # Mbit/s: smaller stretches off the usual profile are noise
      min_rel_delta: 0.2

response:
  jq: |
    [ .series[] | select(.skipped | not)
      | {instance: .labels.instance, broken, noise_sd: .residual.sd,
         regime: [.runs[] | select(.kind == "long") | "\(.start_time)..\(.end_time)"],
         forecast: (.forecast | if .points then
                      {low: (.points | map(.[1]) | min),
                       peak: (.points | max_by(.[1]) | {value: .[1], time: (.[0] | todate)}),
                       last: .points[-1][1], end_time, warning: (.warning // null)}
                    else {reason} end)} ] as $vms
    | {
        at: $params.at, horizon: $params.horizon,
        skipped: [.series[] | select(.skipped) | {instance: .labels.instance, reason}],
        vms: ($vms | sort_by(.broken, -(.forecast.peak.value // 0)))
      }
  render: |
    {{- if not .vms }}No VM had enough history for a forecast at {{ .at }}{{ range .skipped }}; {{ .instance }}: {{ .reason }}{{ end }}.{{ else -}}
    Inbound traffic forecast for {{ .horizon }} after {{ .at }} (Mbit/s):
    {{ range .vms }}{{ .instance }}: {{ with .forecast }}{{ if hasKey . "reason" }}no forecast ({{ .reason }}){{ else }}{{ printf "%.0f" (float64 .low) }}..{{ printf "%.0f" (float64 .peak.value) }}, peak at {{ .peak.time }}, {{ printf "%.0f" (float64 .last) }} at {{ .end_time }}{{ end }}{{ end }}; actual values scatter by about ±{{ printf "%.1f" (float64 .noise_sd) }}
    {{ if .regime }}  history has another regime ({{ join ", " .regime }}): the forecast is distorted, re-run with a history outside it
    {{ end }}{{ with .forecast.warning }}  warning: {{ . }}
    {{ end }}{{ end }}{{ end }}
```

```text
Inbound traffic forecast for 1d after 2026-03-12T00:00:00Z (Mbit/s):
db-1:9100: 30..90, peak at 2026-03-12T13:50:00Z, 33 at 2026-03-13T00:00:00Z; actual values scatter by about ±3.0
web-1:9100: 30..90, peak at 2026-03-12T13:55:00Z, 33 at 2026-03-13T00:00:00Z; actual values scatter by about ±3.0
web-2:9100: 33..83, peak at 2026-03-12T13:25:00Z, 38 at 2026-03-13T00:00:00Z; actual values scatter by about ±7.9
  history has another regime (2026-03-10T00:30:00Z..2026-03-10T09:25:00Z, 2026-03-10T09:30:00Z..2026-03-10T13:55:00Z): the forecast is distorted, re-run with a history outside it
  warning: the history has 2 stretch(es) in another regime, the longest 2026-03-10T00:30:00Z to 2026-03-10T09:25:00Z (below the usual profile by 41.93): the structure and its forecast are distorted by them, forecast from a history without them
```

- Forecasts need history: at least three periods of the cycle, so four days at 5-minute resolution for a daily one,
  and `window` a multiple of the period (`1d`).
- Don't hand the agent 288 forecast points per day: summarise them in jq (low, peak and its time, end value).
- Show why a forecast should not be trusted (`broken`, `warning`) and put such objects last.
- The bundled [`node_forecast`](../tools/node_forecast.yaml) summarises a forecast over an interval the agent
  chooses ("tomorrow 09:00–18:00"), with time above a threshold.

### A command-line tool

"Which filesystems of the server are filling up?" The `shell` worker runs `df` and splits its lines in jq.

```yaml
# Filesystems of the ocellusai-mcp host that are filling up, via df.
# Needs the shell worker with df in its allowlist.
name: filesystems_filling_up
description: |
  Filesystems of the ocellusai-mcp host whose usage is at or above `threshold`
  percent (df). Filesystems smaller than `min_size_gib` (pseudo filesystems
  such as devfs) are left out. Returns how many filesystems were checked and,
  per full one, its device, mount point, used percent and free space in GiB,
  fullest first. An empty list means no filesystem is that full.

params:
  threshold:    {type: integer, default: 80, minimum: 0, maximum: 100, description: "Used percent at or above which a filesystem is reported"}
  min_size_gib: {type: number, default: 1, minimum: 0, description: "Skip filesystems smaller than this, in GiB"}

worker: shell
request:
  command: df
  args: [-P, -k]          # POSIX format, sizes in KiB: the same columns on Linux and macOS
  parse: lines
  timeout: 10s

response:
  # The numeric columns anchor the match, so spaces in the device or the mount
  # point (macOS "map auto_home") survive; the header line does not match.
  jq: |
    [ .[]
      | capture("^(?<device>.+?) +(?<size>[0-9]+) +[0-9]+ +(?<free>[0-9]+) +(?<used>[0-9]+)% +(?<mount>.+)$")
      | {device, mount, size_kib: (.size | tonumber), free_kib: (.free | tonumber), used_pct: (.used | tonumber)}
      | select(.size_kib >= $params.min_size_gib * 1048576) ] as $fs
    | {
        checked: ($fs | length),
        full: ([ $fs[] | select(.used_pct >= $params.threshold)
                 | {device, mount, used_pct, free_gib: (.free_kib / 1048576)} ] | sort_by(-.used_pct))
      }
  render: |
    {{ if not .full }}None of {{ .checked }} filesystem(s) is {{ param "threshold" }}% full or more.{{ else -}}
    {{ len .full }} of {{ .checked }} filesystem(s) at or above {{ param "threshold" }}%:
    {{ range .full }}- {{ .mount }} ({{ .device }}): {{ .used_pct }}% used, {{ printf "%.1f" (float64 .free_gib) }} GiB free
    {{ end }}{{ end }}
```

```text
2 of 5 filesystem(s) at or above 80%:
- /var/lib/postgresql (/dev/nvme1n1): 91% used, 8.7 GiB free
- / (/dev/nvme0n1p1): 83% used, 5.1 GiB free
```

- `df` must be in the instance's allowlist. It runs on the server's host (or in its container) and sees that host's
  filesystems; for a fleet, use the node_exporter metrics in Prometheus instead.
- `-P -k` fixes the columns and the units, so the same jq works on Linux and macOS.
- `parse: lines` gives one string per line. `capture` with the numeric columns as anchors keeps a device or mount
  point with spaces in one piece, and the header line simply does not match.
- Neither parameter reaches the command: both are read in jq as `$params`, so nothing a caller passes ends up on
  the command line. When a parameter does go into `args`, constrain it so that it cannot become an option of the
  program. [`tools-test/dns_targets_up.yaml`](../tools-test/dns_targets_up.yaml) passes a DNS name to `dig` and allows
  host names only:

  ```text
  tool dns_targets_up failed at stage arguments: validating root: validating /properties/name: pattern: "-f/etc/passwd" does not match regular expression "^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\\.?$"
  ```

### Reading an HTTP API

"What is Alertmanager paging about?" The `rest` worker calls one endpoint under the instance's base URL.

```yaml
# Active alerts from the Alertmanager API v2 (rest worker), optionally by
# severity. Needs a rest instance named "alertmanager" whose url ends in /api/v2.
name: alerts_active
description: |
  Alerts that are active in Alertmanager now (not silenced or inhibited),
  optionally only those of one `severity`. Returns per alert its name,
  severity, instance, summary and since when it fires, oldest first. An empty
  list means nothing is firing at that severity. Use it to see what the
  on-call people are being paged about.
worker: alertmanager

params:
  severity: {type: string, default: "", enum: ["", critical, warning, info], description: "Only alerts of this severity; empty = all"}

request:
  path: /alerts
  query:
    active: true
    silenced: false
    inhibited: false
    filter: '{{ if .severity }}severity="{{ .severity }}"{{ end }}'   # not sent when empty

response:
  jq: |
    [ .[] | {alert: .labels.alertname, severity: (.labels.severity // null), instance: (.labels.instance // null),
             summary: (.annotations.summary // null), since: .startsAt} ]
    | sort_by(.since)
  render: |
    {{- if not . }}No active alerts{{ with param "severity" }} with severity {{ . }}{{ end }}.{{ else -}}
    {{ len . }} active alert(s){{ with param "severity" }} with severity {{ . }}{{ end }}:
    {{ range . }}- {{ .alert }} [{{ .severity | default "none" }}]{{ with .instance }} on {{ . }}{{ end }} since {{ .since }}{{ with .summary }}: {{ . }}{{ end }}
    {{ end }}{{ end }}
```

```text
3 active alert(s):
- HostOutOfDiskSpace [warning] on web-2:9100 since 2026-03-12T07:55:00Z: Filesystem /var on web-2 is 92% full
- HostHighCpuSteal [warning] on web-1:9100 since 2026-03-12T09:34:00Z: CPU steal above 10% on web-1
- PostgresReplicationLag [critical] on db-1:9187 since 2026-03-12T10:02:00Z: Replica is 5 minutes behind
```

- The tool gives only a `path`; the base URL and the credentials stay in the config:

  ```yaml
  workers:
    alertmanager:
      type: rest
      url: http://alertmanager.monitoring:9093/api/v2
      methods: [GET]
  ```

- Query parameters go into `query`; a value that renders empty (`filter` without a severity) is not sent.
- `enum` limits `severity` to known values, so nothing else can reach the filter expression.

### An action

"Silence that alert for two hours." A `POST` changes something every time the tool is called, so it gets more care.

```yaml
# Creates an Alertmanager silence for one alert name (an action). Needs a rest
# instance named "alertmanager_rw" that allows POST.
name: alert_silence
description: |
  This is an action: creates a silence in Alertmanager for alerts named
  `alertname` (optionally only on one `instance`) for `duration`, starting
  now, with `comment` as the reason. Returns the id of the new silence and
  the HTTP status. Use it only when a person asked to silence that alert;
  the silence mutes notifications for everyone.
worker: alertmanager_rw

params:
  alertname: {type: string, required: true, description: "Alert name to silence, e.g. HostOutOfDiskSpace", pattern: "^[A-Za-z_][A-Za-z0-9_]*$"}
  instance:  {type: string, default: "", description: "Only this instance label value, e.g. web-2:9100; empty = every instance", pattern: "^[A-Za-z0-9._:-]*$"}
  duration:  {type: string, default: "2h", description: "How long, e.g. 30m, 2h or 1h30m", pattern: "^([0-9]+h|[0-9]+m|[0-9]+h[0-9]+m)$"}
  comment:   {type: string, required: true, description: "Why the alert is silenced"}
  created_by: {type: string, default: "ocellusai-mcp", description: "Author recorded in the silence"}

request:
  method: POST
  path: /silences
  body: |
    {
      "matchers": [
        {"name": "alertname", "value": {{ .alertname | toJson }}, "isRegex": false, "isEqual": true}
        {{- if .instance }},
        {"name": "instance", "value": {{ .instance | toJson }}, "isRegex": false, "isEqual": true}{{ end }}
      ],
      "startsAt": {{ now | date "2006-01-02T15:04:05Z07:00" | toJson }},
      "endsAt": {{ now | dateModify (printf "+%s" .duration) | date "2006-01-02T15:04:05Z07:00" | toJson }},
      "createdBy": {{ .created_by | toJson }},
      "comment": {{ .comment | toJson }}
    }

response:
  jq: '{status: .status, silence_id: (.body.silenceID // null)}'
  render: 'Silence {{ .silence_id }} created for {{ param "alertname" }}{{ with param "instance" }} on {{ . }}{{ end }} for {{ param "duration" }} (HTTP {{ .status }}).'
```

```text
Silence 5c1d2e3f-0a9b-4c7d-8e6f-1a2b3c4d5e6f created for HostOutOfDiskSpace on web-2:9100 for 2h (HTTP 200).
```

- The description starts with **"This is an action:"** and says what changes.
- The tool uses a separate instance, `alertmanager_rw`, that allows `POST`; read-only tools keep using an instance
  with `methods: [GET]`.
- The inputs that decide what is silenced are `required` and tightly constrained; there is no "silence everything"
  default.
- The string body puts every value through `toJson`, which quotes and escapes it.
- Actions return `{status, headers, body}` by default; the answer shows the status and the new id.
- Test an action only against a test system, or with the explicit agreement of whoever owns the target.

### More techniques in the catalog

| Technique | See |
|---|---|
| Two sources in one tool: dig resolves a name, Prometheus says which of its addresses are scraped and up | [`tools-test/dns_targets_up.yaml`](../tools-test/dns_targets_up.yaml) |
| Many unrelated signals in one instant query (`label_replace` + `or`), split into sections in jq | [`tools/estate_overview.yaml`](../tools/estate_overview.yaml) |
| Finding a VM by host name or address, with or without domain and port | [`tools/vm_metrics.yaml`](../tools/vm_metrics.yaml) |
| Peak, average and time above a threshold over a window with subqueries | [`tools/vm_steal_time.yaml`](../tools/vm_steal_time.yaml) |
| The query chosen by an `enum` parameter | [`tools/node_signal_window.yaml`](../tools/node_signal_window.yaml) |
| Comparison with the same window a week ago | [`tools/pg_slowdown_check.yaml`](../tools/pg_slowdown_check.yaml) |
| Joint anomalies of three metrics with per-metric thresholds | [`tools/node_disk_pressure_mv.yaml`](../tools/node_disk_pressure_mv.yaml) |
| Folding segments of many VMs into incidents | [`tools/node_cluster_events.yaml`](../tools/node_cluster_events.yaml) |
| Summarising a forecast for an interval, durations parsed in jq | [`tools/node_forecast.yaml`](../tools/node_forecast.yaml) |
| Outliers among pods grouped by cluster and namespace | [`tools/pod_memory_outliers.yaml`](../tools/pod_memory_outliers.yaml) |

## Writing the description

The description is the most important text in the file: it is the only thing the model reads before deciding
whether to call the tool and with which arguments. Write it for a model.

Include:

1. **What it returns and about which objects**, in the first sentence.
2. **The source and its requirements**: the exporter, the command, the API.
3. **Non-obvious parameter meaning**: which parameters are regexes, the units of thresholds, what `0` or an empty
   value means.
4. **The shape of the result and its units**: the key fields, percent or bytes or seconds, the order, the limits.
5. **What an empty answer means**: "an empty list means every pod is ready", not silence.
6. **When to use it**, and how it differs from neighbouring tools.
7. **Limits and cost**: "for windows longer than a day raise `step`".
8. **Data quirks that affect conclusions**: "busy % includes iowait and steal".
9. For actions: **"This is an action: …"** and what it changes.

Leave out the PromQL, the implementation and the parameter schema (the agent sees the schema anyway).

Too thin:

```yaml
description: Steal time metrics
```

Useful:

```yaml
description: |
  Virtual machines that had CPU steal time (CPU time taken by the hypervisor
  for other guests) within the last `window`. Based on node_exporter
  (node_cpu_seconds_total{mode="steal"}), steal is a percentage of the VM's
  total CPU time, averaged across its vCPUs.
  Returns only VMs whose peak steal exceeded `threshold` percent, sorted by
  peak, highest first: instance, node name, peak %, average % over the whole
  window and roughly how many minutes steal stayed above the threshold.
  The kernel counts steal in 10ms ticks, so a single tick already shows up as
  a few thousandths of a percent; keep `threshold` well above 0 unless you
  really need every VM with any steal at all.
  Use it to find noisy-neighbour / overcommitted hosts. For windows longer
  than a day raise `step` (e.g. 5m) to keep the query cheap.
```

When agents misuse a tool (wrong tool for the question, wrong arguments), the fix is almost always in the
description.

## Designing parameters

- **Every parameter has a `description` with an example value.** It is the model's only hint about what to pass.
- **Defaults give a useful answer.** An agent often calls with no arguments. A threshold of `0` that returns noise
  is a bad default; the smallest change a person cares about is a good one.
- **`required` only when the request is impossible without it** (the name `dig` resolves, the alert to silence).
- **Constrain everything that reaches a request**: `pattern` for strings, `minimum`/`maximum` for numbers, `enum` for
  choices. Patterns are not anchored by JSON Schema: always write `^…$`.
- **Natural units for the caller**: milliseconds, MiB, percentage points. Convert in the template (`divf .min_ms
  1000`, `mulf .min_mib 1048576`).
- **The same names across the catalog**: `instance`, `at`, `range`, `step`, `window`, `top`, `min_*`. Agents learn
  them once.

Patterns that come up again and again:

| Value | `pattern` |
|---|---|
| Prometheus duration | `^[0-9]+[smhdwy]$` |
| Compound duration (`1h30m`, `3d12h`) | `^([0-9]+[smhdwy])+$` |
| `at`: now, now-<duration>, RFC 3339 | `^(now(-[0-9]+[smhdwy])?\|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z\|[+-][0-9]{2}:[0-9]{2}))$` |
| Kubernetes name or namespace | `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` |
| Host name | `^[A-Za-z0-9][A-Za-z0-9._-]*$` |
| Kubernetes label selector, empty allowed, no leading `-` | `^([A-Za-z0-9._/][A-Za-z0-9._/=!,() -]*)?$` |
| Command argument that must not become an option | `^[^-].*$` (or stricter) |
| Numeric id | `^[0-9]+$` |

An optional regex filter defaults to `.*` and goes into PromQL as `instance=~{{ promLabel .instance }}`.

## Designing the answer

There are three levels:

| Level | `response` | The agent gets | Choose when |
|---|---|---|---|
| 0 | none | the worker's JSON | debugging, or the answer is already tiny |
| 1 | `jq` | the jq result as JSON | the calling program processes the data further |
| 2 | `jq` + `render` | a text, with the jq result as `structuredContent` | almost always: reports, analyses, summaries |

Most clients show the model only the text, so at level 2 **everything that matters goes into the text**. Good
answers:

- **Lead with a summary**: how many objects were checked, how many matched, how many were skipped and why.
- **Name the context**: the moment or window, the thresholds, the units. "No VMs with steal above 1% in the last
  1h" is an answer; "No results" is not.
- **Handle the empty case first**, as a sentence that names what was looked for.
- **Cap and sort lists**, and say when a cap hid something ("Top 2 of 3").
- **Keep the shape stable** in jq: every key present, `null` when unknown, lists wrapped in `[…]`.
- **Don't round in jq**: `structuredContent` stays exact; `render` formats.
- **One line per object**, indented lines for its findings.
- **Show doubt**: skipped objects, unstable forecasts, findings below a threshold. An agent that knows the limits
  of an answer draws better conclusions.

The details of jq and `render` are in [Templates and jq](templates-and-jq.md#the-answer-text).

## Thresholds for detectors

Every detector (`anomaly`, `outliers`, `anomaly_mv`, `anomaly_ensemble`) measures change in units of the object's
own variability. On a nearly constant object that variability is close to zero, and a meaningless wiggle becomes a
huge "anomaly". So every tool with a detector has a **noise floor in the metric's own units**:

1. **It is a change from the expected value, not a level.** "At least 50 ms slower than usual", not "latency above
   100 ms". A level threshold passes noise on busy objects and hides real jumps on quiet ones.
2. **It goes into the processor's `with`** as `min_delta` (and `min_rel_delta` where levels differ by orders of
   magnitude), not into a filter in `response.jq`. It then applies before `max_anomalies`, so the cap is spent on
   significant findings.
3. **The caller sees it in natural units** and the template converts: `min_delta: "{{ divf .min_ms 1000 }}"`.
4. **Several metrics get one value each**: `min_delta: {cpu: 5, mem: 3}`.
5. **The answer says how much was ignored**: count `below_min_delta` and say "87 smaller ignored".

Good starting values: CPU 5 percentage points, memory 3, iowait and steal 1, disk utilisation 10, network 5 Mbit/s,
load per core 0.25, TCP connections 20, latency 50 ms, Kafka lag 1000 messages, pod memory 100 MiB. Check on your
data: call once with `0` and once with your default; the difference is the noise you removed. How each processor
applies the floor is in [Processors](processors.md#the-noise-floor-min_delta).

## Safety

The agent controls the arguments of the tools it calls. A tool must make sure that no argument can do more than
intended.

- **PromQL**: label values through `promLabel`, a literal inside your own regex through `promRegex`, durations
  through `promDuration`, numbers from `integer`/`number` parameters. Never `"{{ .x }}"` inside a query.
- **Commands**: one value per argument, a leading `-` forbidden by `pattern`, read-only commands. Never put a shell
  or an interpreter in an allowlist.
- **HTTP APIs**: values inside `path` through `pathEscape` and constrained by `pattern`; query parameters in `query`;
  string bodies built with `toJson`.
- **Actions**: "This is an action" in the description, `required` and tightly constrained inputs, a separate instance
  that allows only the methods needed, no test calls against systems you don't own.
- **Secrets** stay in the config. A tool never contains a token, and its templates don't read environment variables.

More in the [security model](user-guide.md#security-model).

## Validate and test

### Validate

```bash
./bin/ocellusai-mcp -config config.yaml -validate
```

`-validate` checks the YAML, the fields, the parameter schemas, the request keys, the processor settings, and the
syntax of templates and jq. It contacts nothing. It does **not** check field names inside templates, PromQL, the
command's output or real data: only a call does.

To validate a new file without touching the catalog in use, validate a copy of the config that points at a directory
holding only that file (the config must still define the worker instances the tool uses):

```bash
mkdir -p /tmp/check/tools && cp tools/vm_cpu_top.yaml /tmp/check/tools/
```

```bash
sed 's#^tools_dir:.*#tools_dir: /tmp/check/tools#' config.yaml > /tmp/check/config.yaml
```

```bash
./bin/ocellusai-mcp -config /tmp/check/config.yaml -validate
```

### Call

[`scripts/mcp-call.py`](../scripts/mcp-call.py) starts the server over stdio, so it needs a config with
`server.transport: stdio`:

```bash
sed 's/transport: http/transport: stdio/' /tmp/check/config.yaml > /tmp/check/stdio.yaml
```

```bash
python3 scripts/mcp-call.py -quiet -config /tmp/check/stdio.yaml call vm_cpu_top '{"top": 3}' --structured
```

Calls worth making for every tool:

- with the defaults only;
- with a filter that matches nothing: a clear sentence, not an error;
- with a bad argument (a wrong duration, an unknown key): an error at stage `arguments`;
- on an object that lacks some of the metrics: `n/a`, not a `render` error;
- with the largest realistic window on the real fleet, timed (the Prometheus timeout is 15 s by default);
- for detectors, `min_delta: 0` against the default;
- for `calls`, a first call that returns nothing: the second request must not widen to "everything".

### Debug

- Set `log.level: debug` and call again: the log line `request rendered` shows the final PromQL, command line or
  HTTP request. Run that PromQL in the Prometheus UI.
- Save a worker's answer and try jq on it with the `jq` command-line tool
  ([how](templates-and-jq.md#try-jq-on-real-data)).
- `--structured` shows the jq result next to the text, so you can tell a jq problem from a `render` problem.

## Common mistakes

| Symptom | Cause | Fix |
|---|---|---|
| `worker: not allowed together with calls` | a top-level `worker` in a tool with `calls` | remove it; each call names its worker |
| A YAML error with a line number | an unquoted value starting with `{{` | `step: "{{ .step }}"` |
| `map has no entry for key "reason"` at stage `render` | `{{ if .reason }}` on an object that may not have the key | `{{ if hasKey . "reason" }}`, or make jq always produce the key |
| `%!f(int=25)` in the text | `printf "%.1f"` of an integer from jq | `printf "%.1f" (float64 .x)` |
| `incompatible types for comparison` | `eq` of an integer and a float | `eq (int .total) 0` |
| `<no value>` in a query or text | an optional parameter without a default | give it a default or test it with `if` |
| The tool loads but returns nothing | a wrong metric or label name, or a `job` that does not exist | run the rendered query in the Prometheus UI |
| A filter matches more than expected | the pattern is not anchored | `^…$` |
| `expected resultType "matrix"` at stage `process` | an `instant` query feeding a time-series processor | `type: range` with `start`, `end`, `step` |
| `input mem: result[0] and result[1] share the join key …` at stage `process` | two series per object in one input, often a label that changed inside the window | aggregate in PromQL: `max by (instance) (…)` |
| `join` drops most samples | calls with different `start`, `end` or `step` | the same three values in every call |
| A detector reports hundreds of tiny anomalies | no noise floor | `min_delta` in the metric's units |
| `cannot iterate over string` at stage `jq` | a command printed text, not JSON | check the command's flags (`-o json`) or use `parse: lines` |

## Deploy

The server reads the catalog only at startup.

- **Locally**: put the file in `tools_dir` and restart the server (or the MCP client, for stdio).
- **Kubernetes**: rebuild the catalog ConfigMap from the directory and restart the Deployment:

  ```bash
  kubectl -n monitoring create configmap ocellusai-mcp-tools --from-file=tools/ --dry-run=client -o yaml | kubectl apply -f -
  ```

  ```bash
  kubectl -n monitoring rollout restart deployment/ocellusai-mcp
  ```

- **A new worker instance** (a second Prometheus, a new API) is a config change: add it before the tools that use
  it, or the server will not start.
- **Keep names stable.** Agents, saved prompts and other people's notes refer to tools by name; a rename breaks them.
  Add a new tool rather than changing the meaning of an existing one.
- **Keep the catalog in version control** and review changes like code: a tool file decides which commands run and
  which APIs are called.

## Checklist

**Design**

- [ ] The tool answers one clear question; there is no existing tool that answers it.
- [ ] `name` is `<object>_<what>`, the file has the same name, a header comment says what the tool assumes.
- [ ] The description says what is returned, from where, in which units, what an empty answer means and when to use
      the tool; actions start with "This is an action".

**Parameters and request**

- [ ] Every parameter has a description with an example and a useful default (or is `required` for a reason).
- [ ] Everything that reaches a request is constrained by an anchored `pattern`, an `enum` or bounds.
- [ ] Values reach PromQL through `promLabel`, `promRegex`, `promDuration`; REST paths through `pathEscape`; command
      arguments one per element, with no leading `-`.
- [ ] Window-based tools take `at`; range calls of one tool share `start`, `end`, `step`.
- [ ] Detectors have `min_delta` in the metric's units, exposed in natural units.

**Answer**

- [ ] jq wraps results in `[…]` or `{…}`, keeps every key, uses `tonumber? // null`, sorts and caps, does not round.
- [ ] `render` handles the empty case first, converts numbers with `float64`, prints `n/a` for nil, and names the
      window, thresholds, units and counts.

**Verification**

- [ ] `-validate` passes.
- [ ] Calls with the defaults, with an empty result and with a bad argument behave as expected.
- [ ] The largest realistic call finishes well within the timeout.
- [ ] No temporary test values (a fixed `time`, a narrowed filter) are left in the file.

## Let an AI agent write the tool

The repository includes an agent skill (a `SKILL.md` with reference files, the format Claude Code and other agents load) for this workflow:
[`skills/ocellus-tool-builder`](../skills/ocellus-tool-builder). With it, an AI coding agent interviews you about
the question, the source, the analysis and the answer, and writes the YAML from the closest template. It then offers
to validate the file with `-validate` and to test-call it. It builds, installs, downloads and runs nothing, and
queries none of your systems, without your explicit permission; where there is no ocellusai-mcp checkout or binary, it
gives you the commands to run yourself. Its reference files are a condensed version of this guide.

To use it with Claude Code, copy the folder into your personal or project skills directory:

```bash
cp -r skills/ocellus-tool-builder ~/.claude/skills/
```

Then ask in your own words, for example: "Make an ocellus tool that shows which Kafka consumer groups are falling
behind." Review the spec it proposes before it writes the file, and the file before it reaches production, as you
would review a colleague's.
