# Tool file format

## Contents

- [Fields](#fields)
- [YAML pitfalls](#yaml-pitfalls)
- [name](#name)
- [description](#description)
- [params](#params)
- [Templates](#templates)

## Fields

| Field | Required | Purpose | Checked at startup |
|---|---|---|---|
| `name` | yes | the tool name the agent calls | `^[a-z0-9_]+$`, unique in the catalog |
| `description` | yes | the text the model uses to decide when and how to call the tool | not empty |
| `worker` | yes, unless `calls` | a worker **instance** name from the config's `workers` (`prometheus`, `prom_staging`, …); its type (`prometheus`, `shell`, `rest`) defines the `request` keys | the instance exists |
| `params` | no | call parameters → the tool's `inputSchema` | rules below; defaults are checked against the schema |
| `request` | yes, unless `calls` | the worker request | keys and types by the worker, template syntax |
| `calls` | no; replaces `worker` + `request` | several named calls in sequence ([calls.md](calls.md)) | names, instances, each request, per-call `jq`, `result` references |
| `process` | no | list of processing steps `{fn, with}` ([processors.md](processors.md)) | `fn` exists, `with` keys by the processor, template syntax |
| `response.jq` | no | final gojq expression | compiles |
| `response.render` | no | final text template | template syntax |

Order used in the catalog: `name, description, worker, params, request, process, response`
(with calls: `name, description, params, calls, process, response`). Start the file with a `#`
comment block: what the tool does and what it assumes about the data.

```yaml
# Top VMs by memory use at a moment. Assumes node_exporter; add a job filter for your setup.
name: node_memory_top
description: |
  ...
worker: prometheus

params:
  top: {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many VMs to return"}

request:
  type: instant
  query: ...

response:
  jq: ...
  render: ...
```

## YAML pitfalls

- **Parsing is strict on every level.** An unknown field (`descripton`, `title`, `annotations`,
  `items` in a param, a stray key in a step) is a startup error. No other fields exist.
- **One bad file stops the whole server**, including a file that references a worker instance that is
  not configured.
- `worker`/`request` and `calls` are mutually exclusive.
- **Quote values that start with `{{`**: `step: "{{ .step }}"`. Unquoted, YAML reads `{` as a map.
  The same for values starting with `*`, `&`, `!`, `%`, `@`, `` ` `` or containing `: ` or ` #`.
- Multi-line values (`description`, `query`, `jq`, `render`, string `body`) use the block style `|`:
  no escaping of quotes, `{}` or `#` inside.
- Single-line jq is easiest in single quotes: `jq: '[.result[] | .metric.job]'` (a single quote inside
  is doubled: `''`).
- A `#` inside a `|` block is part of the value, not a YAML comment. PromQL accepts `#` comments, jq
  and templates do not.

## name

- Only `a-z`, `0-9`, `_`; unique (a duplicate is an error naming both files).
- Pattern `<object>_<what>`: `vm_steal_time`, `pod_restarts`, `kafka_lag_anomalies`, `targets_down`.
  No `get_`/`list_` prefixes.
- The file is `<tools_dir>/<name>.yaml`.
- The name is a contract with agents and saved prompts: don't rename existing tools casually.

## description

The most important text in the file: the model decides from it whether to call the tool and with
which arguments. It goes to the agent verbatim. Write it in English for a model, not for a human
reader of the YAML.

Include:

1. **What it returns and about which object** — first sentence.
2. **Data source and requirements**: exporter (node_exporter, kube-state-metrics), command, API.
3. **Non-obvious parameter semantics**: how names match (regex?), threshold units, what `0` means.
4. **Result shape and units**: key fields, percent/bytes/seconds, sorting, limits.
5. **Empty result** and what it means ("[] means all targets are up").
6. **When to use it** and how it differs from neighbouring tools.
7. **Limits and cost** ("for windows longer than a day raise `step`").
8. **Data quirks** that affect conclusions ("steal is counted in 10ms ticks, so…").
9. For actions: "This is an action: …" and what it changes.

Avoid: retelling the PromQL or implementation, marketing, repeating the parameter schema (the agent
sees it anyway).

Bad:

```yaml
description: Steal time metrics
```

Good:

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

## params

A map, one parameter per line in flow style. The order is kept and appears in the schema.

```yaml
params:
  namespace: {type: string, required: true, description: "Kubernetes namespace, e.g. prod", pattern: "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"}
  window:    {type: string, default: "5m", description: "Rate window such as 5m or 1h", pattern: "^[0-9]+[smhdwy]$"}
  top:       {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many pods to return"}
  mode:      {type: string, default: "both", enum: [both, up, down], description: "Which deviations to keep"}
```

Parameter name: `^[A-Za-z_][A-Za-z0-9_]*$` (it is used as `.name` in templates and `$params.name` in
jq, so no dashes).

### Parameter fields

| Field | Types | Meaning |
|---|---|---|
| `type` | — | required: `string`, `integer`, `number`, `boolean`, `array`, `object` |
| `required` | all | the caller must pass it |
| `default` | all | used when not passed |
| `description` | all | shown to the agent; include an example value |
| `enum` | all | allowed values |
| `minimum`, `maximum` | `integer`, `number` | inclusive bounds |
| `pattern` | `string` | Go RE2 regular expression |

Startup checks: `required` and `default` are mutually exclusive; `pattern` only on `string`;
`minimum`/`maximum` only on `integer`/`number`; `default` must pass its own schema; no duplicates.

At call time: arguments are validated against the schema, **extra keys are rejected**, and so are
wrong types and values outside `enum`/bounds/`pattern` (stage `arguments`). `pattern` is **not
anchored** by JSON Schema: always write `^…$`. RE2 has no lookahead and no backreferences.

### What templates and jq receive

| `type` | In templates (`.x`, `param "x"`) | In jq (`$params.x`) |
|---|---|---|
| `string` | string | string |
| `integer` | `int` | number |
| `number` | `float64` (`{{ .x }}` prints `0.5`, `1`, `1e+21`) | number |
| `boolean` | bool | bool |
| `array` | `[]any` | array |
| `object` | `map[string]any` | object |
| not passed, no default | `nil`; `{{ .x }}` prints **`<no value>`** | `null` |

Every declared parameter is always present, so `.x` on an optional parameter does not fail but
prints `<no value>`: test it with `{{ if .x }}` or give a `default`.

### Pattern library

| Value | `pattern` |
|---|---|
| Prometheus duration | `^[0-9]+[smhdwy]$` |
| Compound duration (`1h30m`) | `^([0-9]+(ms|[smhdwy]))+$` |
| `at`: now, now-<duration>, RFC3339 | `^(now(-[0-9]+[smhdwy])?|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2}))$` |
| Kubernetes name / namespace | `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` |
| Host name | `^[A-Za-z0-9][A-Za-z0-9._-]*$` |
| Shell argument without option injection | `^[^-].*$` (or a stricter one) |
| Numeric id | `^[0-9]+$` |
| Kubernetes label selector (empty allowed, no leading `-`) | `^([A-Za-z0-9._/][A-Za-z0-9._/=!,() -]*)?$` |

### Recommendations

- **Every parameter has a `description` with an example value.** It is the model's only hint.
- **Constrain everything that reaches a request**: durations by `pattern`, names by `pattern`,
  numbers by `minimum`/`maximum`, choices by `enum`.
- **Defaults are a useful working mode, not zero.** A steal threshold of `0` returned VMs with one
  10 ms tick (0.0004%); `1` fixed it.
- `required` only for what the request cannot do without.
- An optional regex filter: `default: ".*"`, used as `instance=~{{ promLabel .instance }}`.
- Give the caller natural units (ms, MiB, percentage points) and convert in the template.

## Templates

Go `text/template` with all [sprig](https://masterminds.github.io/sprig/) functions and a few
helpers.

### Where templates work and what `.` is

| Place | Root `.` | `param "x"` | `result "name"` |
|---|---|---|---|
| `request`: every string, including list items and nested maps | params | yes | no |
| `calls[i].request`: every string | params | yes | results of calls **before** i |
| `process[i].with`: every string | params | yes | all calls |
| `response.render` | the jq result (or the previous stage's output when there is no jq) | yes | all calls |

- A string **without `{{`** is not compiled and passes as is.
- A template's result is **always a string**. Numbers and booleans written in YAML without a
  template pass as numbers and booleans.
- `missingkey=error`: a missing map key is an error.
- Syntax and function names are checked at startup; **field names only at call time**:
  `{{ .namepace }}` passes `-validate` and fails on the first call (`map has no entry for key
  "namepace"`). Hence the test call.

### ocellusai-mcp helpers

| Function | Does | Example |
|---|---|---|
| `promLabel v` | escapes `\ " \n \t \r` and wraps in quotes | `namespace={{ promLabel .namespace }}` → `namespace="prod"` |
| `promRegex v` | escapes RE2 metacharacters so the value matches literally | `db.1` → `db\.1` |
| `promDuration v` | validates a Prometheus duration, returns the canonical form; invalid → error at stage `request` | `[{{ promDuration .window }}]`, `60s` → `1m` |
| `promDurationSeconds v` | the same in whole seconds | `{{ promDurationSeconds .step }}` |
| `pathEscape v` | escapes one URL path segment (rest) | `/silence/{{ pathEscape .id }}` |
| `param "name"` | a parameter value; needed in `render`, where `.` is not the params | `{{ param "window" }}` |
| `result "name"` | the result of an earlier call of `calls` (after its jq) | `{{ range result "pods" }}{{ .pod }}{{ end }}` |

### Useful sprig and text/template functions

| Functions | For |
|---|---|
| `printf` | formatting: `printf "%.2f" (float64 .x)` |
| `float64`, `int`, `int64` | type conversion |
| `add sub mul div mod max min` | integer arithmetic: `max 300 (promDurationSeconds .step)` |
| `addf subf mulf divf` | float arithmetic: `divf .min_ms 1000`, `mulf .min_mib 1048576` |
| `default` | fallback for nil/empty: `.nodename \| default "?"` |
| `kindIs "invalid" .x` | nil check that does not confuse nil with zero |
| `hasPrefix`, `trimPrefix`, `join`, `upper`, `lower`, `trim`, `replace` | strings |
| `list`, `append`, `dict`, `get`, `hasKey` | lists and maps: `{{ get (dict "cpu" 5 "mem" 3) .signal }}` |
| `toJson` | quoting values inside a JSON string body |
| `now`, `toDate`, `dateModify`, `dateInZone` | time arithmetic |
| `fail "msg"` | stop with an error at render time (validate parameter combinations) |
| `len`, `index`, `eq ne lt le gt ge`, `and or not` | built into text/template |
| `define` / `template` | reusable fragments inside one template |

### Safe substitution into PromQL

| Value | Do | Never |
|---|---|---|
| a label value | `job={{ promLabel .job }}` | `job="{{ .job }}"`: a quote in the value breaks the query |
| a caller's regex | `pod=~{{ promLabel .pod }}` | without `promLabel` |
| a literal inside your own regex | `{{ promLabel (printf "(?i)%s([.:].*)?" (promRegex .name)) }}` | `"{{ .name }}.*"` |
| a duration | `[{{ promDuration .window }}]` | `[{{ .window }}]` |
| a number | a param with `type: integer/number` and `{{ .top }}` | a string parameter |

The double escaping in the third row is correct: `promRegex` turns `db.1` into `db\.1`, `promLabel`
doubles the backslash for the PromQL string literal (`"db\\.1"`), Prometheus reads back `db\.1`, and
the dot matches only a dot.

### Techniques

**Variables.** Compute repeated pieces once at the start of a string. A variable lives until the end of
that value (e.g. the whole `query`), not in neighbouring `request` fields.

```yaml
query: |
  {{- $w := promDuration .window }}
  {{- $sel := printf "instance=~%s" (promLabel .instance) -}}
  rate(node_network_receive_bytes_total{ {{ $sel }} }[{{ $w }}])
```

**Whitespace.** `{{-` trims spaces and the newline on the left, `-}}` on the right. Lines that only
define variables leave empty lines unless trimmed.

**Back-quoted strings** help with double quotes: ``{{- $fs := `fstype!~"tmpfs|overlay"` }}``.

**The `{{{` trap.** `metric{{{ $sel }}}` does not parse. Add spaces: `metric{ {{ $sel }} }`.

**Arithmetic.** `[{{ max 300 (promDurationSeconds .step) }}s]`: a rate window of at least 5 minutes
and at least the step.

**Conditional parts:**

```yaml
query: |
  sum by (pod) (rate(container_cpu_usage_seconds_total{namespace={{ promLabel .namespace }}{{ if .container }}, container={{ promLabel .container }}{{ end }}}[5m]))
```

**Choosing a value by a parameter:** `min_delta: '{{ get (dict "cpu" 5 "mem" 3 "net_rx" 5) .signal }}'`.

**Validating combinations:** `{{ if lt (promDurationSeconds .to) (promDurationSeconds .from) }}{{ fail "to must be after from" }}{{ end }}`.
