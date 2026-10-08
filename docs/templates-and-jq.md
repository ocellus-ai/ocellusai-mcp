# Templates and jq

[For DevOps](for-devops.md) · [Quick start](quick-start.md) · [User guide](user-guide.md) · [Workers](workers.md) · **Templates and jq** · [Processors](processors.md) · [Writing tools](writing-tools.md)

A tool file mixes three languages. YAML gives it structure. **Go templates** build the requests sent to the sources
and the text the agent reads. **jq** reshapes JSON between the steps. This guide teaches both languages as they are
used in tool files, so that you can read any tool and change what it asks and answers.

The worker requests are described in [Workers](workers.md), the analysis steps in [Processors](processors.md), and
how to design and write a whole tool in [Writing tools](writing-tools.md).

## Contents

- [A tool file at a glance](#a-tool-file-at-a-glance)
- [Parameters](#parameters)
- [Go templates](#go-templates)
- [Writing PromQL safely](#writing-promql-safely)
- [Template recipes](#template-recipes)
- [jq](#jq)
- [Results of earlier calls](#results-of-earlier-calls)
- [The answer text](#the-answer-text)
- [Reading a tool: a walkthrough](#reading-a-tool-a-walkthrough)
- [Debugging](#debugging)

## A tool file at a glance

```yaml
name: pod_memory_usage                       # YAML: what the agent calls
description: |                               # YAML: what the agent reads before calling
  Current working-set memory per pod in a namespace, in MiB, highest first.
  Optional `pod` is a regular expression matched against pod names.
worker: prometheus                           # YAML: which worker instance answers

params:                                      # YAML: becomes the tool's JSON Schema
  namespace: {type: string, required: true, description: "Kubernetes namespace"}
  pod:       {type: string, default: ".*", description: "Regex on pod name (RE2)"}
  top:       {type: integer, default: 10, minimum: 1, maximum: 100}

request:
  type: instant
  query: |                                   # Go template: builds the PromQL from the arguments
    topk({{ .top }}, sum by (pod) (
      container_memory_working_set_bytes{namespace={{ promLabel .namespace }}, pod=~{{ promLabel .pod }}, container!=""}
    ))

response:
  jq: |                                      # jq: reshapes the Prometheus answer
    [.result[] | {pod: .metric.pod, mib: ((.value[1] | tonumber) / 1048576 | floor)}] | sort_by(-.mib)
```

Where each language appears and what it works on:

| Field | Language | Input |
|---|---|---|
| `request`, every string inside it | template | the arguments |
| `calls[i].request` | template | the arguments, plus `result` of earlier calls |
| `calls[i].jq` | jq | the answer of this call |
| `process[i].with`, every string inside it | template | the arguments, plus `result` of all calls |
| `process` step `fn: jq`, its `expr` | jq | the output of the previous step |
| `response.jq` | jq | the output of the last step (or the worker's answer) |
| `response.render` | template | the result of `response.jq` |

Everything else is plain YAML. A string without `{{` is never treated as a template.

The data flow of a call, with the language at each arrow:

```
arguments ─template─▶ request ─▶ worker ─jq─▶ (results of calls) ─▶ process steps ─jq─▶ data ─template─▶ text
                                                                                       │
                                                                                       └──▶ structuredContent
```

## Parameters

Parameters are declared under `params`, one per line, and become the JSON Schema the agent sees.

```yaml
params:
  namespace: {type: string, required: true, description: "Kubernetes namespace, e.g. prod", pattern: "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"}
  window:    {type: string, default: "5m", description: "Rate window such as 5m or 1h", pattern: "^[0-9]+[smhdwy]$"}
  top:       {type: integer, default: 10, minimum: 1, maximum: 100, description: "How many pods to return"}
  mode:      {type: string, default: "both", enum: [both, up, down], description: "Which deviations to keep"}
```

| Field | Applies to | Meaning |
|---|---|---|
| `type` | all | `string`, `integer`, `number`, `boolean`, `array`, `object`; required |
| `required` | all | the caller must pass it (cannot be combined with `default`) |
| `default` | all | the value when the caller does not pass it; checked against the schema at startup |
| `description` | all | shown to the agent |
| `enum` | all | the allowed values |
| `minimum`, `maximum` | `integer`, `number` | inclusive bounds |
| `pattern` | `string` | a Go (RE2) regular expression; JSON Schema does not anchor it, so tools write `^…$` |

Parameter names are letters, digits and `_`, because they are used as `.name` in templates and `$params.name` in jq.

What the languages receive:

| `type` | In templates (`.x`, `param "x"`) | In jq (`$params.x`) |
|---|---|---|
| `string` | a string | a string |
| `integer` | an integer | a number |
| `number` | a float: `{{ .x }}` prints `0.5`, `1`, `1e+21` | a number |
| `boolean` | `true` / `false` | `true` / `false` |
| `array`, `object` | a list, a map | an array, an object |
| optional, not passed, no default | nil: `{{ .x }}` prints **`<no value>`** | `null` |

Every declared parameter is present, so referring to an optional one never fails, but it prints `<no value>` when it
was not passed. Tools give optional parameters a default or test them with `{{ if .x }}`.

## Go templates

Tools use Go's [`text/template`](https://pkg.go.dev/text/template) with all [sprig](https://masterminds.github.io/sprig/)
functions and a few helpers of their own.

### Syntax in brief

| Write | Means |
|---|---|
| `{{ .namespace }}` | the value of `namespace` from the current root `.` |
| `{{ .a.b }}` | a nested field |
| `{{ promLabel .namespace }}` | call a function with an argument |
| `{{ .window \| promDuration }}` | pipe: the value becomes the last argument of the next function |
| `{{ $w := promDuration .window }}` … `{{ $w }}` | define a variable, use it later in the same string |
| `{{ $names = append $names .pod }}` | assign to an existing variable |
| `{{ if .x }}…{{ else if .y }}…{{ else }}…{{ end }}` | condition; false is `false`, `0`, `""`, nil, an empty list or map |
| `{{ range .items }}{{ .name }}{{ else }}none{{ end }}` | loop; inside, `.` is the element; `else` runs for an empty list |
| `{{ range $i, $x := .items }}{{ if $i }}, {{ end }}{{ $x }}{{ end }}` | loop with the index |
| `{{ with .x }}{{ . }}{{ end }}` | run the block with `.` set to `.x`, if it is not empty |
| `{{ eq .a "x" }}`, `ne lt le gt ge`, `and or not` | comparisons and logic |
| `{{ len .items }}`, `{{ index .m "key" }}` | length; element of a map or list |
| `{{- … }}`, `{{ … -}}` | trim the whitespace and newlines before / after the action |
| `{{/* a comment */}}` | a comment |
| `{{ define "pct" }}…{{ end }}` … `{{ template "pct" .x }}` | a named fragment, called with an argument |

### Where templates run and what `.` is

| Place | Root `.` | `param "x"` | `result "name"` |
|---|---|---|---|
| `request` | the arguments | yes | no |
| `calls[i].request` | the arguments | yes | calls before `i` |
| `process[i].with` | the arguments | yes | all calls |
| `response.render` | the result of `response.jq` | yes | all calls |

In `render`, `.` is the data, so parameters are reached with `{{ param "window" }}`.

### What goes in and what comes out

- **The result of a template is always a string.** `step: "{{ .step }}"` sends the string `"5m"`. Values written in
  YAML without a template keep their type: `min_points: 288` is a number, `normalize: false` a boolean. Workers and
  processors accept numbers written as strings where a number is expected.
- **A missing map key is an error** (`map has no entry for key "namepace"`), not an empty string. A declared but unset
  parameter is not missing: it is nil.
- **Startup checks syntax and function names, not field names.** `{{ .namepace }}` passes `-validate` and fails on the
  first call, at stage `request` or `render`.

### Helpers added by Ocellus AI

| Function | Does | Example |
|---|---|---|
| `promLabel v` | escapes `\ " \n \t \r` and wraps the value in double quotes, for a PromQL label value | `namespace={{ promLabel .namespace }}` → `namespace="prod"` |
| `promRegex v` | escapes regular-expression characters, so the value matches literally inside a regex | `db.1` → `db\.1` |
| `promDuration v` | checks a Prometheus duration and returns its canonical form; an invalid one fails the call | `[{{ promDuration .window }}]`; `60s` → `1m` |
| `promDurationSeconds v` | the same, as whole seconds | `{{ promDurationSeconds .step }}` → `300` |
| `pathEscape v` | escapes one segment of a URL path (REST tools) | `/silence/{{ pathEscape .id }}` |
| `param "name"` | the value of a parameter; an undeclared name is an error | `{{ param "window" }}` |
| `result "name"` | the stored result of an earlier call of `calls` | `{{ range result "addrs" }}…{{ end }}` |

### Sprig functions you will meet

| Functions | Used for |
|---|---|
| `printf` | formatting: `printf "%.2f" (float64 .x)` |
| `float64`, `int`, `int64`, `toString` | type conversion |
| `add sub mul div mod max min` | integer arithmetic: `max 300 (promDurationSeconds .step)` |
| `addf subf mulf divf` | float arithmetic: `divf .min_ms 1000`, `mulf .min_mib 1048576` |
| `default` | a fallback for empty values: `.nodename \| default "?"` |
| `kindIs "invalid" .x` | true for nil, without confusing nil with zero |
| `hasPrefix`, `trimPrefix`, `trimSuffix`, `replace`, `upper`, `lower`, `trim`, `join`, `splitList`, `regexMatch` | strings |
| `list`, `append`, `has`, `dict`, `get`, `hasKey`, `keys` | lists and maps |
| `toJson` | a value as JSON, for request bodies |
| `now`, `toDate`, `dateModify`, `date`, `unixEpoch` | time |
| `fail "message"` | stop the call with an error |

Sprig also has `env` and `expandenv`, which read the server's environment variables. They are available to
whoever writes tool files; this is why tool files are trusted like code (see the
[security model](user-guide.md#security-model)).

## Writing PromQL safely

A value from the agent must never be pasted into PromQL as is: a quote in it would end the string and let the rest
become part of the query.

| The value is | Write | Never |
|---|---|---|
| a label value | `job={{ promLabel .job }}` | `job="{{ .job }}"` |
| a regex from the caller | `instance=~{{ promLabel .instance }}` | `instance=~"{{ .instance }}"` |
| a literal inside your own regex | `{{ promLabel (printf "(?i)%s([.:].*)?" (promRegex .name)) }}` | `"{{ .name }}.*"` |
| a duration | `[{{ promDuration .window }}]` | `[{{ .window }}]` |
| a number | a parameter of `type: integer` or `number`, then `{{ .top }}` | a string parameter |

The double escaping in the third row is right: `promRegex` turns `db.1` into `db\.1`, `promLabel` doubles the
backslash for the PromQL string (`"db\\.1"`), and Prometheus reads it back as the regex `db\.1`.

Prometheus anchors label regexes: `instance=~"web-1"` does not match `web-1:9100`. Tools that take a name add
`([.:].*)?` (an optional domain or port) or `.*`.

## Template recipes

### A window that ends at `at`

Most tools take `at` (`now`, `now-<duration>` or an RFC 3339 time) and a `range`, and compute the start of a range
query from both:

```yaml
start: '{{ if eq .at "now" }}now-{{ promDuration .range }}{{ else if hasPrefix "now-" .at }}now-{{ add (promDurationSeconds (trimPrefix "now-" .at)) (promDurationSeconds .range) }}s{{ else }}{{ (toDate "2006-01-02T15:04:05Z07:00" .at | dateModify (printf "-%ds" (promDurationSeconds .range))).Unix }}{{ end }}'
end: "{{ .at }}"
```

Read it in three branches:

1. `at` is `now`: the start is `now-<range>`, for example `now-6h`.
2. `at` is `now-3d`: the start is `now-` plus both durations in seconds, `now-280800s`.
3. `at` is a time: parse it (`toDate` with Go's reference layout), shift it back by the range (`dateModify "-21600s"`)
   and print unix seconds (`.Unix`).

Prometheus accepts all three forms. Tools with several range calls repeat the same `start`, `end` and `step` in every
call, so the samples line up for `join`.

### Choosing a query by a parameter

```yaml
query: |
  {{- if eq .signal "cpu" -}}
  100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))
  {{- else if eq .signal "mem" -}}
  100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)
  {{- else -}}
  max by (instance) (node_load1)
  {{- end }}
```

The `-` in `{{-` and `-}}` keeps the newlines of the template out of the query. The parameter is an `enum`, so no
other value can arrive.

### Picking a number by a parameter

```yaml
min_delta: '{{ get (dict "cpu" 5 "mem" 3 "net_rx" 5 "load" 0.25) .signal }}'
```

### An optional filter

```yaml
query: |
  ALERTS{alertstate="firing"{{ if .severity }}, severity={{ promLabel .severity }}{{ end }}}
```

### Natural units for the caller

Give the agent milliseconds, MiB or percentage points, and convert where the value is used:

```yaml
min_delta: "{{ divf .min_ms 1000 }}"       # ms → seconds
min_delta: "{{ mulf .min_mib 1048576 }}"   # MiB → bytes
```

### Rejecting a bad combination

```yaml
query: |
  {{- if lt (promDurationSeconds .to) (promDurationSeconds .from) }}{{ fail "to must be after from" }}{{ end -}}
  …
```

The call stops at stage `request` (in `process[i].with`, at stage `process`) with the message.

### Variables for repeated parts

```yaml
query: |
  {{- $w := promDuration .window }}
  {{- $sel := printf "instance=~%s" (promLabel .instance) -}}
  rate(node_network_receive_bytes_total{ {{ $sel }} }[{{ $w }}])
```

A variable lives until the end of the string it is defined in (here, the whole `query`), not in other fields.

### YAML and templates

- **Quote values that start with `{{`**: `step: "{{ .step }}"`. Unquoted, YAML reads `{` as the start of a map and
  the file does not load.
- **Multi-line values use `|`.** Inside a `|` block, quotes, braces and `#` need no escaping, and `#` is part of the
  value, not a comment. PromQL accepts `#` comments; jq and templates do not.
- **`metric{{{ $sel }}}` does not parse.** Add spaces: `metric{ {{ $sel }} }`.
- **Backquotes** help with double quotes: ``{{- $fs := `fstype!~"tmpfs|overlay"` }}``.

## jq

Tools use [gojq](https://github.com/itchyny/gojq), a Go implementation of the jq language.

### Where jq runs

| Field | Input | Typical job |
|---|---|---|
| `calls[i].jq` | the answer of call `i` | make it simple for the templates of the next calls (a flat list of names) |
| `process` step `fn: jq`, key `expr` | the output of the previous step | bring data to the shape the next processor accepts |
| `response.jq` | the output of the last step | build the answer: pick, join, sort, cap, summarise |

All of them see the tool's arguments as `$params`. They are compiled at startup, so a syntax error stops the server
with the file and field name.

### The essentials

| Expression | Means |
|---|---|
| `.result[]` | iterate over the elements of `.result` |
| `.a.b`, `.a?`, `.[0]`, `.[-1]`, `.[:10]` | paths; `?` suppresses errors; slices |
| `[ … ]` | collect all outputs into an array |
| `{pod: .metric.pod, v: .value[1]}` | build an object; `{pod}` is short for `{pod: .pod}` |
| `map(f)`, `select(cond)` | transform each element, keep the matching ones |
| `a // b` | `a`, unless it is `null` or `false`, else `b` |
| `. as $x \| …` | name a value |
| `def f: …;` | define a function at the top of the expression |
| `sort_by(-.x)`, `group_by(.k)`, `unique_by(.k)`, `min_by`, `max_by` | ordering and grouping |
| `add`, `length`, `keys`, `to_entries`, `from_entries`, `with_entries(f)` | aggregates and object helpers |
| `tonumber`, `tostring`, `round`, `floor`, `fabs` | conversions and numbers |
| `test(re)`, `capture(re)`, `sub(re; s)`, `split(re; flags)`, `ltrimstr`, `startswith` | strings |
| `todate`, `fromdateiso8601`, `now` | time |
| `"\(.a) of \(.b)"` | string interpolation |

### Outputs

An expression can produce zero, one or several values:

- zero outputs → `null`;
- one → that value;
- several → an array of them.

So `.result[] | .metric.pod` returns a string when there is one pod and an array when there are several. Tools wrap
results explicitly, `[ .result[] | .metric.pod ]`, to keep the shape stable.

### `$params`

Arguments arrive as data, never as text pasted into the expression:

```jq
[ .[] | select(.used_pct >= $params.threshold) ] | .[:$params.top]
```

An optional parameter that was not passed is `null`.

### Prometheus data in jq

Values in a Prometheus answer are **strings**: convert them.

```jq
# instant vector → rows
[ .result[] | {instance: .metric.instance, v: (.value[1] | tonumber? // null)} ]

# range matrix → average per series
[ .result[] | {pod: .metric.pod, avg: ([.values[][1] | tonumber] | add / length)} ]
```

Values can be `"NaN"` or `"+Inf"`, on which `tonumber` fails; `tonumber? // null` turns them into `null`.

### Several metrics in one query

Tools often fetch unrelated values with one instant query: each part gets a synthetic label `stat` with
`label_replace`, the parts are combined with `or`, and jq folds them back into one row per object:

```promql
label_replace(100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))), "stat", "cpu_pct", "", "")
or label_replace(100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes), "stat", "mem_pct", "", "")
```

```jq
[ .result
  | group_by(.metric.instance)[]
  | (map({key: .metric.stat, value: (.value[1] | tonumber? // null)}) | from_entries) as $s
  | {instance: .[0].metric.instance, cpu_pct: $s.cpu_pct, mem_pct: $s.mem_pct}
]
| sort_by(-(.cpu_pct // 0))
```

`$s.mem_pct` is `null` when a host has no memory metric, so every row has every field.

### Numbers, null and truth

- **Integers and floats.** `"1" | tonumber` is an integer, `"0.5"` a float; division without a remainder stays an
  integer (`1500 / 60` is `25`). Numbers that come from a source's JSON stay floats. The difference matters in
  `render` (below).
- **Truth.** Only `false` and `null` are false. `0` and `""` are true, and `0 // 5` is `0`.
- **Missing keys** are `null`; `first` of an empty array is `null`; `add` of an empty array is `null` (hence
  `add // 0`).
- **`all` of an empty array is `true`**, `any` is `false`: a pod with no container statuses is not "all ready";
  check the length first.
- **Binding.** Inside `$list | index(.key)`, `.` is `$list`. Bind the outer value first: `. as $t | $list | index($t.key)`.

### Shaping an answer

The answers of analysis tools usually have a summary and a capped list:

```jq
[ .series[] | select(.skipped | not) | select(.anomaly_count > 0)
  | {instance: .metric.instance, count: .anomaly_count, median: .stats.median,
     anomalies: (.anomalies | map({time, value, direction, score}))}
] as $objs
| {
    at: $params.at, range: $params.range,
    analyzed: .series_analyzed, skipped: .series_skipped,
    raw_total: (.total_anomalies + ([.series[].below_min_delta // 0] | add // 0)),
    total: .total_anomalies,
    objects: ($objs | sort_by(-.count) | .[:$params.top])
  }
```

Conventions in the bundled tools: every key is always present (null when unknown) so the template never meets a
missing key; lists are sorted and capped; numbers are not rounded in jq, so `structuredContent` stays exact, and the
text template formats them.

### gojq and jq

Programs written for jq 1.6/1.7 almost always run unchanged. Differences you may meet:

- objects have no key order: keys come out sorted, and `keys_unsorted` does not exist;
- regular expressions are Go's RE2: no lookahead and no backreferences;
- integers keep their precision;
- `env` and `$ENV` are empty: jq expressions cannot read the server's environment;
- `input` and `inputs` are not available: an expression that uses them does not load.

## Results of earlier calls

In a tool with `calls`, each call has a name and its result is stored under it, after its own `jq` if it has one.
Later templates read it with `result "name"`; `response.jq` and `process` steps receive all results as one object.

```yaml
calls:
  - name: addrs
    worker: shell
    request:
      command: dig
      args: [+short, A, "{{ .name }}"]
      parse: lines
    jq: '[.[] | select(test("^[0-9]+(\\.[0-9]+){3}$"))] | unique'   # stored as result "addrs"
  - name: up
    worker: prometheus
    request:
      query: |
        {{- $addrs := list }}{{ range result "addrs" }}{{ $addrs = append $addrs (promRegex .) }}{{ end -}}
        up{instance=~{{ promLabel (printf "(%s)(:[0-9]+)?" (join "|" $addrs | default "__none__")) }}}

response:
  jq: |                                           # the input is {addrs: [...], up: {resultType, result}}
    (.up.result | map({key: (.metric.instance | sub(":[0-9]+$"; "")), value: (.value[1] == "1")}) | from_entries) as $up
    | [ .addrs[] | {address: ., up: $up[.]} ]
```

- `result "x"` in a call's request sees only calls **before** it. A literal reference to a later or unknown call is a
  startup error.
- The first call's `jq` returns a flat shape (a list of names or simple objects): templates are clumsy with raw
  output and deep JSON, and this shape is also what `response.jq` gets. Here it also drops the CNAME lines that
  `dig +short` prints before the addresses.
- `default "__none__"` matters: with an empty list, the regex would match every series **without** an `instance`
  label; `__none__` matches nothing.
- `$up[.]` is `null` for an address Prometheus does not scrape, so it stays in the list. With several targets on one
  address this short join keeps one of them; [`tools-test/dns_targets_up.yaml`](../tools-test/dns_targets_up.yaml)
  keeps them all.

## The answer text

`response.render` turns the jq result into the text the model reads. Most clients show the model only this text, so
the bundled tools put the parameters, counts and thresholds into it.

```
{{ if not . }}No pods in namespace {{ param "namespace" }}.{{ else }}
{{ len . }} pod(s) in {{ param "namespace" }}, CPU over the last {{ param "window" }}:
{{ range . }}{{ .pod }}: {{ .phase }}, {{ printf "%.3f" (float64 .cores) }} cores
{{ end }}{{ end }}
```

Rules that keep `render` from failing or misleading:

- **Handle the empty result first.** `not` is true for `[]`, `null`, `""` and `0`. For an object answer, test a
  counter: `{{ if eq (int .total) 0 }}`.
- **Convert numbers before formatting.** jq produces integers and floats. `printf "%.2f"` of an integer prints
  `%!f(int=25)` instead of failing, and `eq` of an integer and a float is an error
  (`incompatible types for comparison`). Write `printf "%.2f" (float64 .x)` and `eq (int .n) 0`.
- **nil prints `<no value>`.** Test it with `kindIs "invalid" .x` (which does not confuse nil with zero) or replace
  it: `.x | default "n/a"`.
- **`{{ if .x }}` is false for 0**: don't use it to ask whether a number is present.
- **Small values must not look like zero.** `0.0004` with `%.2f` is `0.00`; print `<0.01`.
- **Repeated formatting goes into `define`** at the top:

  ```
  {{- define "pct" }}{{ if kindIs "invalid" . }}n/a{{ else }}{{ $v := float64 . }}{{ if and (gt $v 0.0) (lt $v 0.01) }}<0.01%{{ else }}{{ printf "%.1f%%" $v }}{{ end }}{{ end }}{{ end }}
  {{- define "mib" }}{{ printf "%.0f MiB" (divf (float64 .) 1048576.0) }}{{ end -}}
  used {{ template "pct" .used_pct }} of {{ template "mib" .total_bytes }}
  ```

- **Whitespace is literal.** Everything outside `{{ }}`, including newlines after `{{ else }}`, goes into the text.
  `{{-` and `-}}` trim it.
- **Lists inline:** `{{ range $i, $s := .skipped }}{{ if $i }}, {{ end }}{{ $s.instance }} ({{ $s.reason }}){{ end }}`.
- **Logic belongs in jq.** Filters, sorting and arithmetic go into `response.jq`; `render` only formats.

## Reading a tool: a walkthrough

[`tools/pod_memory_outliers.yaml`](../tools/pod_memory_outliers.yaml) uses every language in a few lines. It finds
pods whose memory stands out from the other pods of the same namespace.

```yaml
params:
  namespace:   {type: string, default: "kube-system", description: "Regex over the namespace label"}
  cluster:     {type: string, default: ".*", description: "Regex over the cluster label, e.g. prod-.*"}
  at:          {type: string, default: "now", description: "Evaluation time: …", pattern: "^(now(-[0-9]+[smhdwy])?|…)$"}
  sensitivity: {type: number, default: 1.5, minimum: 0.1, maximum: 10, description: "IQR fence multiplier k: …"}
  direction:   {type: string, default: "up", enum: [both, up, down], description: "up = memory hogs, …"}
  min_mib:     {type: number, default: 100, minimum: 0, maximum: 1048576, description: "Smallest difference from the group's median pod worth reporting, MiB"}
```

Six parameters, all with defaults, so the agent can call the tool with no arguments. `at` has a pattern, the numbers
have bounds, `direction` is an enum: nothing else can reach the query.

```yaml
request:
  type: instant
  query: |
    sum by (cluster, namespace, pod) (
      container_memory_working_set_bytes{job="kubelet", container!="", pod!="", namespace=~{{ promLabel .namespace }}, cluster=~{{ promLabel .cluster }}}
    )
  time: "{{ .at }}"
```

A template builds the PromQL. Both regexes go through `promLabel`. `time` is quoted because it starts with `{{`.
The answer is an instant vector: one value per pod, with `cluster`, `namespace` and `pod` labels.

```yaml
process:
  - fn: jq
    with:
      expr: '[.result[] | {key: .metric.pod, value: .value[1], labels: {cluster: .metric.cluster, namespace: .metric.namespace}}]'
```

A jq step turns the vector into the list the `outliers` processor accepts: `key` names the object, `value` is the
number (a numeric string is fine), `labels` are what it groups by.

```yaml
  - fn: outliers
    with:
      method: iqr
      k: "{{ .sensitivity }}"
      group_by: [cluster, namespace]
      direction: "{{ .direction }}"
      min_points: 5
      min_delta: "{{ mulf .min_mib 1048576 }}"   # bytes from the median; applied before the cap
      max_anomalies: 5
```

The processor's settings mix literals (`method`, `group_by`, `min_points`) with templates over the arguments. The
caller's MiB become bytes with `mulf`. What these keys do is in [Processors](processors.md#outliers).

```yaml
response:
  jq: |
    {
      at: $params.at,
      groups_total: .series_total, groups_analyzed: .series_analyzed, groups_skipped: .series_skipped, total: .total_anomalies,
      below_min: ([.series[].below_min_delta // 0] | add // 0),
      groups: [ .series[] | {cluster: .labels.cluster, namespace: .labels.namespace, pods: .points, skipped, reason,
                            median: .stats.median, upper: .stats.upper,
                            outliers: [.anomalies[] | {pod: .key, bytes: .value, direction, score}]} ]
              | sort_by(-(.outliers | length), -.pods)
    }
```

`response.jq` turns the processor's report into the answer: a summary (how many groups, how many outliers, how many
were closer to the median than `min_mib`) and one entry per group, the groups with outliers first. `$params.at`
echoes the argument. This object becomes `structuredContent`.

```yaml
  render: |
    {{- define "mib" }}{{ printf "%.0f MiB" (divf (float64 .) 1048576.0) }}{{ end -}}
    {{ if eq (int .groups_total) 0 }}No pods matched namespace {{ param "namespace" }} / cluster {{ param "cluster" }} at {{ .at }}.{{ else }}Pod memory outliers at {{ .at }} (IQR k={{ param "sensitivity" }}, direction {{ param "direction" }}, at least {{ param "min_mib" }} MiB from the median): {{ .total }} outlier(s) in {{ .groups_analyzed }} group(s){{ if .below_min }} ({{ .below_min }} closer to the median ignored){{ end }}, {{ .groups_skipped }} group(s) skipped.
    {{ range .groups }}
    {{ .cluster }} / {{ .namespace }}: {{ .pods }} pod(s){{ if .skipped }}, skipped ({{ .reason }}){{ else }}, median {{ template "mib" .median }}, upper fence {{ template "mib" .upper }}{{ if not .outliers }}, no outliers{{ end }}{{ end }}
    {{- range .outliers }}
      - {{ .pod }}: {{ template "mib" .bytes }} ({{ .direction }}, score {{ printf "%.1f" (float64 .score) }})
    {{- end }}{{ end }}{{ end }}
```

The text: a `define` for MiB, the empty case first (`int` before `eq`), then a header that repeats the parameters and
counts, then one line per group and one indented line per outlier. Every number goes through `float64` before
`printf`.

## Debugging

### What `-validate` catches

`-validate` checks the YAML, the parameter schemas, the request keys of each worker, the processor settings, the
syntax of every template and jq expression, function names and `result` references. It does not check field names
inside templates, PromQL, commands, real data or the formatting of the answer. Only a call does.

### See the rendered request

Set `log.level: debug` and call the tool. Every call logs `request rendered` with the final request: the PromQL with
the arguments substituted, the command line, the REST path and query. Copy the PromQL into the Prometheus UI and
work on it there.

### Try jq on real data

Save a worker's answer and run the expression with the `jq` (or `gojq`) command-line tool. For Prometheus, the tool
receives the `data` part of the API answer:

```bash
curl -s http://prometheus:9090/api/v1/query --data-urlencode 'query=up' | jq '.data' > answer.json
```

```bash
jq --argjson params '{"top": 5}' '[.result[] | {job: .metric.job, up: (.value[1] | tonumber)}] | .[:$params.top]' answer.json
```

### Call the tool

```bash
python3 scripts/mcp-call.py -quiet -config config.stdio.yaml call pod_memory_outliers '{"namespace": "prod"}' --structured
```

`--structured` prints the JSON next to the text, so you can see what `response.jq` produced and what `render` made of
it.

### Common errors

| Message | Cause |
|---|---|
| `map has no entry for key "namepace"` (stage `request` or `render`) | a misspelt field, or a key that jq did not produce |
| `param "x" is not declared for this tool` | `param` of an undeclared parameter |
| `<no value>` in a query or text | an optional parameter without a default, or nil data |
| `%!f(int=25)` in the text | `printf "%f"` of an integer: add `float64` |
| `incompatible types for comparison` | `eq`/`lt` between an integer and a float: convert both |
| `cannot iterate over null` (stage `jq`) | the expected field is missing in the data |
| `cannot iterate over string` (stage `jq`) | a command printed text where JSON was expected |
| `error calling promDuration: … unknown unit …` | the argument is not a Prometheus duration; constrain the parameter with a `pattern` |
| a YAML error with a line number | an unquoted value that starts with `{{` |
