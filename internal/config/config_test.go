package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseEmptyGivesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Transport != TransportStdio || cfg.ToolsDir != "./tools" || cfg.Log.Format != "json" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.Workers) != 0 || len(cfg.Workers.Names()) != 0 {
		t.Fatal("workers should be empty by default")
	}
}

func TestParseFullWithEnv(t *testing.T) {
	t.Setenv("PROM_TOKEN", "s3cr3t")
	t.Setenv("PORT", "9090")
	t.Setenv("MAXOUT", "2048")
	cfg, err := Parse([]byte(`
server:
  transport: http
  listen: ":${PORT}"
tools_dir: /etc/mcp/tools
workers:
  prometheus:
    url: http://prom:9090
    headers:
      Authorization: "Bearer ${PROM_TOKEN}"
  shell:
    allowlist: [dig]
    max_output_bytes: ${MAXOUT}
    env: {LC_ALL: C}
log:
  level: debug
  format: text
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	prom := cfg.Workers["prometheus"]
	if prom == nil || prom.Type != WorkerPrometheus || prom.Prometheus == nil || prom.Shell != nil {
		t.Fatalf("prometheus instance = %+v", prom)
	}
	if got := prom.Prometheus.Headers["Authorization"]; got != "Bearer s3cr3t" {
		t.Errorf("header = %q", got)
	}
	if prom.Prometheus.Timeout != 15*time.Second {
		t.Errorf("prometheus timeout default = %s", prom.Prometheus.Timeout)
	}
	sh := cfg.Workers["shell"]
	if sh == nil || sh.Type != WorkerShell || sh.Shell == nil || sh.Prometheus != nil {
		t.Fatalf("shell instance = %+v", sh)
	}
	if sh.Shell.MaxOutputBytes != 2048 {
		t.Errorf("max_output_bytes = %d", sh.Shell.MaxOutputBytes)
	}
	if sh.Shell.Timeout != 30*time.Second {
		t.Errorf("shell timeout default = %s", sh.Shell.Timeout)
	}
	if sh.Shell.Env["LC_ALL"] != "C" || cfg.Log.Level != "debug" {
		t.Errorf("unexpected: %+v", cfg)
	}
	if got := strings.Join(cfg.Workers.Names(), ","); got != "prometheus,shell" {
		t.Errorf("names = %q", got)
	}
}

func TestParseNamedInstances(t *testing.T) {
	cfg, err := Parse([]byte(`
workers:
  prometheus:
    url: http://prom:9090
  prom_staging:
    type: prometheus
    url: http://staging:9090
    timeout: 5s
  dns:
    type: shell
    allowlist: [dig]
  disabled:
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Workers.Names(), ","); got != "dns,prom_staging,prometheus" {
		t.Errorf("names = %q (nil entries must be skipped)", got)
	}
	if inst, ok := cfg.Workers["disabled"]; !ok || inst != nil {
		t.Errorf("null entry must be present but nil, got %v %v", inst, ok)
	}
	st := cfg.Workers["prom_staging"]
	if st.Type != WorkerPrometheus || st.Prometheus.URL != "http://staging:9090" || st.Prometheus.Timeout != 5*time.Second {
		t.Errorf("prom_staging = %+v", st.Prometheus)
	}
	if cfg.Workers["prometheus"].Prometheus.URL != "http://prom:9090" {
		t.Errorf("shorthand instance broken: %+v", cfg.Workers["prometheus"])
	}
	kc := cfg.Workers["dns"]
	if kc.Type != WorkerShell || kc.Shell == nil || kc.Shell.Allowlist[0] != "dig" || kc.Shell.Timeout != 30*time.Second {
		t.Errorf("dns = %+v", kc)
	}
}

func TestParseMissingEnvIsError(t *testing.T) {
	_, err := Parse([]byte("workers:\n  prometheus:\n    url: ${NOT_SET_ANYWHERE_XYZ}\n"))
	if err == nil || !strings.Contains(err.Error(), "NOT_SET_ANYWHERE_XYZ") {
		t.Fatalf("expected missing env error, got %v", err)
	}
}

func TestParseIgnoresEnvInComments(t *testing.T) {
	cfg, err := Parse([]byte("# token: ${NOT_SET_IN_COMMENT}\ntools_dir: ./x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolsDir != "./x" {
		t.Errorf("tools_dir = %q", cfg.ToolsDir)
	}
}

func TestParseUnknownField(t *testing.T) {
	_, err := Parse([]byte("server:\n  transprt: http\n"))
	if err == nil || !strings.Contains(err.Error(), "transprt") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestWorkerErrors(t *testing.T) {
	cases := map[string]struct {
		yml  string
		want string
	}{
		"unknown type":              {"workers:\n  loki:\n    url: http://loki\n", `workers.loki.type: unsupported value "loki"`},
		"explicit unknown type":     {"workers:\n  x:\n    type: grpc\n", `workers.x.type: unsupported value "grpc"`},
		"rest no url":               {"workers:\n  am:\n    type: rest\n", "workers.am.url: required"},
		"rest unknown field":        {"workers:\n  am:\n    type: rest\n    url: http://am\n    allowlist: [x]\n", "workers.am: unknown field(s) allowlist (allowed: ca_file, headers, insecure_skip_verify, max_response_bytes, methods, timeout, type, url) (type rest)"},
		"rest negative limit":       {"workers:\n  am:\n    type: rest\n    url: http://am\n    max_response_bytes: -1\n", "workers.am: timeout and max_response_bytes must not be negative"},
		"rest empty method":         {"workers:\n  am:\n    type: rest\n    url: http://am\n    methods: [GET, \"\"]\n", "workers.am.methods: empty entry"},
		"unknown field":             {"workers:\n  prometheus:\n    urll: http://p\n", "workers.prometheus: unknown field(s) urll (allowed: headers, timeout, type, url)"},
		"field of the other type":   {"workers:\n  kc:\n    type: shell\n    url: http://p\n", "workers.kc: unknown field(s) url"},
		"type not a string":         {"workers:\n  kc:\n    type: [shell]\n", "workers.kc.type: must be a string"},
		"settings not a mapping":    {"workers:\n  prometheus: http://p\n", "workers.prometheus: must be a mapping"},
		"workers not a mapping":     {"workers: [prometheus]\n", "workers: must be a mapping"},
		"bad value type":            {"workers:\n  prometheus:\n    url: http://p\n    timeout: soon\n", "workers.prometheus:"},
		"prom no url":               {"workers:\n  prom_b:\n    type: prometheus\n", "workers.prom_b.url: required"},
		"empty allowlist":           {"workers:\n  shell:\n    allowlist: []\n", "workers.shell.allowlist: must list at least one binary"},
		"empty allowlist entry":     {"workers:\n  kc:\n    type: shell\n    allowlist: [dig, \"\"]\n", "workers.kc.allowlist: empty entry"},
		"negative timeout":          {"workers:\n  prometheus:\n    url: http://p\n    timeout: -1s\n", "workers.prometheus.timeout: must not be negative"},
		"bad instance name":         {"workers:\n  \"prom prod\":\n    type: prometheus\n    url: http://p\n", "instance name must match"},
		"prometheus needs settings": {"workers:\n  prometheus: {}\n", "workers.prometheus.url: required"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.yml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"bad transport":   "server:\n  transport: grpc\n",
		"bad log level":   "log:\n  level: verbose\n",
		"bad log format":  "log:\n  format: xml\n",
		"empty tools_dir": "tools_dir: \"\"\n",
		"http no listen":  "server:\n  transport: http\n  listen: \"\"\n",
	}
	for name, yml := range cases {
		if _, err := Parse([]byte(yml)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateProgrammaticConfig(t *testing.T) {
	cfg := Default()
	cfg.Workers = Workers{
		"prod": {Type: WorkerPrometheus, Prometheus: &Prometheus{URL: "http://p"}},
		"dns":  {Type: WorkerShell, Shell: &Shell{Allowlist: []string{"dig"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Workers["broken"] = &Worker{Type: WorkerPrometheus}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "workers.broken: prometheus settings missing") {
		t.Errorf("missing settings: %v", err)
	}
}

func TestParseRestInstance(t *testing.T) {
	t.Setenv("AM_TOKEN", "s3cret")
	cfg, err := Parse([]byte(`
workers:
  rest:
    url: http://alertmanager:9093/api/v2
  gitlab:
    type: rest
    url: https://gitlab.example.com/api/v4
    timeout: 5s
    headers: {PRIVATE-TOKEN: "${AM_TOKEN}"}
    max_response_bytes: 4096
    methods: [get, Post]
    ca_file: /etc/ssl/internal.pem
    insecure_skip_verify: true
`))
	if err != nil {
		t.Fatal(err)
	}
	def := cfg.Workers["rest"]
	if def.Type != WorkerRest || def.Rest == nil || def.Rest.URL != "http://alertmanager:9093/api/v2" {
		t.Fatalf("rest = %+v", def)
	}
	if def.Rest.Timeout != 15*time.Second || def.Rest.MaxResponseBytes != 1<<20 || def.Rest.Methods != nil {
		t.Errorf("defaults not applied: %+v", def.Rest)
	}
	gl := cfg.Workers["gitlab"].Rest
	if gl == nil || gl.Timeout != 5*time.Second || gl.MaxResponseBytes != 4096 || gl.Headers["PRIVATE-TOKEN"] != "s3cret" {
		t.Errorf("gitlab = %+v", gl)
	}
	if strings.Join(gl.Methods, ",") != "GET,POST" {
		t.Errorf("methods should be upper-cased, got %v", gl.Methods)
	}
	if gl.CAFile != "/etc/ssl/internal.pem" || !gl.InsecureSkipVerify {
		t.Errorf("tls settings = %q %v", gl.CAFile, gl.InsecureSkipVerify)
	}
}
