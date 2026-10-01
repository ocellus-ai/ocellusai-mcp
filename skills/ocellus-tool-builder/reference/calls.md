# Several worker calls: calls

Instead of one `worker` + `request`, a tool can declare `calls`: named calls executed **one after
another**, possibly on different workers. Their results form an object `{name: result}` that goes into
`process` and `response` like an ordinary worker answer.

## When to use it

- Values for the second request are only known after the first: pod names, instance list, ids.
- The answer joins two systems in one table (pod status from kubectl + CPU from Prometheus).
- Data from different instances of one type (`prom_prod` and `prom_staging`).
- Several range queries that `join` stitches into one multivariate series (anomaly_mv, ensemble,
  ssa_mv): one call per signal.

When not to: if everything is in one Prometheus, one query with `label_replace` + `or` is cheaper
([prometheus.md](prometheus.md#several-metrics-in-one-query)). If a second call is needed *for each
element* of the first (describe every pod), that is a scenario, not a tool: give the agent two tools.
There are no loops, parallel calls or conditional skipping.

## Syntax

```yaml
calls:
  - name: pods                      # result key; ^[A-Za-z_][A-Za-z0-9_]*$, unique
    worker: shell                   # an instance from the config
    request:                        # as for a single-call tool of that worker
      command: kubectl
      args: [get, pods, -n, "{{ .namespace }}", -o, json]
    jq: '[.items[] | {pod: .metadata.name, phase: .status.phase}]'   # optional; its output is stored
  - name: cpu
    worker: prometheus
    request:
      type: instant
      query: |
        … pod=~{{ … result "pods" … }} …
```

| Field | Required | Purpose |
|---|---|---|
| `name` | yes | the key in `process`/`response` input and in `result "name"` |
| `worker` | yes | instance; calls may use different instances and types |
| `request` | yes | the same keys and checks as for that worker |
| `jq` | no | gojq over **this** call's answer; `$params` available; its output is stored instead of the raw answer |

Rules:

- `calls` and top-level `worker`/`request` are mutually exclusive.
- Calls run strictly in order; the first failure stops the tool and later calls don't run.
- Total time is the sum of the calls; each worker has its own timeout.
- Every referenced instance must be configured; for shell the command must be in *that* instance's
  allowlist.

## How templates see earlier results

`result "name"` returns a stored result (after its `jq`). In `request` the root `.` stays the params.

| Where | `result` sees |
|---|---|
| `calls[i].request` | calls **before** i |
| `process[j].with` | all calls |
| `response.render` | all calls |
| `response.jq`, `calls[i].jq` | no `result`: `response.jq` already gets `{pods: …, cpu: …}`, a per-call jq sees only its own answer |

A literal reference to a call that does not exist or has not run yet is a **startup** error:
`calls[0].request: result "cpu": no call result is available here`. A dynamic name (`result .x`) is
checked at call time.

## The first call returns a flat shape

text/template is bad at digging in raw kubectl JSON. Rule: **the first call's `jq` returns a flat
shape** (a list of names or of flat objects) and the second call's template only assembles a string
from it. This shape also reaches `response.jq`, so keep everything the final join needs (`phase`,
`restarts`), not just the key.

The idiom "list of names → regex for PromQL":

```
{{- $names := list }}{{ range result "pods" }}{{ $names = append $names (promRegex .pod) }}{{ end -}}
… pod=~{{ promLabel (join "|" $names | default "__none__") }} …
```

- `promRegex` escapes each name, `promLabel` quotes the whole regex for PromQL;
- `default "__none__"` protects against an empty list: `pod=~""` would match series **without** a
  `pod` label, `__none__` matches nothing;
- `{{-` and `-}}` keep the query from starting with an empty line.

With hundreds of names the regex gets long (Prometheus copes, the query goes by POST), but narrow the
first call (`-l`, `--field-selector`, a limit parameter).

## Joining results in response.jq

```jq
(.cpu.result | map({key: .metric.pod, value: (.value[1] | tonumber? // null)}) | from_entries) as $cpu
| [.pods[] | . + {cores: ($cpu[.pod] // 0)}]
| sort_by(-.cores)
```

`$cpu[.pod] // 0` keeps a pod without samples in the list; use `// null` and `n/a` in `render` if "no
data" is more honest. Lead with the source that defines which rows exist (here kubectl: the pod exists
even before it has metrics).

Without `response`, the agent gets the whole `{pods: …, cpu: …}` object: fine for debugging, too much
for work.

## description

Name **both** sources and their requirements ("Lists the pods with kubectl, then queries Prometheus
for exactly those pods…"). If one source is missing in an environment, the agent must understand why
the tool fails and what to use instead.

## Errors

At startup:

| Message | Cause |
|---|---|
| `calls must be a list of calls ({name: …, worker: …, request: {...}, jq: …})` | `calls` written as a map |
| `calls[0].name: required (it is the key the result is stored under)` | no name |
| `calls[1].name: duplicate call name "pods" (also calls[0])` | duplicate name |
| `calls[1].worker: unknown worker "loki" (configured: prometheus, shell)` | no such instance |
| `calls[1] (cpu): request.time: only valid for type: instant` | a worker error in that call |
| `calls[0].jq: jq parse: …` | per-call jq syntax |
| `calls[0].request: result "cpu": no call result is available here` | reference to a later call |
| `worker: not allowed together with calls (set calls[i].worker instead)` | both forms at once |

At call time the stages are the usual ones (`request`, `worker`, `jq`) and the text names the call:
`tool pods_cpu_status failed at stage worker: calls[1] (cpu): prometheus bad_data error: …`. `-validate`
shows the chain as `calls=pods>cpu`.

Test: the first call returns an empty list (the second request must not become "everything"), the
first call fails (the agent sees `calls[0] (…)`), the total time of both calls.
