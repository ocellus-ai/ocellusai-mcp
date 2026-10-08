# Processors

[For DevOps](for-devops.md) · [Quick start](quick-start.md) · [User guide](user-guide.md) · [Workers](workers.md) · [Templates and jq](templates-and-jq.md) · **Processors** · [Writing tools](writing-tools.md)

Processors are the analysis part of Ocellus AI. They sit between the data sources and the answer: they reshape data,
find anomalies in time series, find outliers among peers, fold simultaneous events on many hosts into incidents, and
decompose a series into trend and cycles to forecast it. This guide explains what each processor does, how it decides,
what its settings mean, what its report contains and where its limits are.

## Contents

- [How the process stage works](#how-the-process-stage-works)
- [Data shapes](#data-shapes)
- [Choosing a processor](#choosing-a-processor)
- [Concepts shared by the detectors](#concepts-shared-by-the-detectors)
- [jq](#jq)
- [join](#join)
- [anomaly](#anomaly)
- [outliers](#outliers)
- [anomaly_mv](#anomaly_mv)
- [anomaly_ensemble](#anomaly_ensemble)
- [cluster_events](#cluster_events)
- [ssa and ssa_mv](#ssa-and-ssa_mv)
- [Cost](#cost)

## How the process stage works

`process` is an optional list of steps in a tool file. Each step names a processor (`fn`) and gives it settings
(`with`):

```yaml
process:
  - fn: join                                  # several metrics → one multivariate series per VM
    with: {inputs: [cpu, mem], join_by: [instance]}
  - fn: anomaly_ensemble                      # anomalous segments in that series
    with:
      window: 1h
      risk: "{{ .risk }}"                     # a template over the tool's arguments
      min_delta: {cpu: 5, mem: 3}
```

- The first step receives the worker's answer (in a tool with several calls, the object `{call name: answer}`).
  Every next step receives the output of the previous one. The last output goes to `response.jq` and becomes
  `structuredContent`.
- `with` belongs to the processor. Its strings are templates rendered on every call, so settings can come from the
  arguments; numbers may arrive as strings.
- Settings are checked at startup: an unknown key or an invalid literal stops the server with
  `tools/x.yaml: process[1] (anomaly_ensemble): …`. Values that come from templates are checked at call time.
- Errors during a call name the step: `failed at stage process: process[1] (anomaly): …`.

Processors are of two kinds:

- **Transformations** return data of a known shape, so other steps can follow: `jq`, `join`.
- **Analyses** return a report and usually end the chain: `anomaly`, `outliers`, `anomaly_mv`, `anomaly_ensemble`,
  `cluster_events`, `ssa`, `ssa_mv`. A `jq` step can turn a report into the input of another analysis, as tools do
  before `cluster_events`.

## Data shapes

Each analysis accepts exactly one shape of input. Data of another shape is brought to it by a `jq` step before it;
processors have no settings for picking fields out of arbitrary JSON.

**Scalar series** = a Prometheus matrix, what a `range` query returns:

```json
{"resultType": "matrix",
 "result": [{"metric": {"instance": "web-1:9100"}, "values": [[1773316800, "25.1"], [1773316860, "25.4"]]}]}
```

This is also the format of VictoriaMetrics, Thanos, Mimir and Loki metric queries. Values may be numbers or numeric
strings; `NaN` and infinities are dropped. Used by `anomaly`, `join`, `ssa`.

**Multivariate series**: several metrics of one object on common timestamps. `join` builds it:

```json
{"dims": ["cpu", "mem"],
 "series": [
   {"labels": {"instance": "web-1:9100"}, "points": [[1773316800, 25.1, 42.0], [1773316860, 25.4, 41.9]]},
   {"labels": {"instance": "web-3:9100"}, "points": [], "reason": "missing in input(s) cpu"}]}
```

Each point is a timestamp followed by one value per dimension. A series without points can carry a `reason`; the
analyses report it as skipped with that reason. Used by `anomaly_mv`, `anomaly_ensemble`, `ssa_mv`.

**List of objects** without a time axis, for comparing peers:

```json
[{"key": "/data", "value": 97, "labels": {"host": "db-1"}}, {"key": "/var", "value": 41, "labels": {"host": "db-1"}}]
```

Used by `outliers`.

**List of events**, intervals in time:

```json
[{"start": 1773306000, "end": 1773307200, "labels": {"instance": "db-1:9100"}, "signals": ["cpu", "disk"], "score": 33.1}]
```

Used by `cluster_events`.

## Choosing a processor

| The question | Processor | Input |
|---|---|---|
| Which samples are unusual for this object's own history? | `anomaly` | scalar series |
| Which object stands out among its peers right now? | `outliers` | list of objects |
| Where did the usual relation between metrics break, sample by sample? | `anomaly_mv` | multivariate series, ≥ 2 metrics |
| Which time segments were anomalous, how, and which metric is to blame? | `anomaly_ensemble` | multivariate series, ≥ 1 metric |
| Which events on many objects happened together? | `cluster_events` | list of events |
| What are the trend, cycles and noise, and what comes next? | `ssa` | scalar series |
| The same for several metrics of an object together | `ssa_mv` | multivariate series |
| Stitch several metrics into one series per object | `join` | results of several range calls |
| Reshape anything into the shape the next step needs | `jq` | any JSON |

`anomaly` and `anomaly_mv` judge single samples and are cheap. `anomaly_ensemble` finds segments, tells kinds of
anomalies apart and attributes them, at a higher cost. `ssa` describes what is normal and predicts it.

## Concepts shared by the detectors

### Scores in units of the series' own spread

Every detector compares a value with what is usual **for that object** and measures the difference in units of the
object's own variability. A score of 5 means "five times the usual spread away". A quiet host and a busy one are
judged on their own scale.

The robust statistics used throughout:

| Statistic | Meaning |
|---|---|
| median | the middle value; not moved by a few extreme ones |
| MAD | median absolute deviation from the median; a robust spread |
| robust z | (value − median) / (1.4826 · MAD): like a z-score, but outliers do not inflate the scale |
| quartiles, IQR | q1, q3 and their distance; the middle half of the data |

### The noise floor: `min_delta`

Scaling by the series' own spread has one failure: a nearly constant series has almost no spread, so a tiny move
gets a huge score. A database at 7.9333 transactions per second with a standard deviation of 0.00002 gets z = 12 at
7.9336; iowait of an idle VM going from 0.1% to 1% is "ten times" larger. Statistically these are anomalies; for a
person they are noise.

Every detecting processor therefore accepts the smallest change worth reporting, in the metric's own units:

| Key | Meaning |
|---|---|
| `min_delta` | an absolute amount: 5 percentage points, 50 ms, 1000 messages |
| `min_rel_delta` | a share of the usual level: 0.2 = 20% |

The larger of the two applies. How each processor uses it:

| Processor | Effect |
|---|---|
| `anomaly`, `outliers` | a finding is kept only if `|value − expected| ≥ max(min_delta, min_rel_delta·|expected|)`; the count of removed ones is reported as `below_min_delta` |
| `anomaly_mv` | a quarter of the minimum is assumed as the noise of each metric (added to the covariance), so a flat metric cannot produce a large distance |
| `anomaly_ensemble` | a quarter of the minimum is added to the detectors' input as noise, so no detector can see moves much smaller than the minimum; the report keeps the raw values |
| `ssa`, `ssa_mv` | a stretch away from the usual daily profile must exceed it; in `ssa_mv` it also bounds the normalisation scale |

Choose it as "the smallest change I would want to be told about": CPU 5 points, memory 3 points, latency 50 ms, Kafka
lag 1000 messages. The bundled tools expose it as a parameter in natural units. An empty answer then means "nothing
larger than the minimum", and the tools say how many smaller findings were ignored.

### Skipped series

An analysis never fails because one object cannot be analysed. Such a series is reported with `skipped: true` and a
`reason`: too few points, too many gaps, a series that `join` could not assemble, a singular covariance. Counts of
analysed and skipped series are part of every report; the bundled tools print them.

### Points or durations

Lengths in settings (`window`, `min_len`, `merge_gap`, `season`, `forecast`) accept a number of points or a
Prometheus duration such as `1h` or `1d4h`, which is converted with the series' step. Processors that work on a
regular grid (`anomaly_ensemble`, `ssa`, `ssa_mv`) infer the step as the smallest interval between samples unless
`step` is given; empty grid slots are gaps.

## jq

A step that runs one jq expression over the data, anywhere in the chain.

```yaml
  - fn: jq
    with:
      expr: '[.result[] | {key: .metric.pod, value: .value[1]}]'
```

| Key | Meaning |
|---|---|
| `expr` | the jq expression; required. A literal, not a template: the tool's arguments are available as `$params`. |

The output follows the rules of `response.jq`: no output is `null`, one is itself, several form an array. Typical
uses: pick one call's result out of `{name: result}`; build the list for `outliers` from a vector, command output or
an API answer; flatten a report into events for `cluster_events`. See [Templates and jq](templates-and-jq.md#jq).

## join

Stitches the results of several range queries into one multivariate series per object.

```yaml
calls:
  - {name: cpu, worker: prometheus, request: {type: range, query: "…", start: "…", end: "…", step: 60s}}
  - {name: mem, worker: prometheus, request: {type: range, query: "…", start: "…", end: "…", step: 60s}}
process:
  - fn: join
    with:
      inputs: [cpu, mem]         # call names; become the dimensions, in this order
      join_by: [instance]        # labels that identify the object across inputs
```

| Key | Meaning |
|---|---|
| `inputs` | names of calls (or keys of the input object); required, at least one |
| `join_by` | labels that identify an object in every input; required |

How it stitches:

- Every input must have **one series per object**. Two series for the same `instance` (for example after a label
  changed inside the window) is an error that suggests aggregating in PromQL, `max by (instance) (…)`.
- Samples are matched by **exact timestamp**, so all calls need the same `start`, `end` and `step`. Timestamps
  missing in one input are dropped from the joined series.
- Nothing is lost silently. A series without a `join_by` label, and an object missing from some input, come out as
  series without points and with a `reason` (`input mem: missing join_by label(s) instance`,
  `missing in input(s) cpu`). Every series also carries `input_points`, the number of samples each input had.
- The output `labels` hold only the `join_by` labels.

One input is allowed: it turns a matrix into a multivariate series with one dimension, which `anomaly_ensemble` and
`ssa_mv` accept.

## anomaly

Point anomalies in each series of a range query, each series judged against its own history in the window.

```yaml
request: {type: range, query: "…", start: "now-6h", end: now, step: 60s}
process:
  - fn: anomaly
    with:
      method: iqr
      k: 3
      direction: up
      min_delta: 5
      max_anomalies: 5
```

### Methods

**`zscore`**: the mean and the standard deviation of the series; a sample is anomalous when
`|value − mean| / stddev > threshold`. The score is `|z|`, `expected` is the mean. A series without spread has no
anomalies. Simple, but the outliers themselves inflate the standard deviation, so a few large spikes can hide each
other.

**`iqr`** (Tukey's fences): the quartiles q1 and q3 and `IQR = q3 − q1`; a sample is anomalous outside
`[q1 − k·IQR, q3 + k·IQR]`. The score is the distance beyond the fence in units of IQR (in metric units when
IQR = 0), `expected` is the median. Robust to the outliers themselves; the better default.

### Settings

| Key | Default | Meaning |
|---|---|---|
| `method` | required | `zscore` or `iqr`; written literally |
| `threshold` | 3 | `zscore`: the \|z\| above which a sample is anomalous |
| `k` | 1.5 | `iqr`: the fence multiplier; 1.5 = ordinary outliers, 3 = extreme ones |
| `direction` | `both` | `up`, `down` or `both`: which side to report |
| `min_points` | 10 | shorter series are skipped |
| `max_anomalies` | 20 | the strongest anomalies kept per series, 0 = all |
| `min_delta`, `min_rel_delta` | 0 | the [noise floor](#the-noise-floor-min_delta) |

### Report

```json
{
  "method": "iqr",
  "series_total": 2, "series_analyzed": 1, "series_skipped": 1, "total_anomalies": 1,
  "series": [
    {"metric": {"pod": "api-1"}, "points": 120, "skipped": false, "anomaly_count": 1, "below_min_delta": 4,
     "stats": {"q1": 0.38, "median": 0.41, "q3": 0.45, "iqr": 0.07, "lower": 0.27, "upper": 0.56, "k": 1.5, "min": 0.35, "max": 1.9},
     "anomalies": [
       {"timestamp": 1773316800, "time": "2026-03-12T12:00:00Z", "value": 1.9, "score": 19.1, "expected": 0.41, "direction": "up"}
     ]},
    {"metric": {"pod": "job-x"}, "points": 3, "skipped": true, "reason": "fewer than 10 points (min_points)",
     "anomaly_count": 0, "anomalies": []}
  ]
}
```

- `stats` are the method's model (`mean`, `stddev`, `threshold` for `zscore`) plus `min` and `max`.
- `anomaly_count` counts all anomalies above the noise floor; `anomalies` lists the strongest `max_anomalies` of them
  in time order.
- `below_min_delta` appears when a noise floor is set: how many anomalies it removed.

### Reading it

`node_cpu_anomalies` over six hours, with `sensitivity: 1.5` (the IQR `k`), on three VMs one of which had a CPU spike
of about 20 points for twenty minutes:

```text
7 anomalous CPU sample(s) on 1 of 3 VM(s) over the 6h ending at 2026-03-12T12:00:00Z (IQR k=1.5, direction both, deviations under 5 points ignored: 7 raw; 0 skipped). Top 1:
web-1:9100 (web-1:9100): 7 anomaly(ies), typical 28.6% busy, fences 9.7..48.3%
  - 2026-03-12T09:33:00Z up to 49.7% (score 0.2)
  - 2026-03-12T09:40:00Z up to 49.4% (score 0.1)
  - 2026-03-12T09:46:00Z up to 50.2% (score 0.2)
```

The fences are wide (9.7–48.3%) because the window holds the morning rise of the daily cycle: the detector sees all
six hours as one distribution, so only samples above the whole morning range count, and only barely (scores of 0.1–0.2
IQR beyond the fence). With the default `k: 3` the same call reports nothing.

### Limits

- **The window is one distribution.** Trends and daily cycles inside the window widen the spread and hide
  anomalies. Use windows where the metric is roughly level, or use `anomaly_ensemble` or `ssa`, which model shape and
  cycles.
- **Single samples.** A long plateau is a run of anomalous samples, not one event; `anomaly_ensemble` reports
  segments.
- **Enough points.** Tens to hundreds of samples per series: two hours at 60 s is 120.

## outliers

Outliers in a set of objects with no time axis: each object is compared with the **other objects** of its group, not
with its own history. Filesystems by fill level, pods by memory or restarts, backends by error rate, rows of an API
answer.

```yaml
request: {type: instant, query: 'sum by (cluster, namespace, pod) (container_memory_working_set_bytes{container!=""})'}
process:
  - fn: jq                                       # vector → [{key, value, labels}]
    with: {expr: '[.result[] | {key: .metric.pod, value: .value[1], labels: {cluster: .metric.cluster, namespace: .metric.namespace}}]'}
  - fn: outliers
    with: {method: iqr, k: 1.5, group_by: [cluster, namespace], direction: up, min_points: 5, min_delta: 104857600}
```

The input is a list of `{key, value, labels}`. `value` is required (a number or a numeric string); `key` names the
object in the report; `labels` are needed only for `group_by`. Other fields are ignored.

The detectors and their keys are those of [`anomaly`](#anomaly). The other settings:

| Key | Default | Meaning |
|---|---|---|
| `group_by` | none | a label or list of labels; objects with equal values form one group, compared among themselves. Without it, all objects form one group. |
| `min_points` | 5 | smaller groups are skipped |
| `direction`, `max_anomalies`, `min_delta`, `min_rel_delta` | as for `anomaly` | |

The report is like that of `anomaly` with three differences: a series is a group, with `labels` (only the `group_by`
labels) instead of `metric`; an outlier has `index` (its position in the input) and `key` instead of a time; outliers
are sorted **strongest first**.

```json
{"method": "iqr", "group_by": [], "series_total": 1, "series_analyzed": 1, "series_skipped": 0, "total_anomalies": 1,
 "series": [{"labels": {}, "points": 8, "skipped": false, "anomaly_count": 1,
             "stats": {"q1": 39.75, "median": 40.5, "q3": 42.25, "iqr": 2.5, "lower": 36, "upper": 46, "k": 1.5, "min": 38, "max": 97},
             "anomalies": [{"index": 3, "key": "/data", "value": 97, "score": 20.4, "expected": 40.5, "direction": "up"}]}]}
```

**Small groups need `iqr`.** In a group of `n` objects no z-score can exceed `(n − 1)/√n`: 2.04 for six objects, 2.85
for ten. A `zscore` threshold of 3 never fires on a handful of objects. `iqr` has no such bound.

**When most values are equal** (most pods never restart), the quartiles collapse, IQR is 0, and every object above
the common value is an outlier, scored by its distance in metric units. `min_delta` then states which distance is
worth reporting.

## anomaly_mv

Every sample judged on several metrics at once. It catches what single-metric detectors cannot: memory rising while
CPU stays flat, though the two normally move together; disk latency jumping without the utilisation that usually
explains it.

```yaml
process:
  - fn: join
    with: {inputs: [cpu, mem], join_by: [instance]}
  - fn: anomaly_mv
    with: {method: mahalanobis, threshold: 4, min_points: 30, min_delta: {cpu: 5, mem: 3}, max_anomalies: 0}
```

### Method

**`mahalanobis`**: for each object, the mean vector μ and the covariance matrix Σ of its metrics over the window.
The score of a sample x is the Mahalanobis distance

```
d(x) = sqrt( (x − μ)ᵀ Σ⁻¹ (x − μ) )
```

the distance from the usual state in standard deviations, taking the correlation between the metrics into account.
Where two metrics are strongly correlated, a sample that breaks the correlation is far even if each value alone is
ordinary. A sample with `d > threshold` is anomalous. For each one the report gives `expected` (the means),
`deviation` (each metric's deviation in its own standard deviations, with sign) and `dominant` (the metric that
deviates most).

### The noise floor

With `min_delta` (a number for all metrics or `{cpu: 5, mem: 3}`) and `min_rel_delta` (a share of each metric's
mean), a quarter of the minimum is added to each metric's variance: `Σ + diag((unit/4)²)`. This is exactly the
covariance of independent noise of that size. A metric with its own noise larger than that is barely affected; a
nearly flat one is judged as if it had at least that noise, so a tiny wiggle no longer produces a large distance.
A metric that is constant in the window no longer makes Σ singular. The floor is reported per series as
`noise_floor`.

The floor is not a filter: two metrics that each moved less than their minimum can still break their correlation
strongly enough to cross the threshold. Tools that want "at least one metric moved by its minimum" add that test in
`response.jq` (the bundled `node_cpu_mem_mv` and `node_disk_pressure_mv` do).

### Settings

| Key | Default | Meaning |
|---|---|---|
| `method` | required | `mahalanobis` |
| `threshold` | 3 | the distance above which a sample is anomalous |
| `min_points` | 10 | shorter series are skipped |
| `max_anomalies` | 20 | the strongest anomalies kept per series, 0 = all |
| `min_delta`, `min_rel_delta` | 0 | the noise floor, per metric |

### Report

```json
{
  "method": "mahalanobis", "dims": ["cpu", "mem"],
  "series_total": 2, "series_analyzed": 1, "series_skipped": 1, "total_anomalies": 1,
  "series": [
    {"labels": {"instance": "web-2:9100"}, "points": 360, "skipped": false, "anomaly_count": 1,
     "noise_floor": {"cpu": 1.25, "mem": 0.75},
     "stats": {"mean_cpu": 28.4, "stddev_cpu": 4.1, "mean_mem": 45.0, "stddev_mem": 8.0, "corr_cpu_mem": -0.04, "threshold": 3, "max_distance": 3.4},
     "anomalies": [
       {"timestamp": 1773303840, "time": "2026-03-12T08:24:00Z", "values": {"cpu": 25.8, "mem": 72.4},
        "expected": {"cpu": 28.4, "mem": 45.0}, "deviation": {"cpu": -0.6, "mem": 3.4}, "dominant": "mem", "score": 3.4}
     ]},
    {"labels": {"instance": "web-3:9100"}, "points": 0, "skipped": true, "reason": "missing in input(s) cpu", "anomaly_count": 0, "anomalies": []}
  ]
}
```

### Limits

The method is classical, not robust: the anomaly itself enters the mean and the covariance. Here is
`node_cpu_mem_mv` with `threshold: 3` over six hours, where web-2's memory jumped by 29 points for half an hour:

```text
web-2:9100: 31 anomaly(ies), cpu/mem correlation -0.04, max distance 3.4
  - 2026-03-12T08:24:00Z cpu 25.8% (expected 28.4), mem 72.4% (expected 45.0), driven by mem (+3.4 sigma), score 3.4
```

Thirty minutes out of six hours are enough to inflate memory's standard deviation, so a jump of 29 points scores
only 3.4, and at the tool's default threshold of 4 it would not be reported at all. Use a window much longer than the
anomalies you look for, or `anomaly_ensemble`, which finds the same jump with a score of about 21.

## anomaly_ensemble

Finds anomalous **segments** (start, end, peak) in one metric or several metrics of each object, with an ensemble of
detectors that look for different kinds of anomalies, thresholds adapted to each series, and attribution of each
segment to the metrics behind it.

```yaml
process:
  - fn: join
    with: {inputs: [cpu, mem], join_by: [instance]}
  - fn: anomaly_ensemble
    with:
      window: 1h               # length of a "shape" to compare
      risk: 0.001              # expected false positives per sample and detector
      min_len: 2m              # shortest segment to report
      min_points: 120
      max_ranges: 20
      min_delta: {cpu: 5, mem: 3}
      min_rel_delta: 0.2
```

The input is a multivariate series from `join`; one dimension is enough (then only the single-metric detectors run).

### The grid

Each series is laid on a regular grid: the step is the smallest interval between samples (or `step`), each sample goes
to its nearest slot, and empty slots are gaps. Gaps are interpolated for the detectors and masked, so a hole in the
data is neither an anomaly nor an "unusually predictable" stretch. A series whose samples do not fit a grid, or that
has fewer than `min_points` samples, is skipped.

### The detectors

| Name | Detector | Catches | Runs on |
|---|---|---|---|
| `mp` | **Matrix Profile** (STOMP): for each window of `window` points, the distance to its nearest look-alike anywhere in the series (two nearest neighbours, so one repeat of an incident does not hide it) | shapes unlike anything else in the window: a changed pattern, an unusual wave | each metric |
| `ar` | **Autoregressive residual**: a linear model predicts each point from the previous ones (lags 1, 2, 3), fitted twice so that large anomalies do not pull the model; the score is the robust z of the prediction error | spikes, drops, sudden jumps | each metric |
| `level` | **Level shift**: the distance of each point from the median of the preceding `window` real samples, as a robust z | a level the series moved to slowly and held, which the AR residual misses (every step is small) and the Matrix Profile dilutes; also the return to the old level | each metric |
| `seasonal` | **Seasonal residual**: the distance from the median of the same phase in previous cycles; only with `season` | a value that is normal, but not at this time of day; changed amplitude | each metric |
| `iforest` | **Isolation Forest**: how easily random splits isolate a point; on short lag windows per metric and on the vector of all metrics | rare states and combinations | each metric and the vector |
| `pca` | **PCA** on robustly scaled metrics, keeping the components that explain 90% of the variance: the residual outside them (SPE) and the distance inside them (Hotelling T²) | a broken correlation between metrics, and "everything rose together" | the vector |
| `kmp` | **Multidimensional Matrix Profile** (mSTOMP) | shapes that are unusual across metrics | the vector |

By default every detector runs except `seasonal`, which needs `season`. `detectors` selects a subset. A detector that
cannot run on a series drops out of that series quietly (the Matrix Profile needs a series longer than two windows);
the report lists the detectors that actually ran.

### From scores to segments

1. **Common scale.** Each detector's scores become robust z-scores (median and MAD of the series; detectors that
   already produce robust z are left as they are). For each point the ensemble takes the **maximum** over detectors.
2. **A threshold per detector.** Instead of one global cut-off, each detector gets its own threshold from **extreme
   value theory** (Peaks Over Threshold): a generalised Pareto distribution is fitted to the scores above their 98th
   percentile, and the threshold is the level exceeded with probability `risk` per sample. The fit is repeated
   without the points above the previous threshold, so that incidents in the batch do not raise the threshold that
   should find them. The threshold is never below `z_floor` (4 by default) and never above three times the level
   a Gaussian tail of the same width would reach at `risk`. The Matrix Profile, whose scores are spread over a whole
   window, uses `z_floor` only. With `z_threshold > 0` a fixed threshold on the combined score replaces all of this.
3. **Segments.** A point is flagged when any detector exceeds its threshold. Flagged points closer than `merge_gap`
   join one segment; segments shorter than `min_len` are dropped. Each segment gets its `peak` (where the point-wise
   detectors scored highest), its `score` (the highest combined z inside), `fired_by` (the detectors that crossed
   their thresholds inside it), the raw `values` at the peak and the `baseline` (each metric's median over the
   series), so it reads in the metric's units: "memory 71% at the peak, usually 42".

`risk` is the main sensitivity control. At `1e-4` (the processor's default) a detector expects about one false sample
in ten thousand; the bundled tools use `1e-3`; `1e-2` is for looking closely around a known moment.

### Two layers: joint and per metric

The ensemble runs twice on a multi-metric series:

- **per metric**, with the single-metric detectors: `metrics.<dim>.anomalies`;
- **jointly**, with `pca`, `iforest` on the vector and `kmp`: the series' top-level `anomalies`.

Both layers matter. With many metrics the joint thresholds rise (they adapt to the combined residual), and an event of
one metric may appear only in its own layer. The bundled `node_deep_ensemble` shows joint segments and the
single-metric segments that no joint segment covers.

### Attribution

Each joint segment says which metrics drove it:

- `top_metrics`: for each metric, `own_z` (its own single-metric score in the segment, usually the root cause),
  `pca_share` (its share of the PCA residual: it broke its relation with the others), `mp_share` (its share of the
  multidimensional shape distance) and a combined `score`. Metrics with `own_z > 4` come first, ordered by `own_z`.
- `kind`:
  - `single`: exactly one metric is anomalous on its own (`own_z > 4`), or the first metric scores more than twice the
    second;
  - `correlation`: no metric is anomalous on its own, but their relation broke;
  - `systemic`: several metrics are anomalous and none dominates.

With two metrics the PCA shares are close to one half each almost always; rely on `own_z`.

### The noise floor

With `min_delta` (a number or `{dim: number}`) and `min_rel_delta` (a share of each metric's median), Gaussian noise
with σ = a quarter of the minimum is added to the input of the detectors. Where the series' own noise is larger,
this changes almost nothing; where the series is nearly flat, the minimum becomes the yardstick, and with thresholds
around 4 the smallest change a detector notices is about one minimum. Noise is used rather than a formula because
the Isolation Forest and the Matrix Profile have no scale that a formula could prop up. It is deterministic (seeded by
the series' labels and metric name), so the same data gives the same report, and it never reaches the report:
`values`, `baseline` and `quarters` are raw, and `noise_floor` shows the σ per metric. On real fleets this removes
almost all statistically rare but meaningless segments without losing short strong events.

### Settings

| Key | Default | Meaning |
|---|---|---|
| `window` | max(8, n/50) points | the length of a shape: points or a duration. Events much shorter than a quarter of it are diluted. At least 4 points. |
| `season` | none | the length of a season (`1d`): enables `seasonal` and gives the Isolation Forest the phase |
| `step` | inferred | the grid step: seconds or a duration |
| `detectors` | all | a list or a comma-separated string of `mp, ar, level, seasonal, iforest, pca, kmp` |
| `risk` | 0.0001 | false positives per sample and detector for the POT thresholds, in (0, 1) |
| `z_floor` | 4 | the lowest threshold, in robust z; 0 = none |
| `z_threshold` | 0 | > 0: one fixed threshold on the combined score instead of POT |
| `min_len` | 1 point | the shortest segment: points or a duration |
| `merge_gap` | window/16 | segments closer than this merge: points or a duration |
| `ar_lags` | `[1, 2, 3]` | lags of the AR model; add the number of points in a day for a daily lag |
| `kmp_dims` | -1 | mSTOMP: -1 = the worst metric decides (an event in one metric of ten stays visible), 0 = the average of all, k = the k best |
| `knn` | 2 | neighbours in the Matrix Profile |
| `pca_train_fraction` | 1 | the share of the series, from its start, that PCA learns from |
| `min_points` | 20 | shorter series are skipped; use about 120 for one-minute data |
| `max_ranges` | 20 | the strongest segments kept per series and per metric, 0 = all |
| `top_metrics` | 5 | metrics listed in a segment's attribution |
| `min_delta`, `min_rel_delta` | 0 | the noise floor |

### Report

Top level: `detectors` (configured), `dims`, `threshold` (`{mode: pot, risk, z_floor}` or `{mode: fixed, z}`),
`series_total`, `series_analyzed`, `series_skipped`, `total_anomalies`, `series`.

Each series:

| Field | Meaning |
|---|---|
| `labels`, `points`, `grid_points`, `gaps`, `step` | identity, input samples, grid size, empty slots, step in seconds |
| `baseline` | each metric's median over the series |
| `quarters` | each metric's median in the four consecutive quarters of the window: a level shift ("255 Mbit/s, then 85") is visible without another call |
| `noise_floor` | with `min_delta`: the σ of the added noise per metric |
| `window`, `season`, `min_len`, `merge_gap` | the values actually used, in points |
| `skipped`, `reason` | |
| `anomaly_count`, `anomalies` | joint segments (with one metric: that metric's segments) |
| `detectors`, `thresholds`, `max_score` | the joint ensemble: detectors that ran (`pca(k=0)`, `kmp(w=60,k=-1)`), their thresholds, the highest score |
| `metrics.<dim>` | the same per metric: `anomaly_count`, `anomalies`, `detectors`, `thresholds`, `max_score` |
| `error` | the joint ensemble failed on this series (every joint detector dropped out); the per-metric layer is still there |

A segment: `start`, `start_time`, `end`, `end_time`, `peak`, `peak_time`, `points`, `duration` (seconds), `score`,
`fired_by`, `values` (or `value` in a per-metric segment), `baseline`, and for joint segments `kind` and
`top_metrics`.

### Reading it

`node_deep_ensemble` analyses ten signals per VM. On one day of three VMs, with a nightly job on db-1, a noisy
neighbour on web-1, a memory jump on web-2 and a backup that pushed traffic out of all three at 10:15:

```text
web-1:9100: 1 significant joint segment(s), 5 single-signal, 0 level shift(s), max change 14.0x the minimum, max joint score 90
  - 2026-03-12T09:30:00Z .. 2026-03-12T09:51:00Z (22 samples, single, change 14.0x, score 90; fired by pca(k=0), iforest(w=1); moved: cpu, load, steal): steal 14.1% (usually 0.1, own z 52.1, pca 0.00, shape 0.10); cpu 49.7% (usually 25.1, own z 10.2, pca 0.13, shape 0.10); load 0.7/core (usually 0.3, own z 7.2, pca 0.49, shape 0.10)
  - 2026-03-12T10:15:00Z .. 2026-03-12T10:33:00Z (19 samples, net_tx only, change 12.3x, z 26.2; fired by ar(lags=[1 2 3]), level(w=60), iforest(w=4)): net_tx 138.7 Mbit/s (usually 40.0)
web-2:9100: 1 significant joint segment(s), 3 single-signal, 0 level shift(s), max change 12.3x the minimum, max joint score 21.6
  - 2026-03-12T08:10:00Z .. 2026-03-12T08:40:00Z (31 samples, single, change 3.5x, score 21.6; fired by pca(k=0); moved: mem): mem 71.3% (usually 42.0, own z 14.7, pca 0.02, shape 0.10)
db-1:9100: 1 significant joint segment(s), 6 single-signal, 0 level shift(s), max change 12.2x the minimum, max joint score 33.1
  - 2026-03-12T02:00:00Z .. 2026-03-12T02:19:00Z (20 samples, systemic, change 4.3x, score 33.1; fired by pca(k=0), iforest(w=1); moved: cpu, disk, iowait, load, net_rx, net_tx, tcp): cpu 39.3% (usually 17.6, own z 16.5, …); disk 54.7% (usually 13.1, own z 18.3, …); iowait 4.5% (usually 0.6, own z 15, …)
```

How to read it:

- **web-1, 09:30–09:51, `single`, steal first.** Steal (CPU time taken by the hypervisor) went from 0.1% to 14% with
  an own z of 52, and CPU and load rose with it. The VM did not get busier on its own: a neighbour on the same host
  took its CPU.
- **web-2, 08:10–08:40, `single`, memory only.** Memory jumped from 42% to 71% while nothing else moved.
- **db-1, 02:00–02:19, `systemic`.** CPU, disk and iowait rose together, each strongly on its own: a job, not a
  fault of one resource. `node_ssa` on the same VM confirms it recurs every night (below).
- **10:15, `net_tx only`, on every VM.** A single-signal segment that no joint segment covers. That it happened
  everywhere at once is what `cluster_events` is for (below).
- **"change 14.0x the minimum"** is the size of the move at the peak in units of the signal's `min_delta`; the tool
  ranks VMs by it rather than by score, because scores of different signals and VMs are on different scales.

### Tuning

| You want | Change |
|---|---|
| fewer, stronger segments | lower `risk` (1e-4), raise `min_delta` |
| subtler events around a known moment | `risk: 0.01` over a short range around it |
| short events (a few minutes) | a shorter `window` (events shorter than a quarter of it are diluted) and `min_len` of a few points |
| longer history | raise `step` (5m over a week), then zoom in with `step: 60s` |
| daily patterns not to be flagged | `season: 1d` with at least two days of history |
| only shape changes, or only jumps | `detectors: mp,kmp` or `detectors: ar,level` |

### Limits

- **The level detector's blind zone.** A level shift is a plateau of equal scores, and the POT fit puts the threshold
  above a modest plateau. At the default `risk` only levels a dozen MAD or more away from the usual noise are flagged
  (CPU at 99.9% when it is usually 8%). A plateau of 4 to 11 MAD needs `risk: 0.01`, so a plateau missing from the
  report is not proof that there was none.
- **It learns from the batch it searches.** An incident that fills a large part of the window partly masks itself;
  the two-pass fits and robust scales soften this but do not remove it.
- **Cost.** The Matrix Profile is quadratic in the number of points: a day at one minute (1440 points) is cheap,
  tens of thousands of points per series take seconds each. Raise `step` for long ranges.
- **Scores are relative.** A score says how unusual something is for that series, not how big it is. Compare objects
  by the change in units (`values` against `baseline`).

## cluster_events

Folds events on many objects that overlap in time into **clusters** and counts on how many objects each signal moved.
Dozens of segments on a fleet become a few lines: "10:15–10:32, net_tx on 3 VMs".

The input is a list of events, built by a `jq` step from an analysis report. That step also decides which segments
count, for example only those where some signal moved by its minimum:

```yaml
process:
  - fn: join
    with: {inputs: [cpu, iowait, disk], join_by: [instance]}
  - fn: anomaly_ensemble
    with: {window: 1h, risk: 0.001, min_delta: {cpu: 5, iowait: 1, disk: 10}}
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
    with: {gap: 5m, min_series: 2}
```

An event is `{start, end, labels?, signals?, score?}`. `start` and `end` are unix seconds or RFC 3339 times; `labels`
identify the object (events without labels belong to one anonymous object); `signals` name what moved; other fields
pass through to the report.

### How it clusters

Events are sorted by start. Two events are linked when they overlap or are at most `gap` apart; with `same_signal`,
only when they also share a signal. Linking is transitive: if A overlaps B and B overlaps C, all three are one cluster
even if A and C do not overlap. A connected group with events of at least `min_series` different objects is a
cluster; the rest are reported one by one as `singles`.

| Key | Default | Meaning |
|---|---|---|
| `gap` | 0 | events this close are linked: seconds or a duration (`5m`) |
| `min_series` | 2 | a cluster needs events of at least this many objects |
| `same_signal` | false | link only events that share a signal |
| `max_clusters` | 20 | the largest clusters kept (by objects, then events), in time order; 0 = all |

### Report

`clusters`: `[{start, end, start_time, end_time, duration, series_count, event_count, score, series: [labels…],
signals: [{signal, series}], events: […]}]`, in time order; a cluster spans from its earliest start to its latest
end, `score` is the highest event score, `signals` count the objects each signal moved on. `singles`: events outside
clusters. Counters: `events_total`, `series_total`, `clusters_total`, `clustered_events` (before the
`max_clusters` cut).

### Reading it

`node_cluster_events` on the same three VMs:

```text
1 cluster(s) covering 5 of 12 significant event(s) on 3 VM(s) matching .* over the 12h ending at 2026-03-12T12:00:00Z; events link within 5m, a cluster needs 2+ VMs (window 1h, risk 0.001, min change x1 or 0.2 of the usual level). Times are UTC; a signal's count is the number of VMs it moved on.
- 03-12 10:15..10:32 (17 min): net_tx(3), cpu(2), net_rx(2), tcp(2) on 3 VM(s) (db-1, web-1, web-2); 5 event(s) signal=3 single=2, max change 18.1x the minimum, score 33
    web-2 10:15..10:32 (signal, change 18.1x): net_tx 140.8 Mbit/s (usually 30.5)
    web-1 10:15..10:30 (single, change 18.1x): cpu 40.5% (usually 19.4), net_rx 82.4 Mbit/s (usually 46.0), net_tx 140.0 Mbit/s (usually 30.3), tcp 355.2 conn (usually 260.7)
    db-1 10:15..10:32 (signal, change 17.8x): net_tx 138.1 Mbit/s (usually 30.3)
Single events outside clusters, by VM (7):
- db-1: 4 event(s); 03-12 02:00..02:19 cpu 39.4% (usually 12.3), disk 55.3% (usually 11.3), …
- web-1: 2 event(s); 03-12 09:30..09:51 … steal 14.1% (usually 0.1) …
- web-2: 1 event(s); 03-12 08:10..08:34 mem 70.9% (usually 41.1)
```

The backup at 10:15 is one cluster with net_tx on all three VMs: a fleet-wide event, not three problems. The noisy
neighbour, the memory jump and the nightly job stay single events of their VMs.

The summary of the ensemble (skipped VMs, failures) is not available after `cluster_events`, because each step sees
only the output of the previous one. The bundled tool points to `node_deep_ensemble` for it.

## ssa and ssa_mv

Singular Spectrum Analysis decomposes a series into a **trend**, **cycles** and **noise**, says how much of the
movement the structure explains, finds stretches where the series left its usual daily profile, and forecasts the
structure. `ssa` analyses each series of a range query on its own. `ssa_mv` (multivariate SSA) analyses several metrics
of one object together: a cycle they share is found once, and each component reports its share in every metric.

```yaml
request: {type: range, query: "…", start: "now-4d", end: now, step: 5m}
process:
  - fn: ssa
    with:
      window: 1d             # a multiple of the main period
      forecast: 1d           # horizon; 0 = no forecast
      clip: [0, 100]         # percentages stay in range
      min_points: 288
      min_delta: 5
      min_rel_delta: 0.2
```

### SSA in brief

1. **Embedding.** The series of length `n` is cut into all overlapping windows of length `L` (the `window`), which
   form the columns of a matrix of size L × (n − L + 1).
2. **Decomposition.** The matrix is split into components ordered by strength σ (a singular value decomposition).
   Each component is a pattern that the windows share.
3. **Grouping.** Components that belong together are grouped: a slowly varying component is the trend; a pair of
   components with similar σ and rotating patterns is one oscillation with a period; harmonics `P/2`, `P/3` of a
   period `P` together form one non-sinusoidal cycle; the weak tail is noise.
4. **Reconstruction.** Each group is turned back into a series. The trend group plus the cycle groups is the
   structure; what remains is the residual.

The window decides what can be separated: a cycle is separated cleanly when `L` is a multiple of its period, and the
history should hold at least three periods. For a daily cycle at 5-minute resolution: `window: 1d` (288 points) and
four days of history.

For a short side of the matrix up to 300 points the full decomposition is computed; above that, the 30 strongest
components (randomised iterations, with an error estimate per component). `k` and `iters` override this.

### What the analysis adds

The processors add an interpretation on top of the decomposition:

- **A noise model.** The residual after the leading structure is modelled as autoregressive noise, and simulations of
  that noise give the strongest σ that noise alone would produce for this `n` and `L` (the noise floor). A component
  with `σ ≥ 1.5 ×` the floor is structure; 1.2–1.5× is borderline; below is noise. When the residual has heavy tails
  (bursts on an idle host), the floor is based on its ordinary standard deviation, so bursts are not mistaken for
  structure ("heavy-tailed noise" finding).
- **Component types**: `trend`, `harmonic` (a pair, with its period), a family of harmonics of one cycle, `slow cycle`,
  `unpaired oscillation`, `alternating`. Periods are estimated with ESPRIT.
- **Groups.** Without `groups`, the processor proposes them (trend, one group per cycle family, the rest one by one)
  and checks them: what each group is (level and drift of a trend, peak-to-peak of a cycle), how well groups are
  separated (w-correlation), what remains in the residual and how much the structure explains.
- **Findings**, each with numbers and a conclusion: the window is not a multiple of the main period (with the value to
  use); fewer than three periods of history; impure pairs; saturation at a ceiling or floor (with the `clip` advice).
  A period within 7% of a day or a week, measured on fewer than seven cycles, is called a day or a week rather than,
  say, 23.4 hours.

### Stretches outside the usual profile

Real series contain outages, catch-ups and scheduled jobs, which distort both the decomposition and the forecast: a
13-hour outage inside four days of history can turn a daily cycle into a "27-hour" one. The processors therefore build
a robust **usual daily profile** for each metric, in the spirit of STL: a robust line through the daily medians, a
rolling median over three days, and the median of the same time of day (±15 minutes) across all days, computed twice
and once more without the points already recognised as outliers. With fewer than three days of history, or an unknown
step, the profile is only a level.

A point is outside the profile when it is more than 4 robust σ, and more than `max(min_delta, min_rel_delta·|median|)`,
away from the band the profile covers within ±1 hour of it (so a day that runs an hour late is not an anomaly).
Consecutive points outside on the same side form a stretch:

| Kind | Rule | Reported as |
|---|---|---|
| `long` | at least L/12 points (2 hours with a daily window) | **another regime**: a `warn` finding with UTC times, `broken: true`, a forecast warning, and window advice replaced by "analyse a history without these stretches" |
| `daily` | stretches on the same side starting within ±1 hour of one time of day on at least 3 days and on enough of the days (more for metrics with many stretches) | a **scheduled job**: `recurring` with the time of day, spread, days, duration and size; not counted as another regime |
| `burst` | the rest | counted |

### The forecast

The forecast continues the **reconstructed structure**, not the raw series, from the last sample of the history:

- `method: L` (default) continues it with a linear recurrence; `K` with a vector forecast.
- With `groups`, the forecast continues those components. Without them, it continues the trend, the slow cycles and
  the harmonic pairs that are structure. Unpaired oscillations and alternating components are left out (`left_out`):
  they have no stable continuation and would drive the forecast to the limits within hours.
- **Growing weak cycles are left out.** An oscillation of a bounded metric should neither grow nor decay; a pair whose
  estimated growth over the horizon exceeds ×2 is an amplitude change inside the history or an estimation error, and
  continuing it multiplies that error. Pairs carrying less than 5% of every metric's variance are left out and listed
  under `growing`; strong ones stay, with a warning. Trends are never left out: a level can really grow (a memory leak,
  a filling disk).
- `warning` appears when the kept components grow more than ×2 over the horizon, when the forecast before clipping
  leaves the observed range by more than its width, or when the history contains another regime.
- `clip` keeps the forecast (and groups containing component 0) within limits: `[0, 100]` for percentages.
- The forecast is the smooth expected structure, without noise and without a confidence band. Actual values scatter
  around it by about the residual's σ. Scheduled daily jobs are not part of it.

### ssa_mv: several metrics together

`ssa_mv` places the matrices of all metrics side by side and decomposes them together. A cycle shared by CPU, load and
network occupies one pair of components; a cycle of one metric occupies its own. Every component and group reports
`variance_share` per metric.

With `normalize: true` (the default) each metric is scaled to its standard deviation, at least
`max(min_delta, min_rel_delta·|mean|)`, so a metric in bytes does not drown one in percent. Reconstructions and
forecasts come back in each metric's own units.

### Settings

| Key | Default | Meaning |
|---|---|---|
| `window` | n/3 | the window L: points or a duration; a multiple of the main period. `ssa` accepts up to n − 1 (above n/2 it is replaced by the equivalent n − L + 1), `ssa_mv` up to (n + 1)/2. |
| `forecast` | 0 | the horizon: points or a duration; 0 = no forecast |
| `method` | `L` | `L` recurrent or `K` vector forecast |
| `groups` | proposed | components to keep and forecast: `"0;1-2;3,5-7"` (groups separated by `;`) |
| `clip` | none | `[lo, hi]` or `"lo,hi"`; in `ssa_mv` also `{dim: [lo, hi]}` |
| `step` | inferred | the grid step |
| `k`, `iters` | auto, 8 | number of components to compute (0 = auto), iterations of the partial decomposition |
| `min_points` | 20 | shorter series are skipped |
| `max_gaps` | 0.2 | the largest share of empty grid slots that is filled linearly; more and the series is skipped |
| `reconstruction` | false | `true` adds the reconstructed series of every group (`points`) |
| `min_delta`, `min_rel_delta` | 0 | the smallest meaningful stretch away from the profile; a number in `ssa`, a number or `{dim: number}` in `ssa_mv` |
| `normalize` | true | `ssa_mv` only: bring the metrics to one scale |

### Report

Per series (fields that depend on the metric are scalars in `ssa` and `{dim: value}` in `ssa_mv`):

| Field | Meaning |
|---|---|
| `window`, `K`, `decomposition` | L, the other side of the matrix, `full` or `top-k` |
| `spectrum` | the 20 strongest components: σ, share |
| `noise` | `floor` (the noise threshold), `sd`, `phi` (autocorrelation), `saturation` |
| `components` | `group`, `kind`, `meaning` (in words), `sigma`, `ratio` to the floor, `borderline`, `period`, `period_seconds`, `variance_share`, `growth_per_step` |
| `groups` | the checked groups: `kind`, `mean`, `drift`, `peak_to_peak`, `min`, `max`, `variance_share`, `source` (given or suggested) |
| `groups_check` | separability between groups, what is left in the residual, structure left out of the groups |
| `residual` | `explained` (share of variance the structure explains), `sd`, `robust_sd`, `lag1`, `spikes` |
| `broken`, `runs_total`, `runs` | stretches outside the profile: `kind`, start and end, `duration`, `deviation` (mean distance from the profile, negative = below) |
| `recurring` | scheduled jobs: `time_of_day` (UTC), `spread`, `days` of `of_days`, `duration`, `deviation` |
| `findings` | `{level: ok | info | warn, text}` |
| `forecast` | `group`, `source`, `start_time`, `end_time`, `points`, `growth_per_step`, `left_out`, `growing`, `warning`, `clipped`; or `{reason}` |

### Reading it

`node_ssa` for CPU over four days, a daily window and a one-day forecast:

```text
3 VM(s) analyzed: cpu over 4d to 2026-03-12T00:00:00Z (window 1d, forecast 1d ahead, method L).
web-1:9100: 0 trend: slowly varying, no oscillation within the window; 1-2 harmonic, period 287.7 points = 23.97 h. Noise sigma 1.46, 0.0% of the points off the structure, the structure explains 97%.
  forecast of 0-2 to 2026-03-13T00:00:00Z: 13.1..37, peak at 2026-03-12T13:55:00Z, 14.6 at the end
db-1:9100: 0 trend: slowly varying, no oscillation within the window; 1-2 harmonic, period 288.3 points = 24.03 h. Noise sigma 1.79 (heavy-tailed: sd 2.5x the robust one), 1.4% of the points off the structure, the structure explains 77%.
  recurs daily (a scheduled job): ~02:30 UTC ±25m on 4/4 days, ~20m, above by 34.81
  forecast of 0-2 to 2026-03-13T00:00:00Z: 6.03..28.8, peak at 2026-03-12T13:55:00Z, 7.21 at the end
```

web-1 is a trend plus a daily cycle (components 1–2, period 24 hours) that explain 97% of its movement; the forecast
peaks at about 37% at 14:00. db-1 has the same shape, but its structure explains only 77%: a nightly job around 02:30
(between 02:00 and 02:50 on different days, about 20 minutes, 35 points above the profile) is not part of the daily
cycle. SSA leaves it in the residual, which is why the noise is heavy-tailed, and lists it as a scheduled job.

The same tool on network traffic of a VM that lost its input feed for nine hours, followed by a catch-up:

```text
web-2:9100: 0 trend: …; 1-2 harmonic, period 281.4 points = 23.45 h; 3-4 harmonic, period 164.3 points = 13.69 h; … 9 oscillation, period ~66.7 points = 5.56 h, without a partner; …
  another regime (the trend, periods and forecast are distorted; re-run outside it): 03-10 00:30..03-10 09:25 below by 41.93 (9h); 03-10 09:30..03-10 13:55 above by 117.64 (4h30m)
  forecast of 0-8,10-15,17-18 (without 9,16: no stable continuation) to 2026-03-13T00:00:00Z: 32.9..82.8, peak at 2026-03-12T13:25:00Z, 37.6 at the end
  warning: The main period 281.4 points = 23.45 h is estimated on a history with stretches in another regime (see above) and is distorted by them: fitting with.window to it would chase the distortion. It is most likely the daily cycle (288 points (1d)): keep with.window a multiple of it and analyse a history without the stretches.
```

The outage and the catch-up distort everything: the main period comes out as 23.45 hours, and spurious harmonics and
unpaired oscillations appear. The processor names the two stretches with their UTC times and advises against
chasing the distorted period. The right move is to call again with a history that excludes them (`at` and `range`).

### Practical advice

- Use `step: 5m` and at least four days for a daily cycle, `window: 1d`. For a weekly cycle, three weeks and
  `window: 7d` (at a coarser step).
- If the main period is far from 24 or 12 hours, or changes between calls, look for a `long` stretch in the history
  before trusting the result.
- To forecast only part of the structure (only the trend, say), take the component numbers from a first call and
  pass them as `groups`.
- The forecast says what usually happens, not what will happen: anything outside the usual profile (jobs, incidents,
  growth beyond the trend) is not in it.

## Cost

All analysis runs inside the server, per call, on the data of that call.

| Processor | Cost |
|---|---|
| `jq`, `join`, `anomaly`, `outliers`, `cluster_events` | linear in the data; milliseconds |
| `anomaly_mv` | linear in samples, cubic in metrics (tiny for a few metrics) |
| `anomaly_ensemble` | quadratic in samples per series (Matrix Profile). A call over a hundred VMs × 1440 samples takes about 5 seconds with two metrics and about 15 with ten, queries included |
| `ssa`, `ssa_mv` | tens to hundreds of milliseconds per series (the noise model needs extra decompositions); series run in parallel. A hundred VMs × four days at 5 minutes take about 5 seconds with `ssa` and 10 with `ssa_mv` over five metrics |

The data has to be fetched first: fleet-wide range queries often take longer than the analysis. See
[Performance and limits](user-guide.md#performance-and-limits).
