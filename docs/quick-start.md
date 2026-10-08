# Quick start

[For DevOps](for-devops.md) · **Quick start** · [User guide](user-guide.md) · [Workers](workers.md) · [Templates and jq](templates-and-jq.md) · [Processors](processors.md) · [Writing tools](writing-tools.md)

This page takes you from nothing to an AI agent asking your Prometheus questions through Ocellus AI. It takes about
fifteen minutes if you already have a Prometheus with node_exporter.

You will:

1. build the server;
2. point it at your Prometheus;
3. check that the bundled tool catalog loads;
4. match the catalog to your label names;
5. start the server and connect an MCP client;
6. ask the first questions.

The [User guide](user-guide.md) explains every step in more depth.

## What you need

- **Go 1.27.1 or newer** to build from source, or **Docker** to build an image.
- **A Prometheus** the server can reach over HTTP. Any backend that serves the Prometheus HTTP API works too:
  VictoriaMetrics, Thanos, Mimir. The bundled tools need **node_exporter** data; the Kubernetes, PostgreSQL, Kafka,
  HAProxy and blackbox tools need their exporters.
- **An MCP client**: Claude Code, Claude Desktop, or any other client that speaks MCP over stdio or Streamable HTTP.

## 1. Build

```bash
git clone https://github.com/ocellus-ai/ocellusai-mcp.git
cd ocellusai-mcp
make build
```

This produces one static binary, `bin/ocellusai-mcp`. Check it:

```bash
./bin/ocellusai-mcp -version
```

To build a container image instead:

```bash
docker build -t ocellusai-mcp .
```

