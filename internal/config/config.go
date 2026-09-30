// Package config loads the service configuration (config.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Transport names accepted in server.transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Worker types accepted in workers.<name>.type.
const (
	WorkerPrometheus = "prometheus"
	WorkerShell      = "shell"
	WorkerRest       = "rest"
)

// WorkerTypes lists the supported worker types in display order.
var WorkerTypes = []string{WorkerPrometheus, WorkerShell, WorkerRest}

// Config is the top-level service configuration.
type Config struct {
	Server   Server  `yaml:"server"`
	ToolsDir string  `yaml:"tools_dir"`
	Workers  Workers `yaml:"workers"`
	Log      Log     `yaml:"log"`
}

// Server configures the MCP transport.
type Server struct {
	Transport string `yaml:"transport"`
	Listen    string `yaml:"listen"`
}

// Workers maps worker instance names to their settings. Tools reference an
// instance as `worker: <name>`; several instances may share one type:
//
//	workers:
//	  prometheus:   {url: http://prom:9090}                    # type defaults to the name
//	  prom_staging: {type: prometheus, url: http://staging:9090}
//	  kubectl:      {type: shell, allowlist: [kubectl]}
//	  alertmanager: {type: rest, url: http://alertmanager:9093/api/v2}
//
// A nil entry (`name:` with no value) is ignored, i.e. the instance is disabled.
type Workers map[string]*Worker

// Worker is one configured instance: its type and the settings of that type.
// Exactly one settings pointer is non-nil, the one matching Type.
type Worker struct {
	Type       string
	Prometheus *Prometheus
	Shell      *Shell
	Rest       *Rest
}

// Prometheus configures a prometheus instance.
type Prometheus struct {
	URL     string            `yaml:"url"`
	Timeout time.Duration     `yaml:"timeout"`
	Headers map[string]string `yaml:"headers"`
}

// Shell configures a shell instance.
type Shell struct {
	Allowlist      []string          `yaml:"allowlist"`
	Timeout        time.Duration     `yaml:"timeout"`
	MaxOutputBytes int               `yaml:"max_output_bytes"`
	Env            map[string]string `yaml:"env"`
}

// Rest configures a rest instance: an HTTP JSON API behind one base URL.
// Tools reference the instance and give a path relative to URL; the base
// URL, credentials and limits are never set in tool YAML.
type Rest struct {
	URL                string            `yaml:"url"`
	Timeout            time.Duration     `yaml:"timeout"`
	Headers            map[string]string `yaml:"headers"`
	MaxResponseBytes   int               `yaml:"max_response_bytes"`
	Methods            []string          `yaml:"methods"`
	CAFile             string            `yaml:"ca_file"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify"`
}

// Log configures slog output.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns the configuration used when a field is omitted.
func Default() Config {
	return Config{
		Server:   Server{Transport: TransportStdio, Listen: ":8080"},
		ToolsDir: "./tools",
		Log:      Log{Level: "info", Format: "json"},
	}
}

// Load reads, expands and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandString replaces every ${VAR} in s with the environment value and
// collects the names of unset variables.
func expandString(s string, missing *[]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			*missing = append(*missing, name)
			return ""
		}
		return v
	})
}

// expandNode walks a YAML document and expands ${VAR} in scalar values only,
// so comments and keys are left alone.
func expandNode(n *yaml.Node, missing *[]string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		expanded := expandString(n.Value, missing)
		if expanded != n.Value {
			n.Value = expanded
			if n.Style == 0 || n.Style == yaml.TaggedStyle {
				// Plain scalar: let the decoder resolve the type of the new value
				// (so ${PORT} can become an integer).
				n.Tag = ""
			}
		}
	}
	for _, c := range n.Content {
		expandNode(c, missing)
	}
}

// Parse decodes YAML into a Config, applying environment expansion, defaults
// and validation.
func Parse(raw []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg := Default()
	if doc.Kind != 0 {
		var missing []string
		expandNode(&doc, &missing)
		if len(missing) > 0 {
			slices.Sort(missing)
			missing = slices.Compact(missing)
			return nil, fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		if err := enc.Encode(&doc); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		_ = enc.Close()
		dec := yaml.NewDecoder(&buf)
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// UnmarshalYAML decodes the workers mapping: every key is an instance name,
// the optional `type` field selects the settings struct and defaults to the
// key itself, the remaining fields are decoded strictly into that struct.
func (w *Workers) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("workers: must be a mapping of instance name to settings")
	}
	out := Workers{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		if _, dup := out[name]; dup {
			return fmt.Errorf("workers.%s: duplicate instance", name)
		}
		inst, err := decodeWorker(name, n.Content[i+1])
		if err != nil {
			return err
		}
		out[name] = inst
	}
	*w = out
	return nil
}

// decodeWorker decodes one instance. A null node yields nil (disabled).
func decodeWorker(name string, n *yaml.Node) (*Worker, error) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil, nil //nolint:nilnil // nil means "instance disabled", see Workers
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("workers.%s: must be a mapping of settings", name)
	}
	typ := name
	settings := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value != "type" {
			settings.Content = append(settings.Content, k, v)
			continue
		}
		if v.Kind != yaml.ScalarNode || v.Tag == "!!null" {
			return nil, fmt.Errorf("workers.%s.type: must be a string", name)
		}
		typ = v.Value
	}
	inst := &Worker{Type: typ}
	var target any
	switch typ {
	case WorkerPrometheus:
		inst.Prometheus = &Prometheus{}
		target = inst.Prometheus
	case WorkerShell:
		inst.Shell = &Shell{}
		target = inst.Shell
	case WorkerRest:
		inst.Rest = &Rest{}
		target = inst.Rest
	default:
		return nil, fmt.Errorf("workers.%s.type: unsupported value %q (want %s)", name, typ, strings.Join(WorkerTypes, " or "))
	}
	if err := checkFields(settings, target); err != nil {
		return nil, fmt.Errorf("workers.%s: %w (type %s)", name, err, typ)
	}
	if err := settings.Decode(target); err != nil {
		return nil, fmt.Errorf("workers.%s: %w", name, err)
	}
	return inst, nil
}

