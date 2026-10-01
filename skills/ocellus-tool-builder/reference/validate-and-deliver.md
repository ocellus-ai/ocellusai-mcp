# Validate, test, deliver

## Contents

- [Validate](#validate)
- [Test call](#test-call)
- [Errors and what they mean](#errors-and-what-they-mean)
- [Checklist](#checklist)
- [Deliver](#deliver)

## Validate

`-validate` loads the config and the whole catalog with the same code as a server start and prints
one line per tool. It is offline: it does not contact Prometheus, run commands or call APIs.

```bash
./bin/ocellus-ai -config config.yaml -validate
```

(In a source checkout `make build` builds `bin/ocellus-ai`; `make validate` validates the example
config.) Look for your tool's line, e.g.
`node_memory_top  worker=prometheus  type=prometheus  calls=-  process=-  file=tools/node_memory_top.yaml`.
With `calls` and `process` the columns show the chains (`calls=cpu>mem`, `process=join>anomaly_ensemble`).

**Isolated validation**, when the existing catalog has unrelated problems or you must not touch the
user's config: copy the config to a scratch directory, point `tools_dir` at a directory holding only
the new file, and validate that. The config must still define every worker instance the tool uses.
`${VAR}` placeholders in the config must be set in the environment (an unset variable stops the
start); for validation only, dummy values are fine.

```bash
mkdir -p /tmp/ocellus-check/tools && cp tools/node_memory_top.yaml /tmp/ocellus-check/tools/
sed 's#^tools_dir:.*#tools_dir: /tmp/ocellus-check/tools#' config.yaml > /tmp/ocellus-check/config.yaml
./bin/ocellus-ai -config /tmp/ocellus-check/config.yaml -validate
```

`-validate` checks structure, parameter schemas, request keys, template syntax and function names,
jq syntax and `result` references. It does **not** check field names inside templates, PromQL,
commands, real data or the answer's formatting. Only a call does.

## Test call

Ask the user before calling: the tool queries their real systems. **Never call an action tool**
(non-GET REST, a mutating command) without explicit permission for the exact arguments.

The repository ships a stdlib-only client, `scripts/mcp-call.py`. It starts the server over stdio, so
it needs a config with `server.transport: stdio`; make a scratch copy rather than editing the user's
config:

```bash
sed 's#^\([[:space:]]*transport:\).*#\1 stdio#' /tmp/ocellus-check/config.yaml > /tmp/ocellus-check/stdio.yaml
python3 scripts/mcp-call.py -bin ./bin/ocellus-ai -config /tmp/ocellus-check/stdio.yaml -quiet list
python3 scripts/mcp-call.py -bin ./bin/ocellus-ai -config /tmp/ocellus-check/stdio.yaml -quiet call node_memory_top '{"top": 3}' --structured
```

Use absolute paths for `tools_dir` in a scratch config if you run from another directory. Without the
script, any MCP client works (for example `claude mcp add ocellus -- /path/to/bin/ocellus-ai -config
/path/to/stdio.yaml`).

To see the rendered request (the final PromQL, command line or URL path), set `log.level: debug` in
the scratch config and drop `-quiet`: every call logs `request rendered`. Paste the PromQL into the
Prometheus UI or query it with curl to debug it apart from the tool.

What to call:

- defaults only (just the required arguments);
- an empty result (a filter that matches nothing): a clear sentence, not an error;
- invalid arguments (a bad duration, an extra key): an error at stage `arguments`, not `worker`;
- an object with part of the metrics missing: `n/a`, not a `render` failure;
- the biggest realistic window on the real fleet: time it (the Prometheus timeout is 15 s by default);
- for detectors: `min_delta: 0` against the default, to see how much noise the default removes;
- for `calls`: the first call returning an empty list must not widen the second request.

If the data source is not reachable from where you run, say so, give the user the exact commands, and
ask them to paste the output.

## Errors and what they mean

### At startup (`-validate`, server start)

Format: `<file>: <field>: <reason>`.

| Message | Cause |
|---|---|
| `tools/x.yaml: name: "Pod-CPU" must match ^[a-z0-9_]+$` | capitals or a dash in the name |
| `tools/x.yaml: description: required (it is what the agent sees)` | no description |
| `… field descripton not found in type catalog.Spec` | a typo or an unsupported field |
| `tools/x.yaml: worker: unknown worker "shell" (configured: prometheus)` | no such instance in the config |
| `tools/x.yaml: params.top: required and default are mutually exclusive` | both set |
| `tools/x.yaml: params.window.pattern: only valid for type string` | `pattern` on a non-string |
| `tools/x.yaml: params: …` | a `default` fails its own schema |
| `tools/x.yaml: request.time: only valid for type: instant` | instant and range keys mixed |
| `tools/x.yaml: request.command: "curl" is not in workers.shell.allowlist` | command not allowed |
| `tools/x.yaml: request.method: DELETE is not allowed by this worker instance (methods: GET)` | read-only rest instance |
| `tools/x.yaml: request.path: must not contain ? or #` | rest query parameters belong in `query` |
| `… template request.query: … function "promLable" not defined` | a typo in a function name |
| `tools/x.yaml: response.jq: jq parse: …` | jq syntax |
| `tools/x.yaml: process[0] (anomaly): with.method: must be a literal, …` | `method` as a template |
| `tools/x.yaml: process[1] (anomaly_mv): method mahalanobis: with: unknown field(s) …` | a key the processor or detector does not know |
| `tools/x.yaml: calls[1].name: duplicate call name "pods" (also calls[0])` | duplicate call names |
| `tools/x.yaml: calls[0].request: result "cpu": no call result is available here` | `result` refers to a later or unknown call |
| `tools/x.yaml: worker: not allowed together with calls (set calls[i].worker instead)` | both forms |
| a YAML parse error with a line number | usually an unquoted value starting with `{{` |

### At call time (`tool <name> failed at stage <stage>: …`)

With `calls`, errors of the stages `request`, `worker` and per-call `jq` start with `calls[i] (name): `.

| Stage | Typical causes |
|---|---|
| `arguments` | a missing required parameter, an extra key, a wrong type, `pattern`/`enum`/bounds failed |
| `request` | a typo in a field name (`map has no entry for key`), `param` of an undeclared parameter, an invalid duration in `promDuration`, `fail` in a template |
| `worker` | PromQL syntax (`prometheus bad_data error`), timeout, a bad `time`, a command's non-zero exit or timeout; rest: non-2xx, redirect, non-JSON, over the size limit |
| `process` | wrong input shape (an instant query into `anomaly`; a matrix into `anomaly_mv`/`anomaly_ensemble` without `join`; two series per `join_by` value; not a `[{key, value}]` list for `outliers`; not an event list for `cluster_events`: a missing `jq` step), `with.window: must be at least 4 points`, a non-numeric `value` in `outliers`, a bad value rendered into `with`, `process[i] (jq): jq: …` |
| `jq` | iterating over `null` (an expected field is missing), `tonumber` on `NaN`, a worker that returned a string instead of JSON |
| `render` | a missing key, `printf` with the wrong type, arithmetic on nil |

## Checklist

**Description and parameters**

- [ ] `name` follows `<object>_<what>`, the file has the same name, no clash with existing tools.
- [ ] `description`: what is returned, data source, units, the empty result, when to use, limits;
      actions say "This is an action".
- [ ] Every parameter has a `description` with an example.
- [ ] Everything that reaches a request is constrained by an anchored `pattern`, `enum` or bounds.
- [ ] Defaults give a useful answer, not noise.

**Request**

- [ ] Label values through `promLabel`, regex literals through `promRegex`, durations through
      `promDuration`.
- [ ] Values starting with `{{` are quoted.
- [ ] Filtering and aggregation happen in PromQL; the answer size is bounded.
- [ ] Denominators are protected from zero.
- [ ] Shell: one argument per element, no leading `-` allowed, `timeout` in Go format, command in
      the allowlist.
- [ ] REST: path values through `pathEscape` and `pattern`, query parameters in `query`, string bodies
      use `toJson`; actions return `status`.
- [ ] `calls`: the first call has a flat-shape `jq`; an empty first result does not widen the second
      request (`default "__none__"`); the description names both sources.
- [ ] Detectors: `min_delta` set (a parameter in natural units, converted in the template), a shift
      from the expected value, `max_anomalies` not 0 (except `anomaly_mv` with a jq post-filter);
      `raw_total` from `below_min_delta` in the answer; the threshold named in description and text.

**Answer**

- [ ] jq wraps the result in `[...]` or `{...}`; the shape is stable; all keys present.
- [ ] `tonumber? // null` where `NaN`/`Inf` may appear.
- [ ] `render` handles the empty result, nil (`n/a`) and small values; numbers through `float64`.
- [ ] The text names units, window and threshold, and how many objects were analysed/skipped.

**Verification**

- [ ] `-validate` passes.
- [ ] A call with defaults, an empty result and invalid arguments has been tried (or the user has the
      commands to try them).
- [ ] No temporary test values (`time: now-1d`, a narrowed filter) left in the file.

## Deliver

Tell the user, in their language:

1. **Where the file is** (`tools/<name>.yaml`) and that `-validate` passed (or what could not be run).
2. **What the agent sees**: the name, the first sentence of the description, the parameters with
   defaults.
3. **A sample answer** from the test call (a few lines), or the expected shape if no call was made.
4. **Assumptions to check**: metric names, `job` labels, units, the worker instance, the environment
   of a shell command.
5. **How to deploy**: the server reads the catalog only at startup.
   - Local: restart ocellus-ai.
   - Kubernetes: rebuild the tools ConfigMap from the directory and restart:

     ```bash
     kubectl -n monitoring create configmap ocellus-ai-tools --from-file=tools/ --dry-run=client -o yaml | kubectl apply -f -
     ```

     ```bash
     kubectl -n monitoring rollout restart deployment/ocellus-ai
     ```

     Keep only YAML in the directory (no `.DS_Store`, archives, drafts); ConfigMap keys allow only
     `[-._a-zA-Z0-9]`, so file names must be ASCII; the ConfigMap is limited to 1 MiB.
   - A shell tool in Kubernetes needs an image with the command (`with-kubectl` target), the command in
     the allowlist and its environment in the instance's `env`.
   - A new worker instance (a REST API, a second Prometheus) is a config change: give the snippet.

Offer the next step in one line (e.g. a sibling tool, a tighter default) instead of building it
unasked.
