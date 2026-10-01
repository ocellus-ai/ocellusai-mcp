# Worker prometheus

## Contents

- [Request keys](#request-keys)
- [Time formats](#time-formats)
- [The `at` parameter](#the-at-parameter)
- [What jq receives](#what-jq-receives)
- [Worker errors](#worker-errors)
- [Discovering metrics and labels](#discovering-metrics-and-labels)
- [Recipes](#recipes)

## Request keys

| Key | Query type | Meaning |
|---|---|---|
| `type` | — | `instant` (default) or `range` |
| `query` | both | PromQL, required |
| `time` | `instant` only | evaluation time; default now |
| `start` | `range` only | default `now-1h` |
| `end` | `range` only | default `now`; must be after `start` |
| `step` | `range` only | default `60s`; `30s`, `1m` or plain seconds `15` |

Mixing keys (`time` in a range query, `step` in an instant one) is a startup error. Every key can be a
template. The timeout is the instance's `timeout` (15 s by default) and cannot be set per tool. Long
queries are fine: the client sends them by POST.

```yaml
request:
  type: range
  query: sum by (pod) (rate(container_cpu_usage_seconds_total{namespace={{ promLabel .namespace }}}[5m]))
  start: "now-{{ promDuration .range }}"
  end: now
  step: "{{ .step }}"
```

## Time formats

`time`, `start`, `end` accept: empty (the default), `now`, `now-5m`, `now+1h` (Prometheus durations),
RFC3339 (`2026-09-12T10:00:00Z`) and unix seconds (`1757600000`).

To look at the past, evaluate the whole query at that moment (`time: now-1d`) instead of adding
`offset 1d` to selectors: with `offset`, `time() - node_boot_time_seconds offset 1d` would report an
uptime one day too long.

## The `at` parameter

Recommended for every tool that looks at a window: the caller can examine a past incident exactly
like the present.

```yaml
params:
  at: {type: string, default: "now", description: "End of the window: now, now-<duration> such as now-3d, or an RFC3339 time", pattern: "^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$"}
  range: {type: string, default: "6h", description: "Look-back range ending at `at`, e.g. 2h, 1d", pattern: "^[0-9]+[smhdwy]$"}
```

Instant query:

```yaml
request:
  type: instant
  time: "{{ .at }}"
```

Range query ending at `at` (`start = at − range`; `now` and `now-<d>` stay relative, RFC3339 is
shifted with sprig date functions). Copy this line verbatim:

```yaml
request:
  type: range
  start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
  end: "{{ .at }}"
  step: "{{ .step }}"
```

In a tool with several range calls, repeat the same `start`/`end`/`step` in every call: `join`
matches samples by exact timestamp.

## What jq receives

The answer is normalized to the Prometheus HTTP API format. **Values are strings**, timestamps are
float seconds. `__name__` is present on raw selectors and disappears after functions and arithmetic.

Instant, vector:

```json
{"resultType": "vector",
 "result": [{"metric": {"instance": "a:9100", "job": "node"}, "value": [1757600000, "0.25"]}]}
```

```jq
[.result[] | {instance: .metric.instance, v: (.value[1] | tonumber? // null)}]
```

Range, matrix:

```json
{"resultType": "matrix",
 "result": [{"metric": {"pod": "api-1"}, "values": [[1757600000, "0.2"], [1757600060, "0.3"]]}]}
```

```jq
[.result[] | {pod: .metric.pod, avg: ([.values[][1] | tonumber] | add / length)}]
```

Scalar: `{"resultType": "scalar", "result": [1757600000, "42"]}`.

Values may be `"NaN"`, `"+Inf"`, `"-Inf"`: use `tonumber? // null`.

## Worker errors

- PromQL syntax and other API errors: `prometheus bad_data error: <Prometheus message>`;
- timeout: `request timed out`;
- Prometheus warnings don't fail the call, they go to the server log.

## Discovering metrics and labels

Guessed metric or label names make a tool that loads and returns nothing. **Ask the user before
querying their Prometheus**, then use read-only HTTP API calls against the instance URL from the
config (add the instance's auth header from the environment if it has one; never print it):

```bash
P=http://prometheus:9090
# scrape jobs and how many targets each has
curl -s "$P/api/v1/query" --data-urlencode 'query=count by (job) (up)'
# metric names matching a prefix
curl -s -G "$P/api/v1/label/__name__/values" --data-urlencode 'match[]={__name__=~"kafka_consumergroup.*"}'
# the labels of a metric, a few series only
curl -s "$P/api/v1/query" --data-urlencode 'query=topk(3, kafka_consumergroup_lag)'
# HELP, TYPE and unit
curl -s -G "$P/api/v1/metadata" --data-urlencode 'metric=kafka_consumergroup_lag'
# the final query of the tool, at the moment you test
curl -s "$P/api/v1/query" --data-urlencode 'query=<rendered PromQL>'
```

Check: the metric exists, the labels you aggregate or filter by exist on it, a counter is wrapped in
`rate`/`increase`, the unit (seconds vs milliseconds, bytes vs bits), and how many series the query
returns on the real fleet. Without network access, ask the user to paste a sample series from the
Prometheus UI.

## Recipes

### Several metrics in one query

A tool has one `request`, but often needs CPU, memory and disks at once. Tag every part with a
synthetic label using `label_replace` and combine them with `or`:

```yaml
name: node_cpu_mem
description: |
  Current CPU and memory usage per node_exporter instance.
  Returns [{instance, cpu_pct, mem_pct}], busiest CPU first; a field is null
  when the instance does not export the metric.
worker: prometheus
params:
  window: {type: string, default: "5m", description: "CPU rate window such as 5m", pattern: "^[0-9]+[smhdwy]$"}
request:
  type: instant
  query: |
    label_replace(
      100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[{{ promDuration .window }}]))),
      "stat", "cpu_pct", "", "")
    or label_replace(
      100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes),
      "stat", "mem_pct", "", "")
response:
  jq: |
    [ .result
      | group_by(.metric.instance)[]
      | (map({key: .metric.stat, value: (.value[1] | tonumber? // null)}) | from_entries) as $s
      | {instance: .[0].metric.instance, cpu_pct: $s.cpu_pct, mem_pct: $s.mem_pct}
    ]
    | sort_by(-(.cpu_pct // 0))
```

- `label_replace(v, "stat", "cpu_pct", "", "")`: the empty regex matches the empty value of a missing
  source label, so `stat` is simply added to every series.
- `a or b` adds series of `b` only when that label set is not in `a` yet; a unique `stat` per part
  guarantees nothing is lost.
- Call the label **`stat`**, not `metric` (labels live under `.metric` in the answer, `.metric.metric`
  is confusing), and never a real label name (`job`, `instance`, `device`, `mode`).
- jq groups per object and folds into an object with `from_entries`; `$s.cpu_pct` on a missing key is
  `null`, so every field is always present.
- Parts with an extra dimension (`device`, `mountpoint`) are grouped a second time by that label.

### Filtering by object

If the label value is known, filter **in the selector**: `node_load1{instance=~{{ promLabel .instance }}}`.

If the object is found through another metric (by `nodename` from `node_uname_info`), use
`and on (instance)`:

```promql
node_load1
  and on (instance)
  (last_over_time(node_uname_info{nodename=~"(?i)db-02([.:].*)?"}[1h])
   or last_over_time(node_uname_info{instance=~"(?i)db-02([.:].*)?"}[1h]))
```

- `last_over_time(...[1h])` also finds a VM that just went down (its `up` is `0`, the tool can say
  `DOWN` instead of "not found").
- Cost: Prometheus does not push `and on` filters into selectors; every part is computed over the
  whole fleet first. Measure on the real fleet.
- To attach a label from an info metric: `* on (instance) group_left (nodename) node_uname_info`.
  If the info metric may be missing, fetch it as a separate `or` part instead, so the object does not
  vanish.

### Regular expressions

- Prometheus **anchors** label regexes: `instance=~"db-02"` does not match `db-02:9100`.
- `(?i)`: case-insensitive.
- `([.:].*)?` after a name: with or without domain and/or port; `10.0.0.1` matches `10.0.0.1:9100`
  but not `10.0.0.12:9100`.
- A caller's literal inside your own regex goes through `promRegex`.

### Thresholds, noise, NaN

- Filter in PromQL (`> {{ .threshold }}`), not in jq: less data over the wire.
- Kernel counters are discrete (CPU and steal tick in 10 ms): "zero" may be `0.0004%`. Put default
  thresholds above the noise and say so in `description`.
- Division by zero gives `NaN`: protect the denominator, `a / (b > 0)` (series with zero disappear).
- For **detectors** the threshold is not in PromQL or jq but the processor's `min_delta`
  ([processors.md](processors.md#min_delta-the-significance-threshold)).
- Several series per object where you expect one (a relabelling inside the window) break `join`:
  aggregate explicitly, e.g. `max by (instance) (…)`.

### Windows and subqueries

- A `rate` window covers at least two scrape intervals; `[5m]` is a safe choice.
- Peak over a window: `max_over_time((expr)[{{ promDuration .window }}:{{ promDuration .step }}])`.
  The `rate` window inside a subquery must be at least the step:
  `[{{ max 300 (promDurationSeconds .step) }}s]`.
- Time above a threshold, seconds: `count_over_time((expr > T)[W:S]) * S`.
- The average over the whole window is cheaper without a subquery: `rate(x[{{ promDuration .window }}])`.
- `increase()` extrapolates and returns fractions (`0.98` instead of `1`): `round` counters in jq.

Complete example of peak, average and minutes above a threshold in one instant query:

```yaml
query: |
  label_replace(
    max_over_time((avg by (instance) (rate(node_cpu_seconds_total{mode="steal"}[{{ max 300 (promDurationSeconds .step) }}s])) * 100)[{{ promDuration .window }}:{{ promDuration .step }}]) > {{ .threshold }},
    "stat", "peak", "", "")
  or label_replace(
    count_over_time((avg by (instance) (rate(node_cpu_seconds_total{mode="steal"}[{{ max 300 (promDurationSeconds .step) }}s])) * 100 > {{ .threshold }})[{{ promDuration .window }}:{{ promDuration .step }}]) * {{ promDurationSeconds .step }},
    "stat", "seconds_above", "", "")
  or label_replace(
    avg by (instance) (rate(node_cpu_seconds_total{mode="steal"}[{{ promDuration .window }}])) * 100,
    "stat", "avg", "", "")
  or label_replace(
    max by (instance, nodename) (last_over_time(node_uname_info[{{ promDuration .window }}])),
    "stat", "info", "", "")
```

with jq that keeps only objects that have `peak` (the PromQL filter removed the others):

```jq
[ .result
  | group_by(.metric.instance)[]
  | (map(select(.metric.stat != "info") | {key: .metric.stat, value: (.value[1] | tonumber)}) | from_entries) as $s
  | select($s.peak != null)
  | {instance: .[0].metric.instance,
     node: (map(select(.metric.stat == "info") | .metric.nodename) | first),
     peak_pct: $s.peak, avg_pct: ($s.avg // 0), minutes_above: (($s.seconds_above // 0) / 60)}
]
| sort_by(-.peak_pct)
```

### Response size

- Aggregate and sort in Prometheus (`sum by`, `topk`) instead of pulling raw series into jq.
- Give lists a limit parameter (`top`) or cut in jq (`.[:$params.top]`).
- A range query returns `range / step` points per series: 24 h at 1 m is 1440 points per series. Point
  anomalies need 60–500 points; forecasts need at least three periods (a week at 5 m is 2016 points).
