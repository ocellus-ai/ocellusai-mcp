package prometheus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type mock struct {
	mu   sync.Mutex
	last *http.Request
	form map[string]string
}

func newMock(t *testing.T) (*mock, *httptest.Server) {
	t.Helper()
	m := &mock{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.last = r
		m.form = map[string]string{}
		for k := range r.Form {
			m.form[k] = r.Form.Get(k)
		}
	}
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("query") {
		case "up":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			 {"metric":{"__name__":"up","job":"node","instance":"10.0.0.1:9100"},"value":[1757600000.5,"1"]},
			 {"metric":{"__name__":"up","job":"node","instance":"10.0.0.7:9100"},"value":[1757600000.5,"0"]}]}}`))
		case "scalar(1)":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1757600000,"1"]}}`))
		case "slow":
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case "warn":
			_, _ = w.Write([]byte(`{"status":"success","warnings":["careful"],"data":{"resultType":"vector","result":[]}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": parse error"}`))
		}
	})
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
		 {"metric":{"pod":"a"},"values":[[1757600000,"1"],[1757600060,"2"]]}]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return m, srv
}

func newWorker(t *testing.T, url string, headers map[string]string) *Worker {
	t.Helper()
	w, err := New(Config{URL: url, Timeout: 2 * time.Second, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	return w
}

func TestInstantVectorNormalized(t *testing.T) {
	m, srv := newMock(t)
	w := newWorker(t, srv.URL, map[string]string{"Authorization": "Bearer tok"})
	out, err := w.Execute(context.Background(), map[string]any{"type": "instant", "query": "up"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	want := `{"result":[{"metric":{"__name__":"up","instance":"10.0.0.1:9100","job":"node"},"value":[1757600000.5,"1"]},{"metric":{"__name__":"up","instance":"10.0.0.7:9100","job":"node"},"value":[1757600000.5,"0"]}],"resultType":"vector"}`
	if string(b) != want {
		t.Errorf("normalized =\n%s\nwant\n%s", b, want)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last.Header.Get("Authorization") != "Bearer tok" {
		t.Error("static header not sent")
	}
	if _, has := m.form["time"]; has {
		t.Error("time should be omitted when not set")
	}
}

func TestInstantWithTimeAndScalar(t *testing.T) {
	m, srv := newMock(t)
	w := newWorker(t, srv.URL, nil)
	out, err := w.Execute(context.Background(), map[string]any{"query": "scalar(1)", "time": "now-5m"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["resultType"] != "scalar" {
		t.Errorf("out = %#v", out)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.form["time"] != "1789127700" {
		t.Errorf("time param = %q", m.form["time"])
	}
}

func TestRangeMatrix(t *testing.T) {
	m, srv := newMock(t)
	w := newWorker(t, srv.URL, nil)
	out, err := w.Execute(context.Background(), map[string]any{
		"type": "range", "query": "x", "start": "now-30m", "end": "now", "step": "30s",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if res["resultType"] != "matrix" {
		t.Fatalf("out = %#v", out)
	}
	vals := res["result"].([]any)[0].(map[string]any)["values"].([]any)
	if len(vals) != 2 || vals[1].([]any)[1] != "2" {
		t.Errorf("values = %#v", vals)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.form["start"] != "1789126200" || m.form["end"] != "1789128000" || m.form["step"] != "30" {
		t.Errorf("range params = %v", m.form)
	}
}

func TestRangeDefaults(t *testing.T) {
	m, srv := newMock(t)
	w := newWorker(t, srv.URL, nil)
	if _, err := w.Execute(context.Background(), map[string]any{"type": "range", "query": "x"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.form["start"] != "1789124400" || m.form["end"] != "1789128000" || m.form["step"] != "60" {
		t.Errorf("default range params = %v", m.form)
	}
}

func TestAPIErrorMapped(t *testing.T) {
	_, srv := newMock(t)
	w := newWorker(t, srv.URL, nil)
	_, err := w.Execute(context.Background(), map[string]any{"query": "bogus{"})
	if err == nil || !strings.Contains(err.Error(), "bad_data") || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	_, srv := newMock(t)
	w, _ := New(Config{URL: srv.URL, Timeout: 50 * time.Millisecond})
	_, err := w.Execute(context.Background(), map[string]any{"query": "slow"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
}

func TestWarningsDoNotFail(t *testing.T) {
	_, srv := newMock(t)
	w := newWorker(t, srv.URL, nil)
	if _, err := w.Execute(context.Background(), map[string]any{"query": "warn"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	w := newWorker(t, "http://localhost:1", nil)
	ok := []map[string]any{
		{"query": "up"},
		{"type": "instant", "query": "up", "time": "now"},
		{"type": "range", "query": "up", "start": "now-1h", "end": "now", "step": "1m"},
	}
	for _, req := range ok {
		if err := w.Validate(req); err != nil {
			t.Errorf("%v: unexpected error %v", req, err)
		}
	}
	bad := map[string]map[string]any{
		"unknown key":      {"query": "up", "querry": "x"},
		"missing query":    {"type": "instant"},
		"bad type":         {"type": "stream", "query": "up"},
		"instant w/ step":  {"type": "instant", "query": "up", "step": "1m"},
		"range w/ time":    {"type": "range", "query": "up", "time": "now"},
		"non-string query": {"query": 5},
	}
	for name, req := range bad {
		if err := w.Validate(req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseTimeFormats(t *testing.T) {
	w := newWorker(t, "http://localhost:1", nil)
	now := w.now()
	cases := map[string]time.Time{
		"":                     {},
		"now":                  now,
		"now+1h":               now.Add(time.Hour),
		"2026-09-11T10:00:00Z": time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
		"1757600000":           time.Unix(1757600000, 0),
	}
	for in, want := range cases {
		got, err := w.parseTime(map[string]any{"t": in}, "t", time.Time{})
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := w.parseTime(map[string]any{"t": "yesterday"}, "t", time.Time{}); err == nil {
		t.Error("expected error for unparseable time")
	}
	if _, err := parseStep(map[string]any{"step": "-1s"}); err == nil {
		t.Error("expected error for negative step")
	}
	if d, err := parseStep(map[string]any{"step": "1d"}); err != nil || d != 24*time.Hour {
		t.Errorf("parseStep(1d) = %v, %v", d, err)
	}
}
