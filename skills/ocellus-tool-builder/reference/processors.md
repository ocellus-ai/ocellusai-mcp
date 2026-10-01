# process and the processors

## Contents

- [How process works](#how-process-works)
- [Choosing a processor](#choosing-a-processor)
- [jq](#jq)
- [anomaly](#anomaly)
- [outliers](#outliers)
- [join](#join)
- [anomaly_mv](#anomaly_mv)
- [anomaly_ensemble](#anomaly_ensemble)
- [cluster_events](#cluster_events)
- [ssa and ssa_mv](#ssa-and-ssa_mv)
- [min_delta: the significance threshold](#min_delta-the-significance-threshold)

## How process works

`process` is a list of steps between the worker and `response`. The first step gets the worker's JSON
(with `calls`, the object `{name: result}`); step N+1 gets the output of step N; the last output goes
to `response.jq` and `structuredContent`.

```yaml
process:
  - fn: jq                       # reshape: pick an input, build the canonical list
    with: {expr: '[.result[] | {key: .metric.pod, value: .value[1]}]'}
  - fn: outliers                 # processor name
    with:                        # settings, owned entirely by the processor
      method: iqr                # a literal
      k: "{{ .sensitivity }}"    # strings are templates over the params
      min_points: 5
```

- `with` is rendered on every call; numbers may be numbers or strings from templates; with `calls`,
  `result "name"` is available in `with`.
- An unknown `with` key is a startup error (`process[0] (anomaly): …`). Template values are checked
  only at call time.
- Every typed processor accepts **one** input shape:
  - **scalar series** = Prometheus matrix, what a `range` query returns:
    `{resultType: matrix, result: [{metric, values: [[ts, "v"], …]}]}`;
  - **multivariate series** = `{dims: [cpu, mem], series: [{labels, points: [[ts, v1, v2], …], reason?}]}`,
    built by `join`;
  - **list of objects** without time = `[{key, value, labels?}]`;
  - **list of events** = `[{start, end, labels?, signals?, score?}]`.
  Anything else is shaped by a preceding `fn: jq` step; processors have no selector keys.
- A processor either **transforms** (output of a canonical shape: `join`, `jq`) or **analyses**
  (output is a report: `anomaly`, `outliers`, `anomaly_mv`, `anomaly_ensemble`, `cluster_events`,
  `ssa`, `ssa_mv`; usually the last step, followed by `response.jq`).

## Choosing a processor

| `fn` | Question it answers | Input | Output |
|---|---|---|---|
| `jq` | reshape data between steps | any JSON | any JSON |
| `anomaly` | which samples are unusual for *this* object's own history | matrix (`range` query) | report |
| `outliers` | which object stands out among the others *now* | `[{key, value, labels?}]` | report |
| `join` | stitch several metrics per object into one series | `{call name: matrix}` from `calls` | multivariate series |
| `anomaly_mv` | where the usual relation between metrics breaks | multivariate series (≥ 2 dims) | report |
| `anomaly_ensemble` | anomalous *segments*, with the metric to blame | multivariate series (≥ 1 dim) | report |
| `cluster_events` | which events on many objects happened together | `[{start, end, labels?, signals?, score?}]` | report |
| `ssa` | trend, cycles, noise, forecast of each series | matrix | report |
| `ssa_mv` | the same for several metrics of one object together | multivariate series | report |

## jq

```yaml
  - fn: jq
    with:
      expr: |                              # a literal: no {{ }} inside, parameters via $params
        [.result[] | select(.value[1] | tonumber >= $params.min) | {key: .metric.pod, value: .value[1]}]
```

- The only key is `expr`, compiled at startup (`process[0] (jq): with.expr: jq parse: …`).
- Output like `response.jq`: zero outputs → `null`, one → as is, several → an array.
- Uses: pick an input from `{name: result}`; build `[{key, value}]` for `outliers` from a vector,
  `df` lines or a REST answer; flatten a report into events for `cluster_events`; narrow a report.
- Don't use it for the final answer shape: that is `response.jq`.

## anomaly

Point anomalies in every series of a **range** query, each series against its own history. An
instant query is an error with the hint `request.type: range`.

| `with` key | Meaning |
|---|---|
| `method` | `zscore` or `iqr`; required; **literal only** |
| `min_points` | shorter series are `skipped`; default 10 |
| `direction` | `both` (default), `up`, `down` |
| `max_anomalies` | strongest anomalies per series; 0 = all; default 20 |
| `min_delta` | smallest deviation from `expected` worth reporting, metric units; default 0 = off |
| `min_rel_delta` | the same as a share of \|`expected`\| (0.2 = 20%); the larger of the two applies |
| `threshold` | `zscore`: \|z\| threshold, > 0, default 3 |
| `k` | `iqr`: IQR multiplier (Tukey fences), > 0, default 1.5 |

- `zscore`: mean and standard deviation; a flat series has no anomalies; sensitive to the outliers
  themselves.
- `iqr`: fences `[q1 − k·IQR, q3 + k·IQR]`, robust; `expected` is the median. Prefer it.
- `NaN`/`Inf` are dropped.

Report (`.series[]` per input series):

```json
{
  "method": "iqr",
  "series_total": 2, "series_analyzed": 1, "series_skipped": 1, "total_anomalies": 1,
  "series": [
    {"metric": {"pod": "api-1"}, "points": 120, "skipped": false, "anomaly_count": 1, "below_min_delta": 4,
     "stats": {"q1": 0.38, "median": 0.41, "q3": 0.45, "iqr": 0.07, "lower": 0.27, "upper": 0.56, "k": 1.5, "min": 0.35, "max": 1.9},
     "anomalies": [
       {"timestamp": 1757592000, "time": "2025-09-11T12:00:00Z", "value": 1.9, "score": 19.1, "expected": 0.41, "direction": "up"}
     ]},
    {"metric": {"pod": "job-x"}, "points": 3, "skipped": true, "reason": "fewer than 10 points (min_points)",
     "anomaly_count": 0, "anomalies": []}
  ]
}
```

`zscore` stats are `mean, stddev, threshold, min, max`. `anomaly_count` counts after `min_delta`
and before the `max_anomalies` cut; `anomalies` are the strongest, in time order; `below_min_delta`
appears when the filter is on. Values are not rounded. Make the range and step give clearly more
points than `min_points` (2 h at 60 s = 120 points). Template: [../templates/prometheus-anomaly.yaml](../templates/prometheus-anomaly.yaml).

## outliers

Outliers in a **set of objects without a time axis**: each object is compared with the others of its
group, not with its own history (filesystems, pods, an instant vector, REST rows). Same detectors and
detector keys as `anomaly`.

Input, built by a preceding `jq` step:

```json
[{"key": "/data", "value": 97, "labels": {"host": "a"}}, ...]
```

- `value` required: a number or numeric string (Prometheus values need no `tonumber`); missing or
  non-numeric is an error naming the item; `NaN`/`Inf` dropped.
- `key`: optional scalar, the object's name in the report.
- `labels`: optional object of scalars, needed only for `group_by`. Other fields are ignored.

| `with` key | Meaning |
|---|---|
| `method` | `zscore` \| `iqr`, required, literal |
| `group_by` | a label or list of labels; objects with equal values form one series (default: one series for all) |
| `min_points` | smaller groups are `skipped` (default 5) |
| `direction` | `both` \| `up` \| `down` (default `both`) |
| `max_anomalies` | strongest outliers per group, 0 = all (default 20) |
| `min_delta`, `min_rel_delta` | as for `anomaly` |
| `threshold` (zscore), `k` (iqr) | as for `anomaly` |

Report: like `anomaly` with three differences: the series field is `labels` (only `group_by` labels);
an outlier has `index` (position in the input) and `key` instead of `timestamp`/`time`; outliers are
sorted **strongest first**. Without `group_by` there is exactly one series, so `.series[0]` is safe
even for an empty input (it is then `skipped`). Objects missing a `group_by` label form one skipped
series with a `reason`.

```json
{"method": "iqr", "group_by": [], "series_total": 1, "series_analyzed": 1, "series_skipped": 0, "total_anomalies": 1,
 "series": [{"labels": {}, "points": 8, "skipped": false, "anomaly_count": 1,
             "stats": {"q1": 39.75, "median": 40.5, "q3": 42.25, "iqr": 2.5, "lower": 36, "upper": 46, "k": 1.5, "min": 38, "max": 97},
             "anomalies": [{"index": 3, "key": "/data", "value": 97, "score": 20.4, "expected": 40.5, "direction": "up"}]}]}
```

Small sets: the z-score of `n` objects never exceeds `(n−1)/√n` (2.04 for six objects), so `zscore`
with threshold 3 never fires on a handful of objects; use `iqr`. Idioms: instant vector
`[.result[] | {key: .metric.<label>, value: .value[1]}]`; REST `[.[] | {key: .id, value: .duration,
labels: {ref: .ref}}]` with `group_by: [ref]`. Template: [../templates/prometheus-outliers.yaml](../templates/prometheus-outliers.yaml).

## join

Transform for tools with `calls`: several range results are stitched by labels and time into **one
multivariate series per object**.

| `with` key | Meaning |
|---|---|
| `inputs` | call names = dimensions (`dims`) in this order; ≥ 1; required |
| `join_by` | labels that identify the object across inputs, e.g. `[instance]`; required |

- Each input must have exactly one series per `join_by` value: aggregate (`avg by (instance)`,
  `max by (instance)`), otherwise a `process` error with a hint.
- Samples are matched by exact timestamp, so every call needs the same `start`, `end`, `step`.
- Series without a `join_by` label, and objects missing from an input, are kept without points and
  with a `reason` (`missing in input(s) cpu`); the next analysis reports them as `skipped`.

Output (`labels` holds only the `join_by` labels):

```json
{"dims": ["cpu", "mem"],
 "series": [
   {"labels": {"instance": "vm-1:9100"}, "points": [[1757601020, 0.30, 0.75], [1757601080, 0.31, 0.71]],
    "input_points": {"cpu": 120, "mem": 120}},
   {"labels": {"instance": "vm-3:9100"}, "points": [], "reason": "missing in input(s) cpu",
    "input_points": {"cpu": 0, "mem": 120}}]}
```

## anomaly_mv

Every sample judged on all metrics at once: catches anomalies invisible per metric (memory up while CPU
stays flat, though they usually move together). Input: the output of `join` with ≥ 2 dims.

| `with` key | Meaning |
|---|---|
| `method` | `mahalanobis`; required; literal |
| `min_points` | default 10 |
| `max_anomalies` | default 20; 0 = all |
| `threshold` | Mahalanobis distance above which a sample is anomalous; default 3 |
| `min_delta` | a number for every dimension or `{cpu: 5, mem: 3}`; a quarter of it is the noise floor added to each dimension's variance |
| `min_rel_delta` | the same as a share of the dimension's mean; the larger applies |

Report series: `{labels, points, skipped, [reason], [noise_floor], stats {mean_<dim>, stddev_<dim>,
corr_<a>_<b>, threshold, max_distance}, anomaly_count, anomalies: [{timestamp, time, values{dim},
expected{dim}, deviation{dim}, dominant, score}]}`. `deviation` is in each metric's own sigmas,
`dominant` the metric that deviates most. A singular covariance (a constant input without `min_delta`)
gives `skipped`. Not robust: many outliers in the window mask each other, so use a window much longer
than the expected anomaly.

`min_delta` here is a noise floor, not a filter: the distance can still flag a joint move where each
metric moved less than its minimum. Keep a post-filter in `response.jq` ("at least one dimension moved
by its minimum") and then `max_anomalies: 0` is justified:

```jq
{cpu: $params.min_delta, mem: $params.min_delta} as $min
| [ .series[] | select(.skipped | not)
    | .anomalies |= map(. as $a | select(any($min | keys[]; (($a.values[.] - $a.expected[.]) | fabs) >= $min[.])))
    | select((.anomalies | length) > 0) ]
```

```yaml
process:
  - fn: join
    with: {inputs: [cpu, mem], join_by: [instance]}
  - fn: anomaly_mv
    with: {method: mahalanobis, threshold: "{{ .threshold }}", min_points: 30, min_delta: "{{ .min_delta }}", max_anomalies: 0}
```

## anomaly_ensemble

Batch detection of anomalous **segments** (start, end, peak) by an ensemble: Matrix Profile (shape
change), AR residual (sudden jump), level shift (a level reached slowly and held), seasonal residual
(with `season`), Isolation Forest (rare state), PCA (broken relation between metrics) and k-dim Matrix
Profile. Thresholds adapt to each series' noise (POT/EVT at `risk` false positives per sample and
detector, not below `z_floor`). Input: `join` output; one dimension is enough.

| `with` key | Meaning |
|---|---|
| `window` | shape length: points or a duration (`1h`); default max(8, n/50); at least 4 points |
| `season`, `step` | season period (enables `seasonal`), grid step; points/seconds or a duration |
| `detectors` | subset of `mp, ar, level, seasonal, iforest, pca, kmp`, a list or a comma-separated string |
| `risk` | POT risk per sample, (0, 1); default 1e-4 (tools usually take 1e-3 as a parameter) |
| `z_floor` | lowest threshold in robust z; default 4 |
| `z_threshold` | > 0: a fixed threshold instead of POT |
| `min_len`, `merge_gap` | shortest segment and the gap that merges two, points or a duration; defaults 1 point and window/16 |
| `ar_lags`, `kmp_dims`, `knn`, `pca_train_fraction` | detector tuning, rarely needed |
| `min_points` | default 20 (use ~120 for 1-minute data) |
| `max_ranges` | strongest segments per series; default 20, 0 = all |
| `top_metrics` | metrics in a segment's attribution; default 5 |
| `min_delta`, `min_rel_delta` | number or `{dim: number}`, and a share of the median; a quarter of the larger is a deterministic noise added to the detectors' input |

Report: top level `{detectors, dims, threshold, series_total, series_analyzed, series_skipped,
total_anomalies, series}`; each series `{labels, points, grid_points, gaps, step, [noise_floor],
baseline{dim} (median), quarters{dim: [4 medians of the window's quarters]}, window, season, min_len,
merge_gap (actual, in points), skipped, [reason], anomaly_count, anomalies, detectors, thresholds,
max_score, [error], metrics{dim: {anomaly_count, anomalies, max_score, …}}}`. A segment is
`{start, start_time, end, end_time, peak, peak_time, points, duration, score, fired_by, values{dim} (or
value), baseline}` plus, for multivariate ones, `kind` (`single`: one metric alone; `correlation`: no
metric alone but their relation broke; `systemic`: several) and `top_metrics: [{metric, score, own_z,
pca_share, mp_share}]`. Header `anomalies` are joint segments (≥ 2 dims); single-signal segments are in
`metrics.<dim>.anomalies`. A series with `error` is analysed but its multivariate part failed: show
it.

With `min_delta`, all detectors stop seeing moves smaller than about one `min_delta` (the reported
`values`/`baseline` stay raw). Still keep a final filter in jq on the effect at the peak
(`|values[dim] − baseline[dim]| >= delta`). The level detector sees only a plateau a dozen MAD or more
from the noise at `risk` 1e-3; a milder one needs `risk: 0.01` around a known moment: say so in the
description. Template: [../templates/multi-metric-ensemble.yaml](../templates/multi-metric-ensemble.yaml).

## cluster_events

Groups events of many objects that overlap in time (with a `gap`) into clusters and counts on how many
objects each signal moved: dozens of segments become lines like `11:05..11:16 (11 min): cpu(8),
disk(7), iowait(6) on 9 VM(s)`. Input: a **list of events** that a preceding `jq` step builds from an
`anomaly_ensemble` (or `anomaly`/`anomaly_mv`) report, deciding there which segments are significant.

```yaml
process:
  - fn: join
    with: {inputs: [cpu, iowait, disk], join_by: [instance]}
  - fn: anomaly_ensemble
    with: {window: "{{ .window }}", risk: "{{ .risk }}", min_delta: {cpu: 5, iowait: 1, disk: 10}}
  - fn: jq
    with:
      expr: |
        {cpu: 5, iowait: 1, disk: 10} as $d
        | [ .series[] | select(.skipped | not) | .labels.instance as $i
            | .anomalies[] | . as $a
            | ([ $a.values | to_entries[] | select(((.value - $a.baseline[.key]) | fabs) >= $d[.key]) | .key ]) as $sig
            | select(($sig | length) > 0)
            | {labels: {instance: $i}, start, end, score, kind, signals: $sig} ]
  - fn: cluster_events
    with:
      gap: "{{ .gap }}"          # seconds or a duration; default 0 (overlap only)
      min_series: 2              # a cluster needs events of ≥ 2 objects (default 2)
      same_signal: false         # true: link only events sharing a signal
      max_clusters: 20           # strongest clusters (by objects), in time order; 0 = all
```

Event: `start`/`end` required (unix seconds or RFC 3339), `labels` identify the object, `signals` list
what moved, `score` a number; other fields pass through. Report: `clusters: [{start, end, start_time,
end_time, duration, series_count, event_count, score, series: [labels…], signals: [{signal, series}],
events}]`, `singles` (events outside clusters), `events_total`, `series_total`, `clusters_total`,
`clustered_events`. The ensemble summary (`skipped`, `errors`) is lost in this chain, because `process`
is linear: mention it in the description or offer a sibling tool without clustering.

## ssa and ssa_mv

Singular Spectrum Analysis: the series is decomposed into components classified as trend, cycle (a
pair with a period), or noise; groups are suggested and checked, and the structure is optionally
forecast. `ssa` takes every series of a matrix separately; `ssa_mv` decomposes the dimensions of a
`join` output together (MSSA: a shared cycle is found once, each component shows its share in every
metric).

```yaml
worker: prometheus
request: {type: range, query: '…', start: "now-7d", end: now, step: 5m}
process:
  - fn: ssa
    with:
      window: "{{ .window }}"     # points or a duration; a multiple of the main period (1d); default n/3
      forecast: "{{ .horizon }}"  # points or a duration; default 0 = no forecast
      method: L                   # L recurrent (default), K vector
      groups: "{{ .groups }}"     # "0;1-2;3,5-7"; empty = suggested groups, the structure is forecast
      clip: [0, 100]              # percentages: groups and forecast with the level stay in range
      min_points: 288             # at least a day of 5m samples
      min_delta: 5                # smallest meaningful stretch away from the usual profile
      min_rel_delta: 0.2
```

| `with` key | Meaning |
|---|---|
| `window` | window L, points or a duration; default n/3; a multiple of the main period; history ≥ 3 periods |
| `step` | grid step; default the smallest interval in the data |
| `k`, `iters` | number of leading components (0 = auto) and power iterations (default 8) |
| `groups` | component groups `"0;1-2;3,5-7"`; empty = suggested |
| `forecast`, `method` | horizon (points or a duration) and `L`/`K` |
| `clip` | `[lo, hi]` or `"lo,hi"`; in `ssa_mv` also `{dim: [lo, hi]}`; applies to groups and forecasts containing component 0 |
| `min_points`, `max_gaps` | minimum points (default 20) and the share of empty grid slots filled linearly (default 0.2) |
| `reconstruction` | `true` adds the reconstructed `points` to every group (n points each) |
| `normalize` (`ssa_mv`) | scale dimensions to one size (default `true`) |
| `min_delta`, `min_rel_delta` | smallest meaningful change: a number in `ssa`, a number or `{dim: number}` in `ssa_mv` (there also the lower bound of the normalization scale) |

Report series (fields per channel are scalars in `ssa`, `{dim: v}` in `ssa_mv`): `labels, points,
grid_points, gaps, step, skipped, [reason], window, decomposition, [scale], noise {floor, sd, phi, …},
components [{group, kind (trend | harmonic | slow cycle | unpaired oscillation | alternating), meaning,
sigma, ratio, borderline, [period, period_seconds], variance_share}], groups [{group, source, kind,
mean, drift, peak_to_peak, min, max, variance_share}], residual {explained, sd, robust_sd, lag1,
spikes}, broken, runs [{kind long|daily|burst, start_time, end_time, duration, deviation}], recurring
[{time_of_day, days, of_days, spread, duration, deviation}], findings [{level ok|info|warn, text}],
forecast {group, source, points [[ts, v…]], start_time, end_time, [left_out], [growing], [warning]} or
{reason}`.

What to show the agent: the structure (`components` without `borderline`, with `meaning`), how much
it explains (`residual.explained`), `findings` with level `warn`, long `runs` (another regime: an
outage or catch-up distorts the trend, periods and forecast; suggest re-running with `at`/`range`
outside it), `recurring` (scheduled daily jobs), and the forecast summarized for the asked interval
(low, peak with time, end value), not 288 points a day. Put objects with `broken` or
`forecast.warning` last. The forecast is a smooth expectation without noise or a confidence band.
Template: [../templates/prometheus-forecast.yaml](../templates/prometheus-forecast.yaml).

## min_delta: the significance threshold

Every detector measures deviation in units of the series' own spread. A nearly constant series has a
spread close to zero, and a move in the third decimal becomes an "anomaly" with a huge score (a
database at 7.9333 tx/s with stddev 0.00002 gets z = 12 at 7.9336). So **every tool with a detector
has a significance threshold in metric units**: `min_delta` (and `min_rel_delta` where levels differ
by orders of magnitude).

1. **The threshold is a shift from the expected value, not a level.** The processor compares
   `|value − expected|` with `max(min_delta, min_rel_delta·|expected|)`. A level filter ("latency ≥
   100 ms") fails both ways: it passes noise on busy objects and hides real spikes on quiet ones.
2. **It goes in the processor's `with`, not in `response.jq`.** It then applies before
   `max_anomalies`, so the cap is spent on significant findings: use `max_anomalies: 3`–`5`, not `0`
   plus a jq cut (`anomaly_mv` is the exception, see above).
3. **The parameter uses units natural to the agent; convert in the template:**

   ```yaml
   params:
     min_ms: {type: number, default: 50, minimum: 0, maximum: 60000, description: "Smallest slowdown worth reporting: ms above the usual latency"}
   process:
     - fn: anomaly
       with:
         method: zscore
         direction: up
         min_delta: "{{ divf .min_ms 1000 }}"       # seconds; MiB → bytes: "{{ mulf .min_mib 1048576 }}"
         max_anomalies: 5
   ```

4. **Multivariate processors take an object per dimension**, items may be templates; an unknown
   dimension name is a call error:
   `min_delta: {iowait: "{{ .min_iowait }}", disk_util: "{{ .min_util }}"}`.
5. **Show how much was cut.** With the filter on, each series has `below_min_delta`:

   ```jq
   raw_total: (.total_anomalies + ([.series[].below_min_delta // 0] | add // 0)),
   total: .total_anomalies,
   ```

   Name the threshold and its meaning in `render` and `description` ("moves under 3 points ignored: 87
   raw"): an empty answer means "nothing larger than the threshold", not "nothing".
6. **The default is the smallest change worth showing a person**, not a statistical constant: see the
   defaults table in [interview.md](interview.md#defaults-when-the-user-says-you-decide). Check on real
   data: call once with 0 and once with the default; `raw_total` against `total` shows the share of
   noise.
