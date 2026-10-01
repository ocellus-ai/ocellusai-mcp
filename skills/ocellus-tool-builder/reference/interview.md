# Interviewing the user

Goal: collect exactly what is needed to write a tool that answers the user's question, without
making them fill in a form. Ask every question **in the user's language** (translate the wording
below; keep YAML terms such as `min_delta` as they are, explained in plain words).

## Contents

- [Principles](#principles)
- [Round 1: the need](#round-1-the-need)
- [Round 2: the data source](#round-2-the-data-source)
- [Round 3: objects and time](#round-3-objects-and-time)
- [Round 4: analysis](#round-4-analysis)
- [Round 5: the answer](#round-5-the-answer)
- [Round 6: parameters for the calling agent](#round-6-parameters-for-the-calling-agent)
- [Actions and side effects](#actions-and-side-effects)
- [Defaults when the user says "you decide"](#defaults-when-the-user-says-you-decide)
- [The spec](#the-spec)
- [Example flow](#example-flow)

## Principles

1. **Start from the question, not from the YAML.** "What should the agent be able to answer?" beats
   "Which PromQL query?". Ask for one or two example questions phrased the way a person or an agent
   would ask them.
2. **Find out before asking.** Worker instances, shell allowlist, REST base URLs, existing tools and
   the `job` label come from the config and the catalog. Metric and label names can be checked
   against Prometheus with the user's permission. Ask only what cannot be found.
3. **Rounds of at most 3–4 questions.** Skip a round when the first message already answers it. Merge
   rounds when the answers are obvious. Most tools need two or three rounds.
4. **Offer options with a recommended default** ("Window: 1h (recommended), 6h, 1d, or another
   value?"). "You decide" must always be an acceptable answer; then take the default and list it in
   the spec.
5. **Use the user's vocabulary** in questions and the spec, and translate it into tool terms
   yourself. Don't ask the user to choose between `zscore` and `iqr`; ask whether they want "spikes
   compared with the object's own history" or "objects that stand out from the others".
6. **Stop when the spec is complete.** Mandatory items: data source and worker instance; the object
   and its identifying labels; time scope; analysis; the answer's content; the noise threshold for
   any detector; side effects for actions. Everything else has a default.
7. **Confirm the spec before writing the file**, then don't re-ask decided points.

## Round 1: the need

Always ask (unless the first message already answers):

- **What question should the tool answer?** Ask for an example: "Which VMs had CPU steal above 5% in
  the last hour?", "Is anything unusual with the Kafka consumers since this morning?".
- **What is the answer for?** Incident triage (fast, focused, names the culprit), a regular report
  (complete, stable format), capacity planning (trends and forecasts), or an action (changes
  something). This sets the level of detail and the defaults.
- **Which objects?** Pods, deployments, VMs/hosts, databases, topics/consumer groups, services,
  disks, alerts, pipelines… and roughly how many (5 or 500 changes the output design).

## Round 2: the data source

Map the need to a worker. Ask only when it is not clear:

| Source | Ask (if unknown) | You check yourself |
|---|---|---|
| Prometheus | Which exporter/metrics hold the data (node_exporter, kube-state-metrics, cAdvisor, postgres_exporter…)? Which Prometheus if there are several instances? | Instance names in `workers`; metric and label names with read-only queries, after asking |
| Shell command | The exact command and flags; output format (JSON is best, then lines); does it need `KUBECONFIG` or other env; where the server runs (in Kubernetes it needs an image with the command) | The command is in the instance's `allowlist`; `env` of the instance |
| REST API | Which API and endpoint; an example response (or docs link); read-only or an action | A `type: rest` instance with that base URL exists; its `methods` allow the verb |
| Several | Which source defines the list of objects, which adds values | Instances of each |

- **Metric names unknown?** Offer to look them up (read-only queries, see
  [prometheus.md](prometheus.md#discovering-metrics-and-labels)), or ask the user to paste a sample
  series from the Prometheus UI.
- **The instance does not exist** (no rest instance for that API, command not in the allowlist): the
  tool cannot work until the operator adds it. Tell the user what to add to the config (base URL,
  allowlist entry) and, if they agree, write it as a suggested config snippet. Secrets go into
  `${ENV}` variables in the instance, never into the tool.

## Round 3: objects and time

- **Filter**: by which labels can the caller narrow the objects (namespace, cluster, instance regex,
  job)? Which of them are required? An optional regex filter defaults to `.*`.
- **Time scope**:
  - *now* — an `instant` query (current state, top-N, outliers among peers);
  - *over a window* — an `instant` query with `rate`/`max_over_time`/subqueries (average, peak, time
    above a threshold);
  - *a time series* — a `range` query (anomalies in history, segments, forecasts).
- **Past incidents**: should the caller be able to look at a moment in the past? Then add the `at`
  parameter (`now`, `now-3d`, RFC3339), see [prometheus.md](prometheus.md#the-at-parameter). Recommend
  it for every window-based tool.
- **Window and resolution**: default look-back `range`, `step` (sample resolution), rate window.

## Round 4: analysis

Ask what "interesting" means, in plain words, and map it:

| The user wants | Translate to |
|---|---|
| The values themselves, sorted | no `process`; jq sorts and caps |
| Values above/below a fixed threshold | filter in PromQL (`> {{ .threshold }}`) |
| Peak or minutes above a threshold within a window | subqueries, [prometheus.md](prometheus.md#windows-and-subqueries) |
| "Spikes or drops compared with its own history" | `anomaly` over a `range` query |
| "Which ones stand out from the others" | `outliers` over an `instant` query |
| "When did the relation between X and Y break" | `join` → `anomaly_mv` |
| "Anomalous periods, and which metric caused them" | `join` → `anomaly_ensemble` |
| "The same problem on many hosts at once" | ensemble → jq → `cluster_events` |
| "Trend, daily pattern, what to expect tomorrow" | `ssa` / `ssa_mv` |

For any detector, **always ask for the smallest change worth reporting**, in the metric's units:
"Below how many percentage points of CPU change is it just noise for you? I'd suggest 5." Without it,
a nearly idle object turns a 0.01% wiggle into a huge "anomaly". Also ask, when relevant:

- direction: only increases, only decreases, or both;
- sensitivity: standard or only extreme cases (maps to `k`, `threshold`, `risk`);
- how many objects and how many findings per object to show.

## Round 5: the answer

- **Content**: which fields per object (name, value, usual value, time, direction, score…), units and
  precision, sort order, how many rows.
- **Form**: text summary plus structured data (level 2, the default for analyses and summaries), or
  structured JSON only (level 1, when the calling agent processes the data further). See
  [response.md](response.md).
- **Empty result**: what should it say? ("No VMs with steal above 5% in the last 1h.") An empty
  answer must be clearly different from an error and must name the threshold.
- **Skipped objects** (not enough data, missing labels): list them or just count them?

Show a two- or three-line sample of the text answer in the spec; it is the fastest way to agree on
the output.

## Round 6: parameters for the calling agent

Propose the parameter list yourself and let the user adjust it:

- what the caller can change (filters, window, threshold, sensitivity, top-N, `at`);
- what is fixed in the tool (metric, aggregation, analysis method);
- required parameters: only those without which the request is impossible;
- defaults that give a useful answer on the first call (never a threshold of 0 that returns noise);
- units that are natural for the caller (ms instead of seconds, MiB instead of bytes): the
  conversion happens in the template.

## Actions and side effects

A REST call with `POST`/`PUT`/`PATCH`/`DELETE`, or a command that changes state, acts on the user's
systems every time an agent calls the tool. Before writing one:

- confirm in plain words what it changes and that this is what they want;
- check the rest instance allows the verb (`methods`); a read-only instance is a feature, not an
  obstacle — don't suggest weakening it casually;
- make the dangerous inputs `required` with tight `pattern`s; no "match everything" defaults;
- the `description` starts the effect sentence with "This is an action: …";
- shell tools that mutate state (`kubectl delete`, `scale`, `rollout restart`) are strongly
  discouraged: warn, and write one only on the user's explicit insistence;
- never test-call an action tool without explicit permission for the exact arguments.

## Defaults when the user says "you decide"

| Item | Default |
|---|---|
| Rate window | `5m` |
| Look-back for anomalies / outliers over time | `6h` (`range`), `step: 60s` |
| Look-back for segments (ensemble) | `1d`, `step: 60s`, `window: 1h` |
| History for forecasts | `7d` (at least 3 periods of the daily cycle), `step: 5m`, `window: 1d`, horizon `1d` |
| `at` | `now` |
| Top-N | `10` (max 100–200) |
| Point anomalies | `iqr`, `k` 1.5 (standard) or 3 (only extreme, for fleet-wide scans), `max_anomalies` 3–5 |
| Outliers among peers | `iqr`, `k` 1.5, `min_points` 5 |
| Joint anomalies | `mahalanobis`, `threshold` 3–4 |
| Ensemble | `risk` 0.001, `min_len` 2m, `max_ranges` 20 |
| `min_delta` | CPU busy 5 pp, memory 3 pp, iowait/steal 1 pp, disk utilisation 10 pp, network 5 Mbit/s, load1 per core 0.25, TCP connections 20, latency 50 ms, transactions 0.5 tx/s, Kafka lag 1000 messages, pod memory 100 MiB, restarts 3; or `min_rel_delta` 0.2 when objects differ by orders of magnitude |
| Answer | level 2: text summary + structured data |
| Language of the tool's texts | English |

## The spec

Before writing the file, show the user a short spec **in their language**, for example:

```
Tool: node_steal_time  (tools/node_steal_time.yaml)
Answers: which VMs had CPU steal above a threshold in a window, and how long
Source: Prometheus instance "prometheus", node_exporter node_cpu_seconds_total{mode="steal"}
Objects: VMs (label instance; node name from node_uname_info)
Parameters:
  window     string  default 1h   ^[0-9]+[smhdwy]$   look-back window
  threshold  number  default 1    0..100             minimum peak steal, %
  step       string  default 1m                      resolution for peak and minutes above
Analysis: peak, average and minutes above the threshold, filtered in PromQL
Answer (text + structured), sorted by peak, highest first:
  3 VM(s) with CPU steal time above 1% in the last 1h:
  - db-02 (10.0.0.12:9100): peak 7.31%, avg 0.80%, above threshold ~12 min
Empty: "No VMs with CPU steal time above 1% in the last 1h."
Assumptions to check: node_exporter on every VM; no job filter
```

Keep it to what the user can judge: the question it answers, the data, the parameters, the shape of
the answer and the assumptions. No YAML in the spec unless the user asks.

## Example flow

User: *"I need a tool that shows which Kafka consumer groups are falling behind."*

Round 1 (the need is clear, the objects too, so ask about source and meaning of "falling behind"):

1. Where is the lag? kafka_exporter in Prometheus (`kafka_consumergroup_lag`), Burrow, or another
   API? (I can look in Prometheus myself if you allow read-only queries.)
2. "Falling behind" means: lag above a fixed number of messages now (recommended for alerts-style
   checks), or lag growing unusually compared with the group's own history?
3. Should the agent be able to look at a past moment, e.g. last night's incident? (Recommended: yes.)

Round 2 (after "kafka_exporter; unusual growth; yes"):

1. Smallest lag change worth reporting: 1000 messages above the group's usual lag (recommended), or
   another value?
2. Per group, show the topic, current lag, usual lag and when it jumped — enough? Top 10 groups by
   the number of jumps?

Then the spec, a "yes", the file, validation and a test call.
