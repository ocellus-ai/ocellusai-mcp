// Package prometheus implements the "prometheus" worker on top of the native
// client_golang API. Results are normalised to the Prometheus HTTP API JSON
// shape so jq expressions written against the HTTP API work unchanged.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// Type is the worker type referenced from config (`type: ...`); a config
// entry whose name equals the type may omit it.
const Type = "prometheus"

// Config configures the worker.
type Config struct {
	URL     string
	Timeout time.Duration
	Headers map[string]string
	Logger  *slog.Logger
}

// Worker queries a Prometheus-compatible API.
type Worker struct {
	api     v1.API
	timeout time.Duration
	logger  *slog.Logger
	now     func() time.Time
}

// New creates a worker. Headers are sent with every request and never logged.
func New(cfg Config) (*Worker, error) {
	if cfg.URL == "" {
		return nil, errors.New("prometheus: url is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	rt := api.DefaultRoundTripper
	if len(cfg.Headers) > 0 {
		rt = headerRoundTripper{base: rt, headers: cfg.Headers}
	}
	client, err := api.NewClient(api.Config{Address: cfg.URL, RoundTripper: rt})
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	return &Worker{api: v1.NewAPI(client), timeout: cfg.Timeout, logger: cfg.Logger, now: time.Now}, nil
}

// Type implements worker.Worker.
func (w *Worker) Type() string { return Type }

// Validate implements worker.Worker for the raw request section.
func (w *Worker) Validate(req map[string]any) error {
	if err := worker.CheckKeys(req, "type", "query", "time", "start", "end", "step"); err != nil {
		return err
	}
	typ, err := queryType(req)
	if err != nil {
		return err
	}
	if _, err := worker.RequiredString(req, "query"); err != nil {
		return err
	}
	for _, k := range []string{"time", "start", "end", "step"} {
		if _, err := worker.String(req, k); err != nil {
			return err
		}
	}
	switch typ {
	case "instant":
		for _, k := range []string{"start", "end", "step"} {
			if _, ok := req[k]; ok {
				return fmt.Errorf("request.%s: only valid for type: range", k)
			}
		}
	case "range":
		if _, ok := req["time"]; ok {
			return errors.New("request.time: only valid for type: instant")
		}
	}
	return nil
}

func queryType(req map[string]any) (string, error) {
	typ, err := worker.String(req, "type")
	if err != nil {
		return "", err
	}
	switch typ {
	case "":
		return "instant", nil
	case "instant", "range":
		return typ, nil
	default:
		return "", fmt.Errorf("request.type: must be instant or range, got %q", typ)
	}
}

// Execute implements worker.Worker.
func (w *Worker) Execute(ctx context.Context, req map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	typ, err := queryType(req)
	if err != nil {
		return nil, err
	}
	query, err := worker.RequiredString(req, "query")
	if err != nil {
		return nil, err
	}
	var (
		val   model.Value
		warns v1.Warnings
	)
	switch typ {
	case "instant":
		ts, err := w.parseTime(req, "time", time.Time{})
		if err != nil {
			return nil, err
		}
		val, warns, err = w.api.Query(ctx, query, ts)
		if err != nil {
			return nil, mapError(err)
		}
	case "range":
		now := w.now()
		end, err := w.parseTime(req, "end", now)
		if err != nil {
			return nil, err
		}
		start, err := w.parseTime(req, "start", now.Add(-time.Hour))
		if err != nil {
			return nil, err
		}
		step, err := parseStep(req)
		if err != nil {
			return nil, err
		}
		if !end.After(start) {
			return nil, fmt.Errorf("request: end (%s) must be after start (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
		}
		val, warns, err = w.api.QueryRange(ctx, query, v1.Range{Start: start, End: end, Step: step})
		if err != nil {
			return nil, mapError(err)
		}
	}
	for _, warn := range warns {
		w.logger.Warn("prometheus warning", "warning", warn)
	}
	return Normalize(val), nil
}

// parseTime accepts "", "now", "now-5m", "now+1h", RFC3339 or unix seconds.
func (w *Worker) parseTime(req map[string]any, key string, def time.Time) (time.Time, error) {
	s, err := worker.String(req, key)
	if err != nil {
		return time.Time{}, err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	if s == "now" {
		return w.now(), nil
	}
	if strings.HasPrefix(s, "now-") || strings.HasPrefix(s, "now+") {
		d, err := model.ParseDuration(s[4:])
		if err != nil {
			return time.Time{}, fmt.Errorf("request.%s: %w", key, err)
		}
		if s[3] == '-' {
			return w.now().Add(-time.Duration(d)), nil
		}
		return w.now().Add(time.Duration(d)), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec, frac := int64(f), f-float64(int64(f))
		return time.Unix(sec, int64(frac*1e9)), nil
	}
	return time.Time{}, fmt.Errorf("request.%s: %q is not RFC3339, unix seconds or now[-+duration]", key, s)
}

func parseStep(req map[string]any) (time.Duration, error) {
	s, err := worker.String(req, "step")
	if err != nil {
		return 0, err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Minute, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f <= 0 {
			return 0, errors.New("request.step: must be positive")
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := model.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("request.step: %w", err)
	}
	if d <= 0 {
		return 0, errors.New("request.step: must be positive")
	}
	return time.Duration(d), nil
}

func mapError(err error) error {
	var apiErr *v1.Error
	if errors.As(err, &apiErr) {
		msg := fmt.Sprintf("prometheus %s error: %s", apiErr.Type, apiErr.Msg)
		if apiErr.Detail != "" && apiErr.Detail != apiErr.Msg {
			msg += ": " + strings.TrimSpace(apiErr.Detail)
		}
		return errors.New(msg)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("prometheus: request timed out: %w", err)
	}
	return fmt.Errorf("prometheus: %w", err)
}

// Normalize converts a model.Value into the Prometheus HTTP API "data" object:
// {"resultType": "...", "result": ...}.
func Normalize(val model.Value) map[string]any {
	switch v := val.(type) {
	case model.Vector:
		result := make([]any, 0, len(v))
		for _, s := range v {
			item := map[string]any{"metric": metric(s.Metric)}
			if s.Histogram != nil {
				item["histogram"] = []any{ts(s.Timestamp), jsonValue(s.Histogram)}
			} else {
				item["value"] = []any{ts(s.Timestamp), s.Value.String()}
			}
			result = append(result, item)
		}
		return map[string]any{"resultType": "vector", "result": result}
	case model.Matrix:
		result := make([]any, 0, len(v))
		for _, ss := range v {
			item := map[string]any{"metric": metric(ss.Metric)}
			values := make([]any, 0, len(ss.Values))
			for _, p := range ss.Values {
				values = append(values, []any{ts(p.Timestamp), p.Value.String()})
			}
			item["values"] = values
			if len(ss.Histograms) > 0 {
				hs := make([]any, 0, len(ss.Histograms))
				for _, h := range ss.Histograms {
					hs = append(hs, []any{ts(h.Timestamp), jsonValue(h.Histogram)})
				}
				item["histograms"] = hs
			}
			result = append(result, item)
		}
		return map[string]any{"resultType": "matrix", "result": result}
	case *model.Scalar:
		return map[string]any{"resultType": "scalar", "result": []any{ts(v.Timestamp), v.Value.String()}}
	case *model.String:
		return map[string]any{"resultType": "string", "result": []any{ts(v.Timestamp), v.Value}}
	case nil:
		return map[string]any{"resultType": "", "result": nil}
	default:
		return map[string]any{"resultType": val.Type().String(), "result": jsonValue(val)}
	}
}

func metric(m model.Metric) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[string(k)] = string(v)
	}
	return out
}

// ts renders a model.Time (ms) as float seconds, as the HTTP API does.
func ts(t model.Time) float64 {
	return float64(t) / 1000
}

func jsonValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return string(b)
	}
	return out
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	for k, v := range h.headers {
		r2.Header.Set(k, v)
	}
	return h.base.RoundTrip(r2)
}
