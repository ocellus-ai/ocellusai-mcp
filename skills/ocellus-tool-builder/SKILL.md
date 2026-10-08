---
name: ocellus-tool-builder
description: Designs and writes a ready-to-use tool YAML file for the Ocellus AI (ocellusai-mcp) MCP server from a user's need. Interviews the user about the question the tool must answer, the data source (Prometheus/PromQL, a shell command such as df or dig, a JSON REST API), parameters, analysis (thresholds, anomalies, outliers, forecasts) and the answer the agent should get, then writes tools/<name>.yaml and offers to validate it with `ocellusai-mcp -validate` and to test-call it. It never builds, installs, downloads or runs anything without the user's explicit permission, and works without an ocellusai-mcp checkout or binary (it then hands over the commands). Use when the user wants a new ocellusai-mcp tool, wants to adapt or fix an existing one, or asks how to expose a metric, command or API to AI agents through ocellusai-mcp. Always talk to the user in the language they write in.
---

# Ocellus AI tool builder

You turn a user's need ("I want the agent to tell me which pods eat unusually much memory") into one
working YAML file in the ocellusai-mcp tool catalog. A tool is data, not code: one file says where the data
comes from, how it is analysed and what the calling AI agent reads back.

## Language

**Talk to the user in the language of their messages.** This skill is written in English, but every
question, option, design summary, explanation and final report goes to the user in their language; if
they switch languages, switch with them. Translate the questions from [reference/interview.md](reference/interview.md),
don't paste them in English.

The tool file itself is read by AI agents, so write its `description`, parameter descriptions and
`render` text in **English** (the catalog convention) unless the user explicitly asks for another
language. Never translate YAML keys, field names, function names or identifiers.

## Ask before you run anything

This skill may be installed where there is no ocellusai-mcp at all: no repository, no binary, no config.
That is a normal situation, not a problem to fix. The deliverable is the YAML file; checking it is an
offer to the user, not something you do on your own.

Without asking you may only:

- read files that are already there (a config, existing tools, the repository's examples);
- write the tool file, at the path agreed in the spec.

**Ask first**, name the exact command and what it will do, and wait for a clear yes before you:

- **build** anything: `make build`, `go build`, `docker build`;
- **run** ocellusai-mcp in any form: `-validate`, a test call, `scripts/mcp-call.py`, a server;
- **query the user's systems**: Prometheus, an API, a command such as `dig`;
- **download or install** anything: `git clone`, `go install`, `docker pull`, `brew`, `apt`, `pip`.

A yes covers the command you named, not the next one: building and then validating are two
questions. If the user says no, or the binary or the repository is missing, do not look for a way
around it and do not fetch or build ocellusai-mcp yourself: finish the file and tell the user how they can
validate and test it themselves (step 5). If they want to install ocellusai-mcp, give them the
instructions instead of running them.

## How a tool works

```
agent's JSON arguments
  → params    JSON Schema check, defaults filled in
  → request   Go text/template over the params → a request for the worker   ┐ once (worker + request)
  → worker    prometheus (PromQL) | shell (a command) | rest (HTTP JSON)    │ or per call of `calls`,
  → jq        optional per-call jq (calls only)                             ┘ results → {name: JSON}
  → process   optional chain: jq, anomaly, outliers, join, anomaly_mv,
              anomaly_ensemble, cluster_events, ssa, ssa_mv
  → jq        optional final reshaping (gojq, $params available)
  → render    optional text for the agent (Go text/template)
```

The agent sees `name`, `description` and the JSON Schema built from `params` in `tools/list`; a call
returns the rendered text (or pretty JSON) plus `structuredContent` (the jq result). The server reads the
catalog once at startup and **one broken file stops the whole server**, so every file must pass
validation before it reaches a running server: by you with the user's permission, or by the user.

## Workflow

Copy this checklist and keep it updated while you work:

```
Tool build progress:
- [ ] 1. Orient (read only): config, worker instances, tools_dir, binary, existing tools
- [ ] 2. Interview: the need, source, scope, analysis, output, parameters
- [ ] 3. Design: pick the pattern, write the spec, user confirms it
- [ ] 4. Write: tools_dir/<name>.yaml from the closest template
- [ ] 5. Validate: offer it; run -validate only if the user agrees, otherwise hand over the commands
- [ ] 6. Test call: offer it; only with the user's permission (defaults, empty result, bad arguments)
- [ ] 7. Deliver: file, what the agent sees, sample output, how to deploy
```

### 1. Orient (read, don't run)

Before asking anything, learn what you can by reading files. Anything beyond reading, such as a query
to Prometheus, needs the user's permission (see [Ask before you run anything](#ask-before-you-run-anything)):

- **Config**: usually `config.yaml` in the server's working directory (`config.example.yaml` in a source
  checkout). Read `tools_dir` and `workers`. Each entry under `workers` is an *instance*; its `type`
  (`prometheus`, `shell`, `rest`) defaults to the entry name. Note the instance names (tools reference
  them as `worker: <name>`), the shell `allowlist`, each rest instance's base `url` and `methods`.
  Never repeat secrets (headers, tokens, `${VAR}` values) back to the user.
