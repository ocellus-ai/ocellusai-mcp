# Several worker calls: calls

Instead of one `worker` + `request`, a tool can declare `calls`: named calls executed **one after
another**, possibly on different workers. Their results form an object `{name: result}` that goes into
`process` and `response` like an ordinary worker answer.

## When to use it

- Values for the second request are only known after the first: addresses, instance list, ids.
- The answer joins two systems in one table (the addresses behind a DNS name from dig + CPU from
  Prometheus).
- Data from different instances of one type (`prom_prod` and `prom_staging`).
- Several range queries that `join` stitches into one multivariate series (anomaly_mv, ensemble,
  ssa_mv): one call per signal.

When not to: if everything is in one Prometheus, one query with `label_replace` + `or` is cheaper
([prometheus.md](prometheus.md#several-metrics-in-one-query)). If a second call is needed *for each
element* of the first (describe every host), that is a scenario, not a tool: give the agent two tools.
There are no loops, parallel calls or conditional skipping.

## Syntax

```yaml
calls:
  - name: addrs                     # result key; ^[A-Za-z_][A-Za-z0-9_]*$, unique
    worker: shell                   # an instance from the config
    request:                        # as for a single-call tool of that worker
      command: dig
      args: [+short, A, "{{ .name }}"]
      parse: lines
    jq: '[.[] | select(test("^[0-9]+(\\.[0-9]+){3}$"))] | unique'   # optional; its output is stored
  - name: cpu
    worker: prometheus
    request:
      type: instant
      query: |
        … instance=~{{ … result "addrs" … }} …
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
| `response.jq`, `calls[i].jq` | no `result`: `response.jq` already gets `{addrs: …, cpu: …}`, a per-call jq sees only its own answer |

A literal reference to a call that does not exist or has not run yet is a **startup** error:
`calls[0].request: result "cpu": no call result is available here`. A dynamic name (`result .x`) is
checked at call time.

## The first call returns a flat shape

text/template is bad at digging in raw command output or nested JSON. Rule: **the first call's `jq`
returns a flat shape** (a list of names or of flat objects) and the second call's template only
assembles a string from it. This shape also reaches `response.jq`, so keep everything the final join
needs, not just the key. Here it also drops what is not an address: `dig +short` prints the CNAME
chain too.

The idiom "list of names → regex for PromQL":

```
{{- $addrs := list }}{{ range result "addrs" }}{{ $addrs = append $addrs (promRegex .) }}{{ end -}}
… instance=~{{ promLabel (printf "(%s)(:[0-9]+)?" (join "|" $addrs | default "__none__")) }} …
```

- `promRegex` escapes each name (the dots of an address), `promLabel` quotes the whole regex for
  PromQL; PromQL anchors it, so `(:[0-9]+)?` admits the port of `instance`;
- `default "__none__"` protects against an empty list: `instance=~"()(:[0-9]+)?"` would match series
  **without** an `instance` label, `__none__` matches nothing;
- `{{-` and `-}}` keep the query from starting with an empty line.

With hundreds of names the regex gets long (Prometheus copes, the query goes by POST), but narrow the
first call (a more specific name, a filter, a limit parameter).

## Joining results in response.jq

```jq
(.cpu.result | map({key: (.metric.instance | sub(":[0-9]+$"; "")), value: (.value[1] | tonumber? // null)}) | from_entries) as $cpu
| [.addrs[] | {address: ., cpu_pct: $cpu[.]}]
| sort_by(-(.cpu_pct // -1))
```

`$cpu[.]` is `null` for an address without samples, so it stays in the list; `render` prints it as
"no samples". Lead with the source that defines which rows exist (here dig: an address behind the name
is worth showing even if nothing monitors it).

Without `response`, the agent gets the whole `{addrs: …, cpu: …}` object: fine for debugging, too much
for work.

## description

Name **both** sources and their requirements ("Resolves the name with dig, then queries Prometheus
for exactly those addresses…"). If one source is missing in an environment, the agent must understand why
the tool fails and what to use instead.

## Errors

At startup:

| Message | Cause |
|---|---|
| `calls must be a list of calls ({name: …, worker: …, request: {...}, jq: …})` | `calls` written as a map |
| `calls[0].name: required (it is the key the result is stored under)` | no name |
| `calls[1].name: duplicate call name "addrs" (also calls[0])` | duplicate name |
| `calls[1].worker: unknown worker "loki" (configured: prometheus, shell)` | no such instance |
| `calls[1] (cpu): request.time: only valid for type: instant` | a worker error in that call |
| `calls[0].jq: jq parse: …` | per-call jq syntax |
| `calls[0].request: result "cpu": no call result is available here` | reference to a later call |
| `worker: not allowed together with calls (set calls[i].worker instead)` | both forms at once |

At call time the stages are the usual ones (`request`, `worker`, `jq`) and the text names the call:
`tool dns_hosts_cpu failed at stage worker: calls[1] (cpu): prometheus bad_data error: …`. `-validate`
shows the chain as `calls=addrs>cpu`.

Test: the first call returns an empty list (the second request must not become "everything"), the
first call fails (the agent sees `calls[0] (…)`), the total time of both calls.
