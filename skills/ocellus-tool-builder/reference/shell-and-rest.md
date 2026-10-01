# Workers shell and rest

## Contents

- [shell](#shell)
- [rest](#rest)

## shell

Runs one command from the instance's allowlist and returns its output.

### Request keys

| Key | Meaning |
|---|---|
| `command` | a command name from `workers.<instance>.allowlist`; checked at startup and at call time |
| `args` | list of arguments; each element is one argument and its own template |
| `stdin` | text for stdin (a template), optional |
| `parse` | `json` (default), `lines`, `raw` |
| `timeout` | a **Go** duration (`20s`, `1m30s`); overrides the instance's `timeout` (30 s by default) |

```yaml
name: pods_status
description: |
  Pods of a namespace with phase, readiness and total restarts, via kubectl.
  Returns [{pod, phase, ready, restarts}].
worker: shell
params:
  namespace: {type: string, required: true, description: "Kubernetes namespace, e.g. prod", pattern: "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"}
request:
  command: kubectl
  args: [get, pods, -n, "{{ .namespace }}", -o, json]
  parse: json
  timeout: 20s
response:
  jq: |
    [.items[] | (.status.containerStatuses // []) as $cs | {
      pod: .metadata.name,
      phase: .status.phase,
      ready: (($cs | length) > 0 and ($cs | all(.ready))),
      restarts: ([$cs[] | .restartCount] | add // 0)
    }]
```

### How the command runs

- **No shell.** There is no `sh -c`, so `|`, `>`, `&&`, `*`, `~`, `$VAR` do not work. Filter in jq or
  with the command's own options. Don't put `sh` or `bash` in an allowlist to get around it.
- **Empty environment:** only `PATH` and the instance's `env`. `HOME`, `KUBECONFIG`,
  `KUBERNETES_SERVICE_*` are not passed; whatever the command needs goes into `env` of the instance
  (inside a pod, kubectl needs `KUBERNETES_SERVICE_HOST`/`KUBERNETES_SERVICE_PORT` there).
- **Each `args` element is exactly one argument.** `"-n {{ .namespace }}"` passes one string `-n prod`
  with a space inside. Write `[-n, "{{ .namespace }}"]`.
- An argument cannot be removed conditionally; design arguments so an empty value is harmless
  (kubectl treats `-l ""` as "no selector").
- Numbers and booleans in `args` become strings; lists and maps are not allowed.
- **Option injection.** A parameter value starting with `-` becomes an option (`--kubeconfig=…`,
  `-A`). Forbid a leading dash in `pattern`, or put `--` before positional arguments if the command
  supports it.
- `timeout` uses Go syntax: `1d` does **not** work, write `24h`.
- The command runs on the ocellus-ai host (or in its container: the default image has no kubectl, the
  `with-kubectl` target adds it).
- Prefer read-only commands. A mutating command (`delete`, `scale`, `apply`) runs every time an agent
  calls the tool.

### parse and output limits

| `parse` | jq receives | Notes |
|---|---|---|
| `json` | object, array or primitive | empty stdout → `null`; invalid JSON → a **string** plus a log warning (not an error) |
| `lines` | array of strings | no trailing `\n`, `\r` removed, empty lines kept |
| `raw` | one string | the whole stdout |

- stdout is capped at the instance's `max_output_bytes` (1 MiB by default): a JSON object gets
  `truncated: true`, `lines`/`raw` get a marker line, a truncated non-object JSON is an error. Reduce
  output in the command: `-l`, `--field-selector`, `-o jsonpath`.
- Exit code ≠ 0 → error `exited with code N: <stderr>`; with exit 0 stderr only goes to the log.
- With `parse: json` and a command that printed text, jq receives a string and `.items[]` fails at
  stage `jq`. Check the command's output format.

Parsing `df -P` with `parse: lines`:

```yaml
params:
  threshold: {type: integer, default: 80, minimum: 0, maximum: 100, description: "Minimum use, percent"}
request:
  command: df
  args: [-P]
  parse: lines
response:
  jq: |
    .[1:] | map(split(" +"; ""))
    | map(select(length >= 6))
    | map({fs: .[0], used_pct: (.[4] | rtrimstr("%") | tonumber), mount: .[5]})
    | map(select(.used_pct >= $params.threshold))
```

## rest

One HTTP request to a JSON API. The operator sets the base URL, tokens and limits in an instance
(`workers.<name>` with `type: rest`); the tool gives only a relative `path` and, if needed, method,
query, headers and body. There is no free URL in a tool: this protects against requests to arbitrary
hosts and keeps secrets out of tool files.

```yaml
# config.yaml (the operator's side)
workers:
  alertmanager:
    type: rest
    url: http://alertmanager.monitoring:9093/api/v2   # http(s) only, no ? or #
    timeout: 15s
    headers: {Authorization: "Bearer ${AM_TOKEN}"}     # sent with every request, override the tool's headers, never logged
    max_response_bytes: 1048576                        # larger is an error: JSON cannot be truncated
    methods: [GET]                                     # allowed verbs; default all five
    # ca_file: /etc/ssl/internal.pem
    # insecure_skip_verify: false
```

If the API has no instance yet, propose this snippet to the user (secrets as `${ENV}` variables) and
explain that the server needs a restart with it.

### Request keys

| Key | Meaning |
|---|---|
| `method` | `GET` (default), `POST`, `PUT`, `PATCH`, `DELETE`; a literal, not a template; must be in the instance's `methods` |
| `path` | required; a template; relative to the instance `url`: no `?`/`#`, no scheme or host, cannot climb above the base path with `..` |
| `query` | map `name: value`; a value is a string template, number, bool or list; the worker encodes it; a value (or list item) rendering to an empty string is dropped, which makes optional filters easy |
| `headers` | map of headers (templates); the instance's headers with the same name win |
| `body` | a YAML structure (sent as JSON) or a string template (sent as is); not allowed with `GET` |
| `timeout` | a Go duration; overrides the instance timeout |
| `parse` | `json` (default): the body must be valid JSON; `raw`: the body as one string |
| `result` | `body`: the response body; `full`: `{status, headers, body}`; default `body` for GET, `full` for the other methods |
| `ok_status` | extra status codes to treat as success **in addition to** 2xx, e.g. `[404]` |

### What jq receives

- Success is 2xx or a code from `ok_status`. Anything else is an error at stage `worker` with method,
  path, status and the start of the body: `DELETE /api/v2/silence/x: 404 Not Found: {"error":"not found"}`.
  Redirects are not followed: 3xx is an error too.
- `result: body` (default for GET): the parsed body as is; write jq from the API docs (`.[] | .id`).
  An empty body (204) is `null`.
- `result: full` (default for POST, PUT, PATCH, DELETE): `{"status": 200, "headers": {"content-type":
  "…"}, "body": …}`; header names are lower-case. An action must show the agent the status.
- The body is parsed as JSON regardless of `Content-Type`; for text APIs use `parse: raw`.
- A response larger than `max_response_bytes` is an error, not a truncation: filter on the API side
  (`per_page`, `fields`, `filter`).

### Path and query

- Pass every value inside `path` through `pathEscape`: `/silence/{{ pathEscape .id }}`. Then `/`, `?`
  and `..` in the value cannot change the path. Also constrain the parameter with `pattern`.
- Query parameters only through `query`; `?` in `path` is forbidden. No double encoding.

```yaml
request:
  path: /projects/{{ pathEscape .project }}/pipelines
  query:
    status: "{{ .status }}"        # an empty value is not sent
    per_page: 20
    ref: [main, release]           # ref=main&ref=release
    # optional repeated filters: an item that renders empty is dropped
    filter: ['{{ if .alertname }}alertname="{{ .alertname }}"{{ end }}', '{{ if .severity }}severity="{{ .severity }}"{{ end }}']
```

### Request body

```yaml
# 1. A YAML structure → JSON. A template leaf always becomes a string, literals keep their type.
body:
  matchers: [{name: alertname, value: "{{ .alertname }}", isRegex: false}]
  comment: "{{ .comment }}"
  replicas: 3                      # a number; "{{ .replicas }}" would be the string "3"

# 2. A string template → sent as is; with a JSON Content-Type it must be valid JSON after rendering.
body: |
  {"replicas": {{ .replicas }}, "name": {{ .name | toJson }}}
```

- In the string form put every value through `toJson`: it quotes and escapes strings and leaves
  numbers and booleans as they are.
- The default Content-Type is `application/json`. For another format set
  `headers: {Content-Type: application/x-www-form-urlencoded}` and a string body.
- A literal string body is checked at startup, a template one at call time.

### Actions

`POST`, `PUT`, `PATCH` and `DELETE` act on the user's systems every time an agent calls the tool.

- The `description` says plainly what changes: `This is an action: …`.
- Return the status: keep the default `result: full` and map it in jq: `{id: .body.id, status: .status}`.
- No retries, on purpose. Don't put a non-idempotent action in `calls` after a step that can fail.
- The operator can forbid actions per instance with `methods: [GET]`.
- Never test-call an action without the user's explicit permission for the exact arguments.

### Worker errors

| Message | Cause |
|---|---|
| `request.path: must not contain ? or #` | move query parameters into `query` |
| `request.path: "…" leaves the instance url path /api/v2` | `..` climbed above the base path |
| `request.method: DELETE is not allowed by this worker instance (methods: GET)` | read-only instance |
| `request.method: must be a literal` | method written as a template |
| `request.body: not allowed with method GET` | body on GET |
| `request.body: not valid JSON after rendering` | the string body is not JSON; check `toJson` and quotes |
| `GET /api/v2/x: 404 Not Found: …` | non-2xx answer; if expected, add the code to `ok_status` |
| `GET /api/v2/x: 302 Found (redirects are not followed; Location: …)` | redirect; put the final URL into the instance |
| `GET /api/v2/x: response is not valid JSON (content-type "text/html")` | not JSON: wrong path, login page, proxy; for text use `parse: raw` |
| `GET /api/v2/x: response exceeded 1048576 bytes` | too large; filter on the API side |
| `GET /api/v2/x: request timed out after 15s` | timeout; `timeout` in the request or the instance |
