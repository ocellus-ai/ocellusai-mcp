package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/ocellus-ai/ocellusai-mcp/internal/config"
)

func TestBuildWorkersInstances(t *testing.T) {
	cfg, err := config.Parse([]byte(`
workers:
  prometheus: {url: http://prom:9090}
  prom_staging: {type: prometheus, url: http://staging:9090}
  dns: {type: shell, allowlist: [dig]}
  am: {type: rest, url: http://alertmanager:9093/api/v2, methods: [GET]}
  off:
`))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := buildWorkers(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reg.Names(), ","); got != "am,dns,prom_staging,prometheus" {
		t.Errorf("registry = %q", got)
	}
	for name, typ := range map[string]string{"prometheus": "prometheus", "prom_staging": "prometheus", "dns": "shell", "am": "rest"} {
		if reg[name].Type() != typ {
			t.Errorf("%s: type = %q, want %q", name, reg[name].Type(), typ)
		}
	}
	if reg["prometheus"] == reg["prom_staging"] {
		t.Error("instances of one type must be distinct workers")
	}
}

func TestBuildWorkersNoneConfigured(t *testing.T) {
	cfg, err := config.Parse([]byte("workers:\n  off:\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildWorkers(cfg, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "no workers configured") {
		t.Errorf("expected no-workers error, got %v", err)
	}
}

func TestBuildProcessors(t *testing.T) {
	reg, err := buildProcessors()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reg.Names(), ","); got != "anomaly,anomaly_ensemble,anomaly_mv,cluster_events,join,jq,outliers,ssa,ssa_mv" {
		t.Errorf("processors = %q", got)
	}
}

func TestBuildWorkersRestErrors(t *testing.T) {
	cfg, err := config.Parse([]byte("workers:\n  am: {type: rest, url: \"ftp://am\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildWorkers(cfg, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "workers.am: rest: url: scheme must be http or https") {
		t.Errorf("expected url error, got %v", err)
	}
}