- **Binary**: note whether `./bin/ocellusai-mcp` or `ocellusai-mcp` on `PATH` exists, without running it. A
  source checkout without a binary would need `make build`: that is a build, so only the user decides.
  Neither may exist at all.
- **Existing tools** in `tools_dir`: avoid duplicate names and duplicate tools (offer to extend an
  existing one instead), and copy local conventions: the `job` label in selectors, the `at` parameter,
  the style of `render` text.
- **Metric and label names**: guessing is the most common cause of a tool that loads but returns
  nothing. Offer to confirm them with read-only queries against their Prometheus, and run the queries
  only if the user agrees; otherwise ask them to paste a sample series
  ([reference/prometheus.md](reference/prometheus.md#discovering-metrics-and-labels)).

If there is no ocellusai-mcp setup at hand (no repository, no binary, no config, or the user only wants
the file), carry on: ask for the worker instance names or assume `prometheus` / `shell` / `rest`, say
so, write the file, and at the end explain how they can validate and test it (step 5).

### 2. Interview

Follow [reference/interview.md](reference/interview.md). The essentials:

- Start from the **question the tool must answer**, in the user's words, with an example.
- Ask in **rounds of at most 3–4 questions**. Give options and mark a recommended default, so "you
  decide" is always a valid answer. If a structured question tool (such as AskUserQuestion) is
  available, use it for multiple-choice questions.
- Don't ask what you can look up (step 1) or what has a safe default; list the defaults in the spec.
- **Always** settle these before writing: data source and worker instance, object and how it is
  identified (labels), time scope, analysis, the answer's content, and, for any detector, the
  smallest change worth reporting in real units (`min_delta`).
- A tool that **changes state** (a non-GET REST call, a mutating command) needs the user's explicit
  confirmation of the side effect; prefer a read-only tool when it answers the need.

### 3. Design

Pick the pattern and the closest template:

| The need | Pattern | Start from |
|---|---|---|
| Current value / top-N of a metric | prometheus `instant` → jq → render | [templates/prometheus-snapshot.yaml](templates/prometheus-snapshot.yaml) |
| Several metrics of one object in one answer, peak or time above a threshold over a window | one `instant` query with `label_replace` + `or`, subqueries | [reference/prometheus.md](reference/prometheus.md#recipes) |
| "Anything unusual in each object's own history?" | `range` → `anomaly` (zscore / iqr) | [templates/prometheus-anomaly.yaml](templates/prometheus-anomaly.yaml) |
| "Which object stands out among its peers right now?" | `instant` → jq → `outliers` | [templates/prometheus-outliers.yaml](templates/prometheus-outliers.yaml) |
| Anomalous time segments, which metric is to blame, several signals per object | `calls` (one `range` per signal) → `join` → `anomaly_ensemble` | [templates/multi-metric-ensemble.yaml](templates/multi-metric-ensemble.yaml) |
| The usual relation between two metrics broke | `calls` → `join` → `anomaly_mv` | [reference/processors.md](reference/processors.md#anomaly_mv) |
| The same incident on many hosts at once | … → `anomaly_ensemble` → jq → `cluster_events` | [reference/processors.md](reference/processors.md#cluster_events) |
| Trend, daily cycle, forecast, capacity | `range` → `ssa` (one metric) or `join` → `ssa_mv` | [templates/prometheus-forecast.yaml](templates/prometheus-forecast.yaml) |
| Output of a CLI (df, dig) | shell → jq → render | [templates/shell-df.yaml](templates/shell-df.yaml) |
| A list from one source, metrics for it from another | `calls`: shell → prometheus, joined in jq | [templates/calls-dig-prometheus.yaml](templates/calls-dig-prometheus.yaml) |
| Read a JSON API | rest `GET` | [templates/rest-read.yaml](templates/rest-read.yaml) |
| Do something through an API (an action) | rest `POST`/`PUT`/`PATCH`/`DELETE` | [templates/rest-action.yaml](templates/rest-action.yaml) |

Then write a short **spec** in the user's language (format in [reference/interview.md](reference/interview.md#the-spec))
and get a clear "yes" before writing the file. A one-line change to an existing tool needs no spec.

### 4. Write the file

Copy the closest template, adapt it, and follow the reference for each section:

| Section | Reference |
|---|---|
| File structure, `name`, `description`, `params`, templates and their functions | [reference/tool-format.md](reference/tool-format.md) |
| `worker: prometheus`, PromQL recipes, the `at` parameter | [reference/prometheus.md](reference/prometheus.md) |
| `worker: shell` and `worker: rest` | [reference/shell-and-rest.md](reference/shell-and-rest.md) |
| `calls` (several workers in sequence) | [reference/calls.md](reference/calls.md) |
| `process` and every processor | [reference/processors.md](reference/processors.md) |
| `response.jq` and `response.render` | [reference/response.md](reference/response.md) |

Every template here passes `-validate` and has been called against real data or mocks. More real
examples, if the ocellusai-mcp repository is at hand: `tools/` (working catalog: estate overview, VM
anomaly ensembles, SSA forecasts, PostgreSQL, Kafka) and `tools-test/` (small tools, one feature each).

Save it as `<tools_dir>/<name>.yaml`, file name equal to `name`. Start the file with a short `#`
comment: what it does and what it assumes about the data (exporter, `job` label), so the next person
can adapt it.

### 5–7. Validate, test, deliver

`-validate` catches structural mistakes; only a test call catches wrong field names, PromQL errors and
jq/render failures. Both run software, and a test call touches the user's systems, so **offer them,
don't just do them**:

1. When the file is written, tell the user it can be checked: `-validate` (offline, it loads the
   catalog and contacts nothing) and a test call (it queries their real systems). Ask whether they want
   you to run either, naming the exact commands.
2. Run only what they agreed to. If the binary has to be built first, ask about the build separately.
3. If they decline, or there is no binary or repository, do not install, download or build anything:
   give them the commands for their setup and what to look for in the output, and offer to read the
   output if they paste it.
4. Never call an action tool without explicit permission for those exact arguments.

Details, commands and the error table: [reference/validate-and-deliver.md](reference/validate-and-deliver.md).
Finish with the file path, what the agent will see, a sample answer (or the expected shape if nothing
was run), what was validated or tested and what is left to the user, assumptions to check and how to
deploy (the server reads the catalog only at startup).

## Rules that prevent most broken tools

- **Strict parsing.** Only documented fields exist; an unknown field anywhere (`title`, `annotations`,
  `items` in a param) stops the server.
- **Quote every YAML value that starts with `{{`**: `step: "{{ .step }}"`.
- **PromQL values go through helpers**: label values `{{ promLabel .x }}`, literals inside your own
  regex `promRegex`, durations `[{{ promDuration .window }}]`, numbers come from `integer`/`number`
  params.
- **Every parameter that reaches a query or command is constrained**: `pattern` anchored with `^…$`,
  `enum`, `minimum`/`maximum`. Shell arguments forbid a leading `-`.
- **Shell**: no shell at all (no pipes, globs, `$VAR`); each `args` element is exactly one argument;
  the command must be in the instance's allowlist.
- **REST**: never a free URL; path values through `pathEscape`; query parameters in `query`, not in
  `path`.
- **Detectors** (`anomaly`, `outliers`, `anomaly_mv`, `anomaly_ensemble`) always get `min_delta`: the
  smallest meaningful change in the metric's units, exposed as a parameter in units the agent
  understands.
- **jq**: wrap results in `[...]` or `{...}`, keep every key present (null when unknown), use
  `tonumber? // null`, sort and cap lists, don't round.
- **render**: handle the empty result first, print nil as `n/a`, use `printf "%.2f" (float64 .x)`,
  name the window, threshold and units in the text.
- **description** says what is returned, from which source, in which units, what an empty answer
  means and when to use the tool. It is the only thing the agent reads before calling.
