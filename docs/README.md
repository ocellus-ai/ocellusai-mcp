# Ocellus AI

**Ready-made DevOps tools for AI agents, with the data analysis done on the server.**

Ocellus AI is an [MCP](https://modelcontextprotocol.io) server written in Go. Each tool is one YAML file that says
where to get the data, how to shape and analyse it, and what the agent gets back. Instead of a raw PromQL endpoint
and thousands of samples, the agent calls a typed tool and reads a few lines of findings.

## What the agent sees

A tool that joins CPU and memory per VM and runs the anomaly ensemble over them
([`vm_anomaly_ensemble.yaml`](https://github.com/ocellus-ai/ocellusai-mcp/blob/main/internal/processor/ensemble/testdata/vm_anomaly_ensemble.yaml)).
The agent calls it with `{"instance": "vm-.*", "range": "12h"}` and receives:

```text
1 anomalous segment(s) over the last 12h (2 VM(s) analyzed, 0 skipped; window 1h, risk 0.0001):
vm-1:9100: 1 segment(s), max score 70.2
  - 2025-09-11T22:33:20Z .. 2025-09-11T22:53:20Z (21 samples, single, score 70.2; fired by pca(k=0)), driven by mem (own z 20.6, PCA share 0.49); mem 60.4% at the peak vs 44.9% usually
```

Two VMs, two metrics and 720 one-minute samples each went in; one line came out. It says when, which metric, how far
it moved from its usual level and which detector fired. The full report is returned as `structuredContent` for
agents that want to dig further.

## Where to start

| You want to | Read |
|---|---|
| know what it gives an infrastructure team, in plain terms | [For DevOps engineers](for-devops.md) |
| get an agent asking your Prometheus questions in fifteen minutes | [Quick start](quick-start.md) |
| install, configure and run the server, and understand its answers | [User guide](user-guide.md) |
| connect Prometheus, command-line tools or a JSON HTTP API | [Workers](workers.md) |
| read and change what a tool asks and answers | [Templates and jq](templates-and-jq.md) |
| understand the anomaly detection, outliers and forecasts | [Processors](processors.md) |
| write a tool of your own | [Writing tools](writing-tools.md) |
| work on the server itself | [Architecture](architecture.md) and [Components](components.md) |
| add a new kind of data source or analysis in Go | [Writing a worker](writing-workers.md) and [Writing a processor](writing-processors.md) |

The source code, the tool catalog and the deployment files are on
[GitHub](https://github.com/ocellus-ai/ocellusai-mcp).
