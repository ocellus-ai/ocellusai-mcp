# Workers

[For DevOps](for-devops.md) · [Quick start](quick-start.md) · [User guide](user-guide.md) · **Workers** · [Templates and jq](templates-and-jq.md) · [Processors](processors.md) · [Writing tools](writing-tools.md)

A worker is how Ocellus AI reaches a data source. This guide explains how to connect each kind of source in the
config, what a tool can ask of it, what comes back and which errors to expect.

There are three worker types:

| Type | Talks to | Typical sources |
|---|---|---|
| [`prometheus`](#prometheus) | the Prometheus HTTP query API | Prometheus, VictoriaMetrics, Thanos, Mimir, Cortex |
| [`shell`](#shell) | command-line programs on the server's host | `df`, `dig` |
| [`rest`](#rest) | one JSON HTTP API under a base URL | Alertmanager, GitLab, Grafana, an internal API |

## Contents

- [Types and instances](#types-and-instances)
- [prometheus](#prometheus)
- [shell](#shell)
- [rest](#rest)
- [Several sources in one tool](#several-sources-in-one-tool)
- [Error reference](#error-reference)

## Types and instances

A **type** is code built into the server. An **instance** is one configured connection of a type: an entry under
`workers` in the config. Tools name the instance they use, never a URL or a credential.

```yaml
workers:
  prometheus:                 # instance "prometheus"; the type defaults to the entry name
    url: http://prometheus.monitoring:9090
  prom_staging:               # a second Prometheus under its own name
    type: prometheus
    url: http://prometheus.staging:9090
  shell:                      # instance "shell" of type shell
    allowlist: [df]
  dns:                        # another shell instance with its own allowlist and limits
    type: shell
    allowlist: [dig]
    timeout: 10s
  alertmanager:               # a rest instance
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
```

```yaml
# in a tool file
worker: prom_staging
```

Rules:

- An instance name may contain letters, digits, `_` and `-`.
- `type` can be left out when the entry is named after its type (`prometheus`, `shell`, `rest`).
- An entry with no value (`prom_staging:` followed by nothing) is disabled.
- At least one instance is required.
- A tool that refers to an instance that does not exist stops the start:
  `tools/x.yaml: worker: unknown worker "rest" (configured: prometheus, shell)`. Keep the config and the catalog
  consistent: when you remove an instance, remove the tools that use it.

Instances of one type are independent: each has its own URL, credentials, limits and allowlist. Use this to separate
environments (production and staging Prometheus), sets of programs (one `shell` instance per allowlist), or
permissions (a read-only and a writable instance of the same API).

## prometheus

### Connecting

```yaml
workers:
  prometheus:
    url: http://prometheus.monitoring:9090   # required
    timeout: 15s                             # per query; default 15s
    headers:                                 # optional, sent with every query, never logged
      Authorization: "Bearer ${PROM_TOKEN}"
```

| Key | Default | Meaning |
|---|---|---|
| `url` | — | Base URL of the API, without `/api/v1`. A path prefix is kept: `http://host/prometheus`. |
| `timeout` | `15s` | Time limit of each query. Fleet-wide range queries of the analysis tools may need `60s`. |
| `headers` | none | Static headers: authentication, tenant ids. Values can use `${VAR}`. |

The worker uses the standard HTTP proxy variables (`HTTPS_PROXY`, `NO_PROXY`) and the system's trusted certificates.
It has no option for a private CA or for skipping TLS verification: add the CA to the trust store of the host or
image (on Linux, `SSL_CERT_FILE` or `SSL_CERT_DIR` also work).

### Compatible backends

Anything that serves `/api/v1/query` and `/api/v1/query_range` the way Prometheus does:

| Backend | `url` | Headers |
|---|---|---|
| Prometheus | `http://prometheus:9090` | — |
| VictoriaMetrics, single node | `http://victoriametrics:8428` | — |
| VictoriaMetrics cluster | `http://vmselect:8481/select/0/prometheus` (0 is the tenant) | — |
| Thanos Query | `http://thanos-query:9090` | — |
| Grafana Mimir, Cortex | `http://mimir-query-frontend:8080/prometheus` | `X-Scope-OrgID: <tenant>` |
| A managed service with basic auth | its query URL | `Authorization: "Basic ${PROM_BASIC}"` (base64 of `user:password`) |

### Authentication examples

```yaml
workers:
  prometheus:
    url: https://prometheus.example.com
    headers:
      Authorization: "Bearer ${PROM_TOKEN}"
  mimir:
    type: prometheus
    url: https://mimir.example.com/prometheus
    headers:
      X-Scope-OrgID: production
      Authorization: "Basic ${MIMIR_BASIC}"
```

### What a tool asks for

The `request` of a Prometheus tool:

```yaml
request:
  type: range                       # instant (default) | range
  query: |
    sum by (pod) (rate(container_cpu_usage_seconds_total{namespace={{ promLabel .namespace }}}[5m]))
  start: "now-{{ promDuration .range }}"
  end: now
  step: 60s
```

| Key | Query type | Default | Meaning |
|---|---|---|---|
| `type` | — | `instant` | `instant` evaluates once; `range` returns a series of samples. |
| `query` | both | required | PromQL. |
| `time` | instant | now | The evaluation time. |
| `start` | range | `now-1h` | Start of the range. |
| `end` | range | `now` | End of the range, after `start`. |
| `step` | range | `60s` | Resolution: `30s`, `1m`, or plain seconds `15`. |

Every key can be a template over the tool's parameters ([Templates and jq](templates-and-jq.md)). Mixing keys of the
two types (`time` in a range query) is a startup error. Times accept `now`, `now-5m`, `now+1h`, RFC 3339
(`2026-03-12T02:00:00Z`) and unix seconds.

To look at the past, tools evaluate the whole query at that moment (`time:` or `end:` set from an `at` parameter)
instead of adding `offset` to selectors, so expressions such as uptime stay correct.

### What comes back

The answer has the shape of the Prometheus HTTP API, so the usual Prometheus idioms work in jq. **Values are
strings**; timestamps are seconds.

```json
{"resultType": "vector",
 "result": [{"metric": {"instance": "web-1:9100", "job": "node"}, "value": [1773316800, "0.25"]}]}
```

```json
{"resultType": "matrix",
 "result": [{"metric": {"pod": "api-1"}, "values": [[1773316800, "0.2"], [1773316860, "0.3"]]}]}
```

A scalar is `{"resultType": "scalar", "result": [1773316800, "42"]}`. Values can be `"NaN"`, `"+Inf"` and `"-Inf"`.
Warnings from Prometheus do not fail the call; they go to the server log.

A `range` result (a matrix) is also the input of the time-series processors (`anomaly`, `join`, `ssa`); see
[Processors](processors.md#data-shapes).

### Errors

| Message | Cause |
|---|---|
| `prometheus bad_data error: …` | PromQL syntax or an invalid parameter, as Prometheus reports it |
| `prometheus execution error: …` | the query failed in Prometheus, for example it hit a sample limit |
| `prometheus: request timed out: …` | the query took longer than `timeout` |
| `prometheus: Post "…": dial tcp …: connection refused` | Prometheus is not reachable at `url` |

## shell

The `shell` worker runs a command-line program on the server's host and returns its output. It is how tools use
`df`, `dig` and similar programs.

### Connecting

```yaml
workers:
  shell:
    allowlist: [df, dig]         # required: the only programs tools may run
    timeout: 30s                 # per command; default 30s
    max_output_bytes: 1048576    # stdout limit; default 1 MiB
    env:                         # the child's environment: PATH plus these, nothing else
      TZ: UTC
```

| Key | Default | Meaning |
|---|---|---|
| `allowlist` | — | Program names or absolute paths. A tool's `command` must be one of them exactly. A name is looked up in the `PATH` of the server process. |
| `timeout` | `30s` | Time limit of each command. A tool can set its own. |
| `max_output_bytes` | 1 MiB | Larger stdout is truncated. |
| `env` | none | Environment of the child process. `PATH` is the server's own unless you set it here; it affects what the program sees, not where the program itself is found. Values can use `${VAR}`. |

### How a command runs

- **No shell.** The program is started directly, without `sh -c`. Pipes, redirections, `&&`, globs, `~` and `$VAR`
  in arguments have no special meaning: they are passed as literal text. A tool argument cannot start a second
  command.
- **Each argument is exactly one argument.** A value with spaces stays one argument.
- **A clean environment.** The child gets `PATH` and the variables in `env`, nothing else: no `HOME`, no locale,
  no credentials from the server's environment. Whatever the program needs goes into `env`.
- **Limits.** The command is killed after `timeout`. Stdout beyond `max_output_bytes` is cut (a truncated JSON object
  gets `"truncated": true`); stderr is kept up to 64 KiB for error messages.
- **Exit status.** A non-zero exit is an error that includes stderr. With exit 0, stderr only goes to the log.

Never put a shell (`sh`, `bash`) or an interpreter (`python`, `perl`) in an allowlist: tools would be able to run
anything, and the agent's arguments would become code. Prefer read-only commands that need no credentials: a command
that changes state runs every time an agent calls the tool.

### In a container

The default image is distroless and has no command-line programs, so a `shell` tool fails there with
`executable file not found`. The `ocellusai-mcp` binary is static: copy it into any base image that has the programs
your tools run.

```dockerfile
FROM ocellusai-mcp:dev AS ocellus

FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends bind9-dnsutils \
    && rm -rf /var/lib/apt/lists/*
COPY --from=ocellus /app /app
USER 65532:65532
ENTRYPOINT ["/app/ocellusai-mcp"]
CMD ["-config", "/app/config.yaml"]
```

`df` is part of the base image; `bind9-dnsutils` adds `dig`. Add the programs to the allowlist of the instance.

### What a tool asks for

```yaml
worker: shell
request:
  command: dig                                        # must be in the instance's allowlist
  args: [+short, A, "{{ .name }}"]                    # one element = one argument, each a template
  parse: lines                                        # json (default) | lines | raw
  timeout: 10s                                        # optional, a Go duration
```

| Key | Default | Meaning |
|---|---|---|
| `command` | required | A program from the allowlist; checked at startup and at every call. |
| `args` | none | Arguments; each element is one argument and its own template. Numbers and booleans become strings. |
| `stdin` | empty | Text for standard input, a template. |
| `parse` | `json` | How stdout becomes data for the rest of the tool (below). |
| `timeout` | the instance's | A Go duration: `20s`, `1m30s`, `24h` (not `1d`). |

### What comes back

| `parse` | Result | Notes |
|---|---|---|
| `json` | the parsed JSON | empty stdout → `null`; output that is not JSON → one string (and a warning in the log), not an error |
| `lines` | a list of strings, one per line | the final newline and `\r` are removed, empty lines are kept |
| `raw` | one string | the whole stdout |

For example, `df -P` with `parse: lines` gives `["Filesystem 1024-blocks Used Available Capacity Mounted on", "/dev/sda1 …", …]`,
which a jq expression splits into fields, and `dig +short A <name>` gives the CNAME chain and the addresses, one
per line. Use `parse: json` for programs that print JSON.

A parameter that ends up in `args` needs a `pattern`: a value that starts with `-` becomes an option of the program
(`-f /etc/passwd`), and for `dig` so do `@…` and `+…`. A host name pattern such as
`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.?$` rules them all out.

### Errors

| Message | Cause |
|---|---|
| `request.command: "dig" is not in workers.shell.allowlist` | the command is not allowed for this instance (at startup) |
| `exited with code 1: <stderr>` | the program failed; stderr explains why |
| `timed out after 30s: <stderr>` | the program ran longer than the timeout |
| `command "dig": exec: "dig": executable file not found in $PATH` | the program is not installed in the image or on the host, or not on the server's `PATH`: put its absolute path in the allowlist and in the tools |
| an error at stage `jq` such as `cannot iterate over string` | the program printed text but the tool expected JSON |

## rest

The `rest` worker sends one HTTP request to a JSON API and returns the response. The base URL, credentials and
limits belong to the instance; a tool gives only a path relative to the base URL and, if it needs them, a method,
query parameters, headers and a body. A tool cannot choose a host.

### Connecting

```yaml
workers:
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2   # required: http or https, no ? or #
    timeout: 15s                                      # default 15s
    headers:                                          # sent with every request, never logged
      Authorization: "Bearer ${AM_TOKEN}"
    max_response_bytes: 1048576                       # default 1 MiB; larger is an error
    methods: [GET]                                    # allowed methods; default all five
    # ca_file: /etc/ssl/internal-ca.pem               # PEM bundle for a private CA
    # insecure_skip_verify: false
```

| Key | Default | Meaning |
|---|---|---|
| `url` | — | Base URL: scheme `http` or `https`, a host, optionally a path. No query string or fragment. |
| `timeout` | `15s` | Time limit of each request. A tool can set its own. |
| `headers` | none | Sent with every request. They override headers a tool sets, so a tool cannot replace the credentials. |
| `max_response_bytes` | 1 MiB | A larger response is an error (JSON cannot be cut safely). |
| `methods` | `GET, POST, PUT, PATCH, DELETE` | The methods tools may use. `[GET]` makes the instance read-only. |
| `ca_file` | none | A PEM file with the CA certificates to trust, read at startup. |
| `insecure_skip_verify` | `false` | Turn off TLS verification. Only for testing. |

The URL, `ca_file` and methods are checked at startup. The worker uses the standard proxy variables (`HTTPS_PROXY`,
`NO_PROXY`).

### Examples

Alertmanager, read-only, for listing alerts and silences:

```yaml
workers:
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
    methods: [GET]
```

GitLab with a personal or project access token:

```yaml
workers:
  gitlab:
    type: rest
    url: https://gitlab.example.com/api/v4
    headers: {PRIVATE-TOKEN: "${GITLAB_TOKEN}"}
    methods: [GET]
```

Grafana with a service account token:

```yaml
workers:
  grafana:
    type: rest
    url: https://grafana.example.com/api
    headers: {Authorization: "Bearer ${GRAFANA_TOKEN}"}
    methods: [GET]
```

A read-only and a writable instance of the same API, so that only the tools meant to act can act:

```yaml
workers:
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
    methods: [GET]
  alertmanager_rw:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2
    methods: [GET, POST, DELETE]
```

The reference tools [`tools-test/alertmanager_*.yaml`](../tools-test) use an instance named `rest` pointing at the
Alertmanager v2 API: one lists silences, one creates a silence, one expires a silence.

### What a tool asks for

```yaml
worker: gitlab
request:
  path: /projects/{{ pathEscape .project }}/pipelines
  query:
    status: "{{ .status }}"     # dropped when it renders empty
    per_page: 50
```

| Key | Default | Meaning |
|---|---|---|
| `method` | `GET` | `GET`, `POST`, `PUT`, `PATCH`, `DELETE`. Written literally, not as a template; must be allowed by the instance. |
| `path` | required | Relative to the instance URL, a template. No `?` or `#`; cannot leave the base path with `..`. |
| `query` | none | Query parameters: `name: value`. A value is a template, a number, a boolean or a list (the name repeats). A value that renders to an empty string is not sent. |
| `headers` | none | Extra headers, templates. The instance's headers win on a clash. |
| `body` | none | A YAML structure (sent as JSON) or a string template (sent as is). Not allowed with `GET`. |
| `timeout` | the instance's | A Go duration. |
| `parse` | `json` | `json`: the body must be JSON; `raw`: the body as one string. |
| `result` | by method | `body`: the parsed body. `full`: `{status, headers, body}`. `GET` defaults to `body`, the other methods to `full`. |
| `ok_status` | none | Status codes to accept in addition to 2xx, such as `[404]`. |

Values placed into the path go through `pathEscape`, so a `/` or `..` in a value cannot change which endpoint is
called. Query parameters are encoded by the worker; don't build them into the path.

### What comes back

- **Success** is a 2xx status or a code from `ok_status`. Anything else is an error with the method, the path, the
  status and the start of the response body.
- **`result: body`** (the default for `GET`): the parsed JSON body. An empty body (`204`) is `null`.
- **`result: full`** (the default for other methods): `{"status": 200, "headers": {"content-type": "…"}, "body": …}`,
  with lower-case header names. Actions keep the status so the agent knows whether they worked.
- The body is parsed as JSON whatever its `Content-Type`. For a text API use `parse: raw`.

### Actions

Methods other than `GET` change things in your systems, every time an agent calls the tool. The worker never
retries a request. Good action tools say "This is an action" in their description and return the status. Keep
actions on a separate instance that allows only the methods they need, and leave every other instance at
`methods: [GET]`.

### Errors

| Message | Cause |
|---|---|
| `request.method: DELETE is not allowed by this worker instance (methods: GET)` | a tool uses a method the instance does not allow (at startup) |
| `request.path: must not contain ? or #` | query parameters belong in `query` |
| `request.path: "…" leaves the instance url path /api/v2` | `..` would climb above the base path |
| `GET /api/v2/x: 404 Not Found: {"error": …}` | the API answered with an error status; if it is expected, add it to `ok_status` |
| `GET /api/v2/x: 302 Found (redirects are not followed; Location: …)` | the URL redirects: put the final URL into the instance |
| `GET /api/v2/x: response is not valid JSON (content-type "text/html") …` | a wrong path, a login page or a proxy answered; for text APIs use `parse: raw` |
| `GET /api/v2/x: response exceeded 1048576 bytes` | the answer is too large: filter on the API side or raise `max_response_bytes` |
| `GET /api/v2/x: request timed out after 15s` | the API is slow: raise `timeout` |

Redirects are not followed on purpose: following one would send the instance's credentials to whatever host the
redirect names.

## Several sources in one tool

A tool can make several calls in sequence, to different instances and types, with `calls` instead of
`worker` + `request`:

```yaml
calls:
  - name: addrs                       # dig resolves the name
    worker: shell
    request:
      command: dig
      args: [+short, A, "{{ .name }}"]
      parse: lines
    jq: '[.[] | select(test("^[0-9]+(\\.[0-9]+){3}$"))] | unique'
  - name: up                          # Prometheus is asked about exactly those addresses
    worker: prometheus
    request:
      query: |
        {{- $addrs := list }}{{ range result "addrs" }}{{ $addrs = append $addrs (promRegex .) }}{{ end -}}
        up{instance=~{{ promLabel (printf "(%s)(:[0-9]+)?" (join "|" $addrs | default "__none__")) }}}
```

- Calls run one after another; a later call can use the results of earlier ones through `result "name"`.
- The first failing call stops the tool; its error names the call: `calls[1] (up): …`.
- The time of the tool is the sum of its calls, and each call has the timeout of its instance.
- The results reach the rest of the tool as an object `{addrs: …, up: …}`.

The whole tool, with the join of both results by address, is
[`tools-test/dns_targets_up.yaml`](../tools-test/dns_targets_up.yaml).

Typical uses: a list from one system and metrics from another; the same query against production and staging; one
range query per metric, stitched together by the `join` processor for multi-metric analysis. How the templates and
jq of such tools work is in [Templates and jq](templates-and-jq.md#results-of-earlier-calls).

## Error reference

Errors at **startup** name the file and the field and stop the server. Errors during a **call** come back to the
agent as `tool <name> failed at stage <stage>: …`; source errors are at stage `worker`.

| Worker | At startup | During a call |
|---|---|---|
| any | `worker: unknown worker "x" (configured: …)`, `workers.<name>.url: required` | — |
| prometheus | `request.time: only valid for type: instant` | `prometheus bad_data error: …`, `prometheus: request timed out`, connection errors |
| shell | `request.command: "x" is not in workers.shell.allowlist`, `workers.<name>.allowlist: must list at least one binary` | `exited with code N`, `timed out after …`, `executable file not found` |
| rest | `workers.<name>: rest: …` (bad URL or `ca_file`), `request.method: … is not allowed`, `request.path: …` | `METHOD /path: <status>: <body>`, redirects, non-JSON, size, timeout |
