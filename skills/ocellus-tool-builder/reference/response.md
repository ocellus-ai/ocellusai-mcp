# response: jq and render

## Three response levels

| Level | `response` | `content` (text) | `structuredContent` | Choose when |
|---|---|---|---|---|
| 0 | none | pretty JSON of the worker (the `calls` object) | the worker JSON | debugging; the answer is already compact |
| 1 | `jq` | pretty JSON of the jq result | the jq result | the calling agent processes the data further |
| 2 | `jq` + `render` | the template's text | the jq result | summaries, reports, analyses, large structures |

`render` without `jq` is allowed (a template over the raw JSON). Most clients show the model the text
block, so at level 2 **everything important must be in the text**. A non-object `structuredContent`
(an array, a number) is wrapped as `{"result": …}` by the server.

## jq (gojq)

- Input: the worker's output or the last `process` step's; with `calls`, `{name: result}`.
  Parameters: `$params.<name>`, no text substitution: `select(.used_pct >= $params.threshold)`.
- Compiled at startup; runtime errors are stage `jq`.
- Number of outputs: 0 → `null`, 1 → as is, several → an array. To keep the shape independent of the
  data, **wrap explicitly in `[...]` or `{...}`**.
- **Numbers.** `"1" | tonumber` is an integer, `"0.5"` a float; integer division without remainder
  stays integer (`1500 / 60` → `25`). Always convert with `float64` in `render`.
- **NaN and Inf.** `tonumber` fails on `"NaN"`: write `tonumber? // null`.
- **Truthiness.** Only `false` and `null` are false in jq; `0` and `""` are true. `0 // 5` → `0`.
- `first` on an empty array is `null`; a missing key is `null`; `all` on an empty array is `true`
  and `any` is `false` (a pending pod without container statuses is not "all ready": test the length).
- **Binding.** Bind `. as $x` before using `.` inside another context: in
  `$sig | index(.key)` the `.` is `$sig`, not the outer object.
- Useful: `def f: …;` at the start, `group_by`, `from_entries`, `with_entries`, `IN(...)`,
  `sort_by(-.x)`, `.[:$params.top]`, `round`, `ltrimstr`, `startswith`, `split(re; flags)`, `test(re)`,
  `todate`, `fromdateiso8601`, `fabs`, `min_by`, `max_by`.
- **Stable shape.** Every key of an object is always present (null when unknown): `render` fails on a
  missing key, and agents prefer predictable structures.
- **Don't round** in jq: `structuredContent` stays exact. Round only counters (`increase` → `round`).
  Formatting belongs to `render`.
- Sort and cap lists; put a summary (totals, analysed, skipped, the parameters used) next to the list.

Folding labelled series into rows:

```jq
def num: .value[1] | tonumber? // null;
def val($s): map(select(.metric.stat == $s) | num) | first;
def rows($prefix; $labels; $fields):
  map(select(.metric.stat | startswith($prefix)))
  | group_by(.metric[$labels[0]])
  | map(
      ($labels + $fields | map({key: ., value: null}) | from_entries)
      + (.[0].metric | with_entries(select(.key | IN($labels[]))))
      + (map({key: (.metric.stat | ltrimstr($prefix)), value: num}) | from_entries)
    );
```

The usual shape of an analysis answer:

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

## render

- Root `.` is the jq result; parameters via `{{ param "x" }}`; with `calls`, `{{ result "name" }}`.
- **Empty result first:** `{{ if not . }}Nothing found …{{ else }}…{{ end }}` (`not` is true for
  `[]`, `null`, `""`, `0`); for an object answer test a counter: `{{ if eq (int .total) 0 }}`.
- **A missing map key** is a `render` error; **nil** prints as `<no value>`. Test nil without
  confusing it with zero: `kindIs "invalid" .x`; substitute: `.x | default "n/a"`.
- **`{{ if .x }}` is false for `0`**: don't use it to test whether a number is present.
- **Numbers** from jq are `int` or `float64`: `printf "%.2f" (float64 .x)`; whole numbers
  `printf "%.0f" (float64 .x)`.
- **Small values** must not look like zero: `0.0004` with `%.2f` is `0.00`; print `<0.01`.
- Repeated formatting goes into `define` at the top.
- Name the context in the text: window, threshold, units, how many objects were analysed and skipped
  ("above 1% in the last 1h; 3 VM(s) skipped for lack of samples").
- Logic (filters, sorting, arithmetic) stays in jq; `render` only formats.
- Keep it compact: one line per object, indented sub-lines for findings.

Ready-made formatters:

```
{{- define "pct" }}{{ if kindIs "invalid" . }}n/a{{ else }}{{ $v := float64 . }}{{ if and (gt $v 0.0) (lt $v 0.01) }}<0.01%{{ else }}{{ printf "%.1f%%" $v }}{{ end }}{{ end }}{{ end }}
{{- define "num" }}{{ if kindIs "invalid" . }}n/a{{ else }}{{ printf "%.2f" (float64 .) }}{{ end }}{{ end }}
{{- define "bytes" }}{{ if kindIs "invalid" . }}n/a{{ else }}{{ $v := float64 . }}{{ if ge $v 1099511627776.0 }}{{ printf "%.1f TiB" (divf $v 1099511627776.0) }}{{ else if ge $v 1073741824.0 }}{{ printf "%.1f GiB" (divf $v 1073741824.0) }}{{ else if ge $v 1048576.0 }}{{ printf "%.1f MiB" (divf $v 1048576.0) }}{{ else if ge $v 1024.0 }}{{ printf "%.1f KiB" (divf $v 1024.0) }}{{ else }}{{ printf "%.0f B" $v }}{{ end }}{{ end }}{{ end }}
```

Usage: `used {{ template "pct" .used_pct }} of {{ template "bytes" .total_bytes }}`.

Whitespace: everything outside `{{ }}` goes into the text, including the newline after `{{ else }}`.
Trim lines that only hold `define` with `{{-`. A list with an empty case:

```
{{ range .items }}- {{ .name }}
{{ else }}  none reported
{{ end }}
```

Joining a list inline: `{{ range $i, $s := .skipped }}{{ if $i }}, {{ end }}{{ $s.instance }} ({{ $s.reason }}){{ end }}`.