The image holds the binary, the working catalog in `/app/tools` and the example config as `/app/config.yaml`. It
runs as a non-root user and contains no shell and no CLI tools, which is all the bundled catalog needs. (Tools that
run command-line programs need an image with them; see [Workers](workers.md#in-a-container).)

## 2. Write a config

Create `config.yaml` in the repository directory. The smallest config that runs the bundled catalog has one
Prometheus:

```yaml
server:
  transport: http          # serve MCP over Streamable HTTP
  listen: ":8080"          # the MCP endpoint is /mcp, the health check is /healthz

tools_dir: ./tools         # every *.yaml file here is one tool

workers:
  prometheus:              # the tools refer to this name
    url: http://prometheus.example.internal:9090

log:
  level: info
  format: text             # easier to read than json while you try it out
```

Replace the URL with your Prometheus. If it needs a token, add a header and keep the secret in the environment:

```yaml
workers:
  prometheus:
    url: https://prometheus.example.internal
    headers:
      Authorization: "Bearer ${PROM_TOKEN}"
```

`${PROM_TOKEN}` is read from the environment when the server starts. If the variable is not set, the server refuses
to start and names it.

[`config.example.yaml`](../config.example.yaml) lists every option with comments. The [Workers](workers.md) guide
explains how to connect a second Prometheus, a command-line program or an HTTP API.

## 3. Validate

```bash
./bin/ocellusai-mcp -config config.yaml -validate
```

`-validate` loads the config and every tool exactly as a real start would, prints one line per tool and exits. It
does not contact Prometheus. Each line names the tool, the worker it uses and its analysis chain:

```text
alerts_firing                    worker=prometheus   type=prometheus calls=-            process=-            file=tools/alerts_firing.yaml
estate_overview                  worker=prometheus   type=prometheus calls=-            process=-            file=tools/estate_overview.yaml
kafka_lag_anomalies              worker=prometheus   type=prometheus calls=-            process=anomaly      file=tools/kafka_lag_anomalies.yaml
node_cpu_mem_ensemble            worker=prometheus   type=prometheus calls=cpu>mem      process=join>anomaly_ensemble file=tools/node_cpu_mem_ensemble.yaml
...
```

If something is wrong, the server stops and the message names the file and the field, for example:

```text
ocellusai-mcp: config.yaml: environment variables not set: PROM_TOKEN
```

## 4. Match the catalog to your labels

The tools in [`tools/`](../tools) are real PromQL written for a typical setup. A tool that loads fine can still return
nothing if your scrape jobs are named differently. Each file says what it expects in its header comment. The
assumptions are:

| Tools | Expect |
|---|---|
| `node_*`, `estate_overview` (VMs) | node_exporter scraped as `job="node_exporters"` |
| `vm_metrics`, `vm_steal_time` | node_exporter under any job name |
| `estate_overview` (probes), `probe_latency_anomalies` | blackbox_exporter as `job="blackbox"` with a `service` label |
| `estate_overview` (pods), `pod_restarts`, `pod_restart_outliers` | kube-state-metrics; the outlier tool and the overview group by a `cluster` label |
| `pod_memory_usage`, `pod_memory_outliers` | cAdvisor metrics; the outlier tool expects `job="kubelet"` and a `cluster` label |
| `alerts_firing`, `estate_overview` (alerts) | alerting rules evaluated by this Prometheus (the `ALERTS` series) |
| `pg_xact_anomalies` | postgres_exporter as `job="postgres-exporter"` |
| `pg_slowdown_check` | postgres_exporter and node_exporter on the same host, found by the host name in `instance` |
| `kafka_lag_anomalies` | kafka_exporter (`kafka_consumergroup_lag`) |
| `haproxy_backend_health` | haproxy_exporter as `job="haproxy-exporter"` |

See what your Prometheus calls its jobs:

```bash
curl -s http://prometheus.example.internal:9090/api/v1/query --data-urlencode 'query=count by (job) (up)'
```

If your node_exporter job is called, say, `node`, rewrite the selector in every tool at once:

```bash
sed -i.bak 's/job="node_exporters"/job="node"/g' tools/*.yaml && rm tools/*.bak
```

Tools whose exporters you don't run are harmless: they load and answer with an empty result. You can also delete
those files.

## 5. Start the server

```bash
./bin/ocellusai-mcp -config config.yaml
```

Check that it is up:

```bash
curl -s localhost:8080/healthz
```

The answer is `ok`. Logs go to stderr; every tool call logs one line with the tool name, the status and the duration.

With Docker, mount your config over the example one. Inside the container `localhost` is the container itself, so
the Prometheus URL in the config must be reachable from there:

```bash
docker run --rm -p 8080:8080 -v "$PWD/config.yaml:/app/config.yaml:ro" ocellusai-mcp
```

## 6. Connect an MCP client

**Claude Code**, over HTTP:

```bash
claude mcp add --transport http ocellus http://localhost:8080/mcp
```

**Over stdio**, the client starts the binary itself and talks to it through stdin and stdout. Set
`server.transport: stdio` in the config and use absolute paths, including `tools_dir`, because the client chooses
the working directory:

```bash
claude mcp add ocellus -- /path/to/bin/ocellusai-mcp -config /path/to/config.yaml
```

**Claude Desktop** and other clients configured with JSON (stdio):

```json
{
  "mcpServers": {
    "ocellus": {
      "command": "/path/to/bin/ocellusai-mcp",
      "args": ["-config", "/path/to/config.yaml"]
    }
  }
}
```

**Any other client** that supports Streamable HTTP: the server URL is `http://<host>:8080/mcp`.

The HTTP transport has no authentication. Keep it on localhost or a private network while you try it out; the
[User guide](user-guide.md#transports) explains how to run it safely.

## 7. Ask the first questions

You don't call tools by name: you ask the agent, and it picks a tool from the descriptions. Good first questions:

| Ask | The agent will likely call |
|---|---|
| "Is anything wrong in the infrastructure right now?" | `estate_overview` |
| "Show me the metrics of db-02." | `vm_metrics` |
| "Did anything unusual happen on the web servers in the last day?" | `node_deep_ensemble` |
| "Did several hosts misbehave at the same time last night?" | `node_cluster_events` |
| "Will CPU on the web servers stay under 80% tomorrow between 09:00 and 18:00?" | `node_forecast` |
| "Which pods use unusually much memory compared with their neighbours?" | `pod_memory_outliers` |
| "Why is the postgres-03 server slower than last week?" | `pg_slowdown_check` |

A typical answer is a few lines of text for the model, with the full data attached as `structuredContent`. Here is
`estate_overview` (the host and pod names are made up):

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

And a forecast from `node_forecast` for "CPU tomorrow between 09:00 and 18:00, how long above 30%":

```text
3 VM(s): cpu forecast for 03-12 09:00..03-12 18:00 UTC from 4d of history to 2026-03-12T00:00:00Z (window 1d, method L). Above 30 within the interval: 2 VM(s).
web-1:9100: mean 34.4, 28.6 at the start, 30.7 at the end; min 28.6 at 09:00, max 37 at 13:55; actual values scatter by about +-1.5 around it (noise, not the model error).
  profile (45m means): 09:00 29.5 | 09:45 31.6 | 10:30 33.4 | 11:15 34.9 | 12:00 36.1 | 12:45 36.8 | 13:30 37 | 14:15 36.8 | 15:00 36.2 | 15:45 35.1 | 16:30 33.6 | 17:15 31.7
  above 30: 8h35m in total (94% of the interval) in 1 stretch(es), first at 09:30, longest 09:30..18:05 (8h35m)
...
```

Every tool that looks at a time window takes `at` (now by default), so you can also ask about the past: "What
happened on db-01 last Tuesday around 02:00 UTC?"

## Calling tools without an agent

[`scripts/mcp-call.py`](../scripts/mcp-call.py) needs only Python 3. It starts the server over stdio, lists the tools or
calls one, and prints the answer. It needs a config with `server.transport: stdio`:

```bash
sed 's/transport: http/transport: stdio/' config.yaml > config.stdio.yaml
```

```bash
python3 scripts/mcp-call.py -quiet -config config.stdio.yaml list
```

```bash
python3 scripts/mcp-call.py -quiet -config config.stdio.yaml call vm_metrics '{"name": "db-02"}'
```

Add `--structured` to see the JSON the agent receives next to the text.

## If something does not work

| Symptom | Likely cause |
|---|---|
| The server exits at start with `tools/x.yaml: …` | A tool file is broken or uses a worker that is not configured. The message names the field. |
| `environment variables not set: …` | A `${VAR}` in the config is not set in the environment. |
| Answers are empty: "no VMs", "nothing found" | The selectors don't match your labels (step 4), or there is no data at the requested time. |
| `failed at stage worker: … connection refused` | The server cannot reach Prometheus at the configured URL. |
| `failed at stage worker: … request timed out` | A heavy query over many hosts; raise `timeout` of the Prometheus worker (15 s by default) or narrow the call. |
| The client does not see the server over stdio | Relative paths: use absolute paths for the binary, the config and `tools_dir`. |

## Next steps

- [User guide](user-guide.md): configuration, transports, the whole tool catalog, deployment to Kubernetes,
  security and troubleshooting.
- [Workers](workers.md): connect more Prometheus instances, command-line programs such as `df` or `dig`, and any
  JSON HTTP API.
- [Templates and jq](templates-and-jq.md): read and change what a tool sends and returns.
- [Processors](processors.md): how anomaly detection, outliers, incidents and forecasts work, and how to read their
  results.
- [Writing tools](writing-tools.md): design, write and test tools of your own.
