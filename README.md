# Ocellus AI

**Ask your monitoring what happened. Get findings, not graphs.**

Ocellus AI is an [MCP](https://modelcontextprotocol.io) server that gives your AI assistant (Claude Code, Claude
Desktop or any MCP client) ready-made tools over your monitoring. Ask "did anything unusual happen on the database
hosts last night?" and the assistant calls a tool: it queries Prometheus, analyses every host against its own history
on the server and returns a few lines. When, on which host, which metric, how far from its usual level.

**[Documentation](https://ocellusai.gitbook.io/ocellusai-mcp)** ·
[Quick start](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/quick-start) ·
[For DevOps engineers](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/for-devops) ·
[Tool catalog](tools)

## Ask, don't scroll

> **You:** Did anything unusual happen on the web and database VMs since midnight?

The assistant calls `node_deep_ensemble`, which looks at ten signals of every VM together (CPU, iowait, steal, memory,
disk, network in and out, load, TCP connections, page faults) and compares each VM with its own usual behaviour. From
the few lines it gets back, it can tell you:

- **web-1, 09:30–09:51.** CPU rose from 25% to 50%, and steal from 0.1% to 14% at the same moment. The VM did not get
  busier: a neighbour on the same physical host took its CPU. A CPU alert alone would have sent you looking for a
  runaway process.
- **web-2, 08:10–08:40.** Memory jumped from 42% to 71% while nothing else moved: a leak or a cache, not load.
- **db-1, 02:00–02:19.** CPU, disk and iowait rose together: a job, not one failing resource.

Nobody set a threshold for these hosts, and nobody opened a dashboard.

<details>
<summary>What the tool returned</summary>

```text
web-1:9100: 1 significant joint segment(s), 5 single-signal, 0 level shift(s), max change 14.0x the minimum, max joint score 90
  - 2026-03-12T09:30:00Z .. 2026-03-12T09:51:00Z (22 samples, single, change 14.0x, score 90; fired by pca(k=0), iforest(w=1); moved: cpu, load, steal): steal 14.1% (usually 0.1, own z 52.1, pca 0.00, shape 0.10); cpu 49.7% (usually 25.1, own z 10.2, pca 0.13, shape 0.10); load 0.7/core (usually 0.3, own z 7.2, pca 0.49, shape 0.10)
web-2:9100: 1 significant joint segment(s), 3 single-signal, 0 level shift(s), max change 12.3x the minimum, max joint score 21.6
  - 2026-03-12T08:10:00Z .. 2026-03-12T08:40:00Z (31 samples, single, change 3.5x, score 21.6; fired by pca(k=0); moved: mem): mem 71.3% (usually 42.0, own z 14.7, pca 0.02, shape 0.10)
db-1:9100: 1 significant joint segment(s), 6 single-signal, 0 level shift(s), max change 12.2x the minimum, max joint score 33.1
  - 2026-03-12T02:00:00Z .. 2026-03-12T02:19:00Z (20 samples, systemic, change 4.3x, score 33.1; fired by pca(k=0), iforest(w=1); moved: cpu, disk, iowait, load, net_rx, net_tx, tcp): cpu 39.3% (usually 17.6, own z 16.5, …); disk 54.7% (usually 13.1, own z 18.3, …); iowait 4.5% (usually 0.6, own z 15, …)
```

Synthetic data for three VMs; the names are made up.

</details>

## Why not just give the assistant Prometheus?

Because the data does not fit. A day of one-minute samples for a hundred VMs and ten metrics is about 1.4 million
numbers, tens of megabytes of JSON. A model cannot read that, so it samples or averages, and the short events
disappear. It guesses label names when it writes PromQL, its arithmetic is unreliable, and two runs of the same
question can disagree.

Ocellus AI moves the counting to the server. Each tool holds a query written once by someone who knows the metrics,
and an analysis that runs over all of the data. The assistant gets a few kilobytes of findings and spends its effort
on what it does well: choosing the next question and explaining the answer.

## What you can ask

| Ask | What answers it |
|---|---|
| "Is anything wrong right now?" | `estate_overview`: targets down, failed probes, firing alerts, full filesystems, restarting pods and overloaded VMs in one call |
| "Is this normal for this host?" | anomaly tools that compare every VM, database, Kafka consumer group or probe with its own history |
| "Which pod is different from the rest?" | outliers among peers: the one replica out of forty that leaks memory |
| "Is it one host or the whole cluster?" | `node_cluster_events`: simultaneous events on many hosts folded into one incident |
| "Why is postgres-03 slower than last week?" | `pg_slowdown_check`: host, database and replication signals against the same hour a week ago |
| "Will the web tier stay under 70% tomorrow from 9 to 6?" | `node_forecast`: trend and daily cycle, the peak with its time, how long above the threshold |

Every tool that looks at a time window takes `at`, so the same questions work for a post-mortem.

The [catalog](tools) has 24 tools for VMs with node_exporter, Kubernetes, PostgreSQL, Kafka, HAProxy and blackbox
probes. Besides Prometheus (or VictoriaMetrics, Thanos, Mimir), a tool can run an allowlisted command such as `dig`,
or call a JSON API such as Alertmanager or GitLab.

Your own checks become tools too. A tool is one YAML file with the query, the analysis and the text of the answer: no
code, no rebuild. The [tool builder skill](skills/ocellus-tool-builder) in this repository lets an AI coding agent
interview you and write the file.

## Answers you can check

- **Every host is compared with itself** or with its peers, not with a number someone picked once. 80% CPU is normal
  for a batch server and an emergency for a web front end.
- **Changes are in your units.** Each finding states the value and the usual value ("mem 71.3%, usually 42.0"), and
  a move smaller than you care about (5 points of CPU, 50 ms of latency) is not reported.
- **Answers say what was checked.** "Nothing found" comes with "out of 42 VMs", and skipped hosts come with a reason.
- **Same data, same answer.** The analysis is deterministic, so a finding quoted in a post-mortem can be reproduced.

Under the hood: robust baselines, an ensemble of detectors (Matrix Profile, autoregressive residuals, level shifts,
seasonal residuals, Isolation Forest, PCA) with thresholds from extreme value theory, and Singular Spectrum Analysis
for trends and forecasts. All of it runs in one Go binary, with no Python and no external service.

## Safe to connect

- **Read-only by default.** Actions such as creating an Alertmanager silence need an instance you explicitly allow
  to write.
- **The assistant fills in parameters, nothing else.** It cannot write its own PromQL or pick a command or a URL.
  Arguments are checked against the tool's schema and escaped, commands run from an allowlist without a shell, and
  HTTP calls stay under the base URL you set.
- **Raw metrics stay in your network.** The model sees the tool answers, not the data. Secrets live in the server
  config and never appear in logs.
- **Not yet:** the HTTP endpoint has no authentication. Keep it on a private network or behind an authenticating
  proxy, or run it over stdio.

It is not a monitoring system: it stores nothing, runs nothing in the background and pages nobody. It answers when
asked, from the metrics you already collect, and its findings are leads, not verdicts.

## Quick start

You need Go 1.27.1 or newer, a Prometheus with node_exporter, and an MCP client.

```bash
git clone https://github.com/ocellus-ai/ocellusai-mcp.git
cd ocellusai-mcp
make build
cp config.example.yaml config.yaml   # set workers.prometheus.url
./bin/ocellusai-mcp -config config.yaml
```

Connect it to Claude Code and ask your first question:

```bash
claude mcp add --transport http ocellus http://localhost:8080/mcp
```

The [full quick start](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/quick-start) takes about fifteen
minutes, including a one-time check that the catalog's label names match yours. To run it as a service,
[`deploy/`](deploy) has a Docker Compose file and a Kubernetes manifest.

## Documentation

| Guide | What's in it |
|---|---|
| [For DevOps engineers](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/for-devops) | what it gives an infrastructure team, in plain terms |
| [Quick start](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/quick-start) | from a clone to the first questions |
| [User guide](https://ocellusai.gitbook.io/ocellusai-mcp/getting-started/user-guide) | configuration, transports, deployment, security, troubleshooting |
| [Writing tools](https://ocellusai.gitbook.io/ocellusai-mcp/building-tools/writing-tools) | your own checks as YAML tools |
| [Processors](https://ocellusai.gitbook.io/ocellusai-mcp/building-tools/processors) | how the anomaly detection and forecasts work |
| [Architecture](https://ocellusai.gitbook.io/ocellusai-mcp/developing-ocellus-ai/architecture) | how the server is built, for contributors |

The full documentation is at **[ocellusai.gitbook.io/ocellusai-mcp](https://ocellusai.gitbook.io/ocellusai-mcp)**.
