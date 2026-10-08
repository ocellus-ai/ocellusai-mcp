# For DevOps engineers

**For DevOps** · [Quick start](quick-start.md) · [User guide](user-guide.md) · [Workers](workers.md) · [Templates and jq](templates-and-jq.md) · [Processors](processors.md) · [Writing tools](writing-tools.md)

This page is for engineers who run infrastructure: VMs, Kubernetes, databases, queues, a Prometheus with exporters.
It explains in practical terms what Ocellus AI lets you do, where it saves time, and where it does not help. You need
no background in statistics or programming to read it; the few analysis terms you will meet in answers are explained
at the end.

In one paragraph: Ocellus AI gives your AI assistant (Claude Code, Claude Desktop or another MCP client) a set of
ready-made tools over your monitoring. When you ask the assistant a question such as "did anything unusual happen
on the database hosts last night?", it calls a tool; the tool queries Prometheus, Kubernetes or an API, runs the
analysis on the server and returns a few lines of findings: when, on which host, which metric, how far from its usual
level. The assistant reads those lines and answers you.

## Contents

- [The gap it fills](#the-gap-it-fills)
- [What you can do with it](#what-you-can-do-with-it)
- [Everyday scenarios](#everyday-scenarios)
- [How it fits with what you already have](#how-it-fits-with-what-you-already-have)
- [Why the answers can be trusted, and how far](#why-the-answers-can-be-trusted-and-how-far)
- [Safety and control](#safety-and-control)
- [What it does not do](#what-it-does-not-do)
- [What it takes to try](#what-it-takes-to-try)
- [Words you will see in answers](#words-you-will-see-in-answers)

## The gap it fills

The usual monitoring stack answers well the questions you prepared for: a dashboard for each service, an alert for
each threshold. Day-to-day work brings other questions:

- **Questions nobody prepared a dashboard for.** "What else changed on this host when the latency went up?" "Was it
  only this node or the whole cluster?" "Is this level normal for this database at this time of day?"
- **A different normal for every object.** 80% CPU is normal for a batch server and an emergency for a web front
  end. A fixed threshold is either too noisy for one or too late for the other, and nobody maintains a hundred
  individual thresholds.
- **Too much to look at.** A hundred VMs with ten metrics each are a thousand graphs. During an incident nobody scrolls
  through them, so short events on hosts nobody suspected stay unnoticed.
- **An AI assistant does not solve this on its own.** Given raw access to Prometheus, an assistant writes PromQL by
  guessing label names, and has to read the data itself. One day of one-minute samples for a hundred VMs and ten
  metrics is about 1.4 million numbers, tens of megabytes of JSON: far more than a model can read at once. So it
  samples or averages, and the short events disappear. The arithmetic it does in its head is not reliable, and two
  runs of the same question can give different answers.

Ocellus AI moves the counting to the server. Each tool contains the query, written once by someone who knows the
metrics, and an analysis method that runs in the server over all the data. The assistant gets the findings, usually
a few kilobytes of text, and spends its effort on what it does well: choosing the next question and explaining the
answer to you.

## What you can do with it

The working catalog in [`tools/`](../tools) has 23 tools for a typical estate: VMs with node_exporter, Kubernetes with
kube-state-metrics, PostgreSQL, Kafka, HAProxy and blackbox probes. The sections below group them by the question
they answer. You don't call tools by name: you ask, and the assistant picks a tool by its description.

### A health overview in one question

`estate_overview` collects in one call what you would otherwise check in five places: scrape targets that are down,
failed blackbox probes, firing alerts, filesystems above a fill level, pods that restarted most, and VMs whose load
per core is above 1. It is the natural first question of a shift or of an incident: "is anything wrong right now?"

```text
Estate overview at now (42 VMs, 380 pods):
Scrape targets down: none
Failed probes: artifact-registry
Firing alerts: HostOutOfDiskSpace [warning] x2, PostgresReplicationLag [critical] x1
Filesystems above 80% used (top 10):
  - grafana-01.example.com:9100 /var: 90.4%
  - ftp-01.example.com:9100 /: 80.1%
Pods restarted in the last 1d (top 10):
  - prod / payments / api-7d9f8c6b5-x2kqp: 3
  - stage / default / worker-5c8f565444-8lhn2: 1
VMs with load5 per core > 1 (top 10): none
```

The assistant then drills down with the specific tools.

### A snapshot of one object

| Ask | Tool | What you get |
|---|---|---|
| "Show me db-02." | `vm_metrics` | CPU (including iowait and steal), memory, swap, OOM kills in the last day, every filesystem with inodes and read-only flags, disk throughput and latency, network errors and drops, TCP retransmits. The host is found by hostname or address, with or without the domain. |
| "Why is postgres-03 slower than last week?" | `pg_slowdown_check` | Host, database and replication signals for the last hour next to the same hour a week ago, with the change in percent. The tool's description tells the assistant how to read each signal (what iowait above 10–15% or a cache hit below 95% usually means). |
| "Which HAProxy backend returns errors?" | `haproxy_backend_health` | Every backend with requests per second, 5xx and 4xx shares, errors, retries, queue, active servers and response time, worst first. |
| "Which VMs lose CPU to the hypervisor?" | `vm_steal_time` | VMs whose steal time rose above a threshold: peak, average and how long. Steal is the classic sign of an overcommitted host. |
| "Which pods use the most memory in payments?" "What restarted in the last hour?" | `pod_memory_usage`, `pod_restarts` | Top lists from kube-state-metrics and cAdvisor. |

These need no analysis; their value is that the right queries are already written and the answer comes as a short
list rather than a page of JSON.

### "Is this normal for this host?"

The anomaly tools compare each object with **its own history** over the chosen window instead of a fixed threshold.
A database that always runs at 70% CPU is normal at 70%; a web server that usually idles at 10% stands out at 40%.
You don't configure a threshold per host.

| Tool | Looks at |
|---|---|
| `node_cpu_anomalies` | CPU of every VM |
| `pg_xact_anomalies` | transaction rate of every PostgreSQL database |
| `kafka_lag_anomalies` | consumer lag of every group and topic, spikes only |
| `probe_latency_anomalies` | blackbox probe latency of every service, plus failed probes |

Every one of them has a **minimum change worth reporting**, in the metric's own units: 5 points of CPU, 1000 messages
of lag, 50 ms of latency. Statistically, iowait going from 0.1% to 1% on an idle VM is "ten times higher"; for you it
is nothing, and the tools leave it out. An empty answer therefore means "nothing moved by more than the minimum", and
the answer says how many objects were checked.

### "Which one is different from the others?"

Some questions are about peers rather than history: a deployment with forty replicas where one leaks memory, a
cluster where one pod keeps restarting.

| Tool | Compares |
|---|---|
| `pod_memory_outliers` | the memory of each pod with the other pods of its namespace in the same cluster |
| `pod_restart_outliers` | the restarts of each pod with the other pods of the cluster |

The tool reports the pods that stand out from their group, by how much and in which direction, and ignores groups too
small to compare (fewer than five pods). As with anomalies, a minimum difference (100 MiB by default) keeps small
differences out.

### "What exactly happened on this host, and what moved with it?"

`node_deep_ensemble` is the main investigation tool for VMs. It looks at ten signals of each VM together (CPU, iowait,
steal, memory, disk utilisation, network in and out, load, TCP connections, major page faults) and reports
**segments**: a start, an end, which signals moved, their value at the peak against their usual level, and what kind
of event it was. Several detection methods run side by side, each catching a different kind of change: a spike, a
level that was reached slowly and held, a shape that differs from the rest of the day, a combination of values the
host has not been in before.

An example on synthetic data for three VMs (names made up):

```text
web-1:9100: 1 significant joint segment(s), 5 single-signal, 0 level shift(s), max change 14.0x the minimum, max joint score 90
  - 2026-03-12T09:30:00Z .. 2026-03-12T09:51:00Z (22 samples, single, change 14.0x, score 90; fired by pca(k=0), iforest(w=1); moved: cpu, load, steal): steal 14.1% (usually 0.1, own z 52.1, pca 0.00, shape 0.10); cpu 49.7% (usually 25.1, own z 10.2, pca 0.13, shape 0.10); load 0.7/core (usually 0.3, own z 7.2, pca 0.49, shape 0.10)
web-2:9100: 1 significant joint segment(s), 3 single-signal, 0 level shift(s), max change 12.3x the minimum, max joint score 21.6
  - 2026-03-12T08:10:00Z .. 2026-03-12T08:40:00Z (31 samples, single, change 3.5x, score 21.6; fired by pca(k=0); moved: mem): mem 71.3% (usually 42.0, own z 14.7, pca 0.02, shape 0.10)
db-1:9100: 1 significant joint segment(s), 6 single-signal, 0 level shift(s), max change 12.2x the minimum, max joint score 33.1
  - 2026-03-12T02:00:00Z .. 2026-03-12T02:19:00Z (20 samples, systemic, change 4.3x, score 33.1; fired by pca(k=0), iforest(w=1); moved: cpu, disk, iowait, load, net_rx, net_tx, tcp): cpu 39.3% (usually 17.6, own z 16.5, …); disk 54.7% (usually 13.1, own z 18.3, …); iowait 4.5% (usually 0.6, own z 15, …)
```

What a person reads from it:

- **web-1, 09:30–09:51.** CPU rose from 25% to 50%, and steal rose from 0.1% to 14% at the same time. The VM did not
  get busier: a neighbour on the same physical host took its CPU. A CPU alert alone would have sent you to look for a
  runaway process.
- **web-2, 08:10–08:40.** Memory jumped from 42% to 71% while nothing else moved: not load, a leak or a cache.
- **db-1, 02:00–02:19.** CPU, disk and iowait rose together: a job, not one failing resource.

The tool ranks hosts by the size of the change measured against the minimum change worth reporting for each signal
("change 14.0x the minimum"), not by abstract scores, so the first lines are the ones worth your attention.

There are lighter variants: `node_cpu_mem_ensemble` (CPU and memory only), `node_cpu_mem_mv` (samples where the
usual relation between CPU and memory breaks, such as memory climbing with flat CPU) and `node_disk_pressure_mv`
(iowait, disk utilisation and latency together, which catches a latency jump without more load: a slow datastore
under the VM).

### "Is it one host or many?"

During an incident the first question is often whether the problem is local or shared. `node_cluster_events` runs the
same analysis on a group of VMs and folds the events that happened at the same time into one line, counting on how
many VMs each signal moved:

```text
- 03-12 10:15..10:32 (17 min): net_tx(3), cpu(2), net_rx(2), tcp(2) on 3 VM(s) (db-1, web-1, web-2); 5 event(s) signal=3 single=2, max change 18.1x the minimum, score 33
Single events outside clusters, by VM (7):
- db-1: 4 event(s); 03-12 02:00..02:19 cpu 39.4% (usually 12.3), disk 55.3% (usually 11.3), …
- web-1: 2 event(s); 03-12 09:30..09:51 … steal 14.1% (usually 0.1) …
- web-2: 1 event(s); 03-12 08:10..08:34 mem 70.9% (usually 41.1)
```

Outbound traffic rose on all three VMs at 10:15: one shared cause (here a backup), not three problems. The noisy
neighbour, the memory jump and the nightly job stay events of their own hosts. On a real cluster the same view shows
the schedule of jobs that run on all nodes, and separates "the whole cluster" from "one node" in one call instead of
twelve.

### "What is normal here, and what comes next?"

The forecasting tools describe how a metric usually behaves and continue it:

| Tool | Answers |
|---|---|
| `node_ssa` | the trend, the daily cycle and the noise of one signal per VM, and a forecast |
| `node_mssa` | the same for five signals of a VM together |
| `node_forecast` | a summary for an interval: "CPU tomorrow 09:00–18:00", mean, minimum and maximum with times, how long above a threshold |

```text
web-1:9100: mean 34.4, 28.6 at the start, 30.7 at the end; min 28.6 at 09:00, max 37 at 13:55; actual values scatter by about +-1.5 around it (noise, not the model error).
  profile (45m means): 09:00 29.5 | 09:45 31.6 | 10:30 33.4 | 11:15 34.9 | 12:00 36.1 | 12:45 36.8 | 13:30 37 | 14:15 36.8 | 15:00 36.2 | 15:45 35.1 | 16:30 33.6 | 17:15 31.7
  above 30: 8h35m in total (94% of the interval) in 1 stretch(es), first at 09:30, longest 09:30..18:05 (8h35m)
```

Practical uses:

- **Capacity questions** before a known peak: "will the web tier stay under 80% tomorrow between 09:00 and 18:00?"
- **Maintenance windows**: the forecast shows the quietest hours of each host.
- **Scheduled jobs you did not know about.** The analysis lists stretches that recur at the same time every day, such
  as "~02:30 UTC ±25m on 4/4 days, ~20m, above by 34.81": a backup or a cron job, with its time, duration and size.
- **Outages in the history.** If the history contains another regime, such as an outage followed by a catch-up, the
  tool names that stretch with its times and warns that the forecast is distorted, rather than quietly forecasting
  from broken data.

The forecasts need at least three to four days of history, and they describe what usually happens: a job or an
incident that is not part of the usual daily pattern is not in them.

### Looking back: post-mortems

Every tool that looks at a time window has an `at` parameter, "now" by default. The same questions work for the past:
"what happened on the database hosts between 01:00 and 03:00 UTC on 12 March?" The analysis is deterministic: the
same data gives the same answer, so a finding quoted in a post-mortem can be reproduced later, as long as Prometheus
still keeps the data.

### Command-line tools and APIs

Besides Prometheus, a tool can:

- run a command-line program from an allowlist (`df`, `dig`), without a shell;
- call a JSON HTTP API under a base URL you configure: Alertmanager, GitLab, Grafana, an internal service;
- combine sources in one tool: resolve a service name with `dig`, then ask Prometheus about exactly the addresses
  behind it.

The reference tools include Alertmanager silences: list, create and expire. Creating a silence is an action, and it
only works if you give the instance permission to send `POST` requests; by default you can keep every API read-only.

### Your own checks as tools

A tool is one YAML file: the query, the analysis and the text of the answer. No code, no rebuild; the server checks
every file when it starts and names the file and field of any mistake. This turns team knowledge into something the
assistant can use:

- the check you run first for a certain kind of alert;
- the PromQL you keep in a notes file;
- reading hints in the tool's description, like those of `pg_slowdown_check`, so the assistant interprets the numbers
  the way your team does.

The repository includes an assistant skill (`skills/ocellus-tool-builder`) that interviews you about what the tool
should answer and writes the file. See [Writing tools](writing-tools.md).

## Everyday scenarios

**Start of a shift.** Ask "is anything wrong right now?" (`estate_overview`), then "did anything unusual happen on
the production VMs overnight?" (`node_deep_ensemble` or `node_cluster_events` over the night). You get the
targets down, the alerts, and the events of the night that never reached an alert threshold.

**An alert fired.** "CPU high on db-3": ask what else moved on db-3 around that time (`node_deep_ensemble` with a
short range), whether other hosts moved at the same time (`node_cluster_events`), and show the raw samples around the
moment (`node_signal_window`). In a few calls you know whether it is steal from a neighbour, a job, a single host or
the whole cluster.

**"The service is slow."** Look at probe latency (`probe_latency_anomalies`), the backends behind the load balancer
(`haproxy_backend_health`) and the database against last week (`pg_slowdown_check`). The assistant collects the
evidence from all three while you think about the cause.

**After a release.** Compare the new pods with each other (`pod_memory_outliers`, `pod_restart_outliers`) and look
for anomalies after the deployment time on the hosts involved. One bad replica, or memory that grows on every pod,
shows up without a new dashboard.

**Shared hardware.** "Do our VMs suffer from neighbours?" (`vm_steal_time`, and the steal signal in
`node_deep_ensemble`): which VMs, when, how long, and whether steal moved together on several VMs, which points to a
shared physical host.

**Capacity and planning.** "When will the web tier be busiest tomorrow, and will it exceed 70%?" (`node_forecast`
with `above: 70`). "Which jobs run every night on the database hosts?" (`node_ssa`, the recurring stretches).

**Post-mortem.** Re-run the same questions with `at` set to the incident. The answer gives exact times, the signals
that moved, and their values against their usual levels, ready to paste into the timeline.

## How it fits with what you already have

| You have | Ocellus AI |
|---|---|
| **Prometheus** (or VictoriaMetrics, Thanos, Mimir) | is its main data source. It stores nothing itself and needs no new exporters. |
| **Grafana** | answers questions instead of showing graphs; scans every host instead of the panels you open. Use Grafana to look at what it found. |
| **Alertmanager** | does not page anyone and does not watch in the background. It is for investigation after an alert, or for checks before one. It can read alerts and silences. |
| **Command-line tools and scripts** | runs allowlisted commands as tools, so the assistant can use them without a shell. |
| **Runbooks** | can hold their checks as tools, with the reading hints in the descriptions. |
| **An AI assistant** | gives it tested queries and server-side analysis instead of raw data, so it can work on a whole fleet. |

## Why the answers can be trusted, and how far

What the methods do, in plain terms:

- **Each object is compared with itself** (or with its peers), not with a number someone picked once.
- **The usual level is robust.** It is based on medians, so a few spikes do not shift what counts as normal and do
  not hide each other.
- **Changes are measured in your units.** Every finding states the value and the usual value ("mem 71.3%, usually
  42.0"), and changes smaller than the minimum you care about are not reported.
- **Gaps are not anomalies.** A scrape hole or a restarted exporter is reported as a gap or a skipped object with a
  reason, not as an event.
- **Answers say what was checked**: how many objects were analysed and how many skipped, the window and the
  thresholds. "Nothing found" comes with "out of 42 VMs".
- **Answers explain themselves**: which signals moved and which detection methods fired, so you can check the
  finding on a graph (`node_signal_window` shows the raw samples around any moment).
- **Limits are written into the tool descriptions**, so the assistant knows them too: for example, that a moderate
  level change may need a more sensitive setting around a known moment, or that a forecast has no confidence band.

How far: a finding is a lead, not a verdict. The tools tell you where and when to look, and they are good at finding
the event you did not know to look for. Whether it matters for your service is your judgement. The assistant can
also misread a correct answer, which is why the answers keep the raw values next to the scores.

## Safety and control

- **Read-only by default.** The working catalog only reads. Actions (such as creating a silence) are separate tools
  that need an instance you explicitly allow to send `POST`, `PUT`, `PATCH` or `DELETE`.
- **The assistant fills in parameters, nothing else.** It cannot write PromQL, choose a command or a URL: every
  argument is checked against the tool's schema, the tools escape values before they put them into a query, commands
  run without a shell from an allowlist, and HTTP calls stay under the base URL you set.
- **Secrets stay in the server's config**, taken from environment variables, and are never logged or shown in error
  messages.
- **Raw metrics stay in your network.** The assistant's model sees only the tool answers: the findings, with host
  names and values. If the model runs as a cloud service, those answers are sent there; the raw data is not.
- **Every call is logged** with the tool, the duration and the outcome.
- **Not yet:** the HTTP endpoint has no authentication. Keep it on a private network or behind an authenticating
  proxy, or run it over stdio on your own machine.

## What it does not do

- **It is not a monitoring system.** It stores nothing, runs nothing in the background and pages nobody. It answers
  when asked.
- **It needs your metrics.** If an exporter is not installed, the tools have nothing to analyse. The catalog assumes
  common exporters (node_exporter, kube-state-metrics, postgres_exporter, kafka_exporter, haproxy_exporter,
  blackbox_exporter) and common job names; you adjust the selectors to your labels once, when you install it.
- **It cannot see further back than Prometheus keeps data**, and the forecasts need several days of history.
- **It does not replace knowing your system.** It finds unusual behaviour; it does not know that the 02:00 job is
  expected unless you tell the assistant, or put it into a tool's description.
- **Heavy questions take seconds and load Prometheus.** An analysis over a hundred VMs and a day of one-minute
  samples takes about 5 seconds with two metrics and about 15 with ten, queries included. There is no cache: every
  call queries again.

## What it takes to try

- A Prometheus-compatible server with your exporters.
- One binary (or a container image) and a config file with the Prometheus URL. No database, no Python, no external
  service.
- An MCP client: Claude Code, Claude Desktop or another client that supports MCP servers.
- A one-time check that the label names in the catalog match yours (the [Quick start](quick-start.md#4-match-the-catalog-to-your-labels)
  shows how).

The [Quick start](quick-start.md) goes from a clone to the first questions; with a Prometheus and node_exporter
already in place it takes about fifteen minutes.

## Words you will see in answers

| Word | Meaning |
|---|---|
| **usually**, typical, baseline, median | the object's normal level over the window |
| **anomaly** | a sample or a period unusual for this object compared with its own history |
| **outlier** | an object unusual compared with its peers |
| **segment** | an anomalous period with a start, an end and a peak |
| **score**, own z | how unusual something is, in units of the object's own usual variation: 5 means five times the usual spread. Use it to rank, not to compare hosts: a quiet host gets high scores from small moves |
| **change Nx the minimum** | how big the change was, in units of the minimum change worth reporting; this one compares across hosts and signals |
| **minimum change**, `min_delta` | the smallest change worth reporting, in the metric's units (5 points of CPU, 50 ms) |
| **single / systemic / correlation** | one signal misbehaved / several signals at once / no signal is unusual alone but their usual relation broke |
| **fired by** | which detection methods flagged the segment |
| **level shift** | a signal that moved to a new level and stayed there |
| **skipped** | an object that could not be analysed, with the reason (too few samples, too many gaps) |
| **regime**, another regime | a long stretch where a signal left its usual daily pattern: an outage, a catch-up |
| **daily**, scheduled job | a stretch that recurs at the same time of day |
| **noise sigma** | how much the actual values scatter around the forecast |
| **sensitivity**, threshold, risk | how strict a tool is: higher sensitivity or threshold and lower risk report fewer, stronger findings |
| **cluster** (in `node_cluster_events`) | events on several hosts that overlapped in time |

The methods behind these words are described in [Processors](processors.md) for those who want the details.