// checkFields rejects mapping keys that have no yaml-tagged field in target,
// which yaml.Node.Decode would silently ignore.
func checkFields(n *yaml.Node, target any) error {
	t := reflect.TypeOf(target).Elem()
	allowed := make([]string, 0, t.NumField()+1)
	for i := 0; i < t.NumField(); i++ {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag == "" {
			tag = strings.ToLower(t.Field(i).Name)
		}
		allowed = append(allowed, tag)
	}
	var unknown []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !slices.Contains(allowed, n.Content[i].Value) {
			unknown = append(unknown, n.Content[i].Value)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	allowed = append(allowed, "type")
	sort.Strings(allowed)
	return fmt.Errorf("unknown field(s) %s (allowed: %s)", strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// Names returns the instance names in sorted order, disabled (nil) entries excluded.
func (w Workers) Names() []string {
	names := make([]string, 0, len(w))
	for n, inst := range w {
		if inst != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func (c *Config) applyDefaults() {
	for _, inst := range c.Workers {
		if inst == nil {
			continue
		}
		if p := inst.Prometheus; p != nil && p.Timeout == 0 {
			p.Timeout = 15 * time.Second
		}
		if s := inst.Shell; s != nil {
			if s.Timeout == 0 {
				s.Timeout = 30 * time.Second
			}
			if s.MaxOutputBytes == 0 {
				s.MaxOutputBytes = 1 << 20
			}
		}
		if r := inst.Rest; r != nil {
			if r.Timeout == 0 {
				r.Timeout = 15 * time.Second
			}
			if r.MaxResponseBytes == 0 {
				r.MaxResponseBytes = 1 << 20
			}
			for i, m := range r.Methods {
				r.Methods[i] = strings.ToUpper(strings.TrimSpace(m))
			}
		}
	}
}

// workerName constrains instance names so they read well in tool YAML and logs.
var workerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Validate checks the configuration for errors that would prevent startup.
func (c *Config) Validate() error {
	switch c.Server.Transport {
	case TransportStdio, TransportHTTP:
	default:
		return fmt.Errorf("server.transport: unsupported value %q (want stdio or http)", c.Server.Transport)
	}
	if c.Server.Transport == TransportHTTP && c.Server.Listen == "" {
		return errors.New("server.listen: required for http transport")
	}
	if c.ToolsDir == "" {
		return errors.New("tools_dir: required")
	}
	for _, name := range c.Workers.Names() {
		if err := c.Workers[name].validate("workers." + name); err != nil {
			return err
		}
		if !workerName.MatchString(name) {
			return fmt.Errorf("workers.%s: instance name must match %s", name, workerName)
		}
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level: unsupported value %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log.format: unsupported value %q (want json or text)", c.Log.Format)
	}
	return nil
}

func (w *Worker) validate(prefix string) error {
	switch w.Type {
	case WorkerPrometheus:
		p := w.Prometheus
		if p == nil {
			return fmt.Errorf("%s: prometheus settings missing", prefix)
		}
		if p.URL == "" {
			return fmt.Errorf("%s.url: required", prefix)
		}
		if p.Timeout < 0 {
			return fmt.Errorf("%s.timeout: must not be negative", prefix)
		}
	case WorkerShell:
		s := w.Shell
		if s == nil {
			return fmt.Errorf("%s: shell settings missing", prefix)
		}
		if len(s.Allowlist) == 0 {
			return fmt.Errorf("%s.allowlist: must list at least one binary", prefix)
		}
		for _, b := range s.Allowlist {
			if strings.TrimSpace(b) == "" {
				return fmt.Errorf("%s.allowlist: empty entry", prefix)
			}
		}
		if s.Timeout < 0 || s.MaxOutputBytes < 0 {
			return fmt.Errorf("%s: timeout and max_output_bytes must not be negative", prefix)
		}
	case WorkerRest:
		r := w.Rest
		if r == nil {
			return fmt.Errorf("%s: rest settings missing", prefix)
		}
		if strings.TrimSpace(r.URL) == "" {
			return fmt.Errorf("%s.url: required", prefix)
		}
		if r.Timeout < 0 || r.MaxResponseBytes < 0 {
			return fmt.Errorf("%s: timeout and max_response_bytes must not be negative", prefix)
		}
		for _, m := range r.Methods {
			if m == "" {
				return fmt.Errorf("%s.methods: empty entry", prefix)
			}
		}
	default:
		return fmt.Errorf("%s.type: unsupported value %q (want %s)", prefix, w.Type, strings.Join(WorkerTypes, " or "))
	}
	return nil
}
