package rest

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen records the last request the test server received.
type seen struct {
	mu       sync.Mutex
	Method   string
	Path     string // escaped path, as sent on the wire
	RawQuery string
	Header   http.Header
	Body     string
}

func (s *seen) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Method, s.Path, s.RawQuery, s.Header, s.Body = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(body)
}

// server starts a test server whose handler records the request and then
// runs respond, and a worker pointing at ts.URL + basePath.
func server(t *testing.T, basePath string, cfg Config, respond http.HandlerFunc) (*Worker, *seen) {
	t.Helper()
	s := &seen{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	cfg.URL = ts.URL + basePath
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w, s
}

func jsonResponse(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func run(t *testing.T, w *Worker, req map[string]any) any {
	t.Helper()
	out, err := w.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute(%v): %v", req, err)
	}
	return out
}

func mustFail(t *testing.T, w *Worker, req map[string]any, want string) {
	t.Helper()
	_, err := w.Execute(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Execute(%v): want error containing %q, got %v", req, want, err)
	}
}

func TestNewErrors(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"empty url":     {Config{}, "url is required"},
		"bad scheme":    {Config{URL: "ftp://x"}, "scheme must be http or https"},
		"no host":       {Config{URL: "http://"}, "host is required"},
		"query in url":  {Config{URL: "http://x/api?x=1"}, "must not contain a query or fragment"},
		"fragment":      {Config{URL: "http://x/api#f"}, "must not contain a query or fragment"},
		"bad method":    {Config{URL: "http://x", Methods: []string{"GET", "HEAD"}}, `methods: unsupported value "HEAD"`},
		"ca missing":    {Config{URL: "http://x", CAFile: filepath.Join(t.TempDir(), "nope.pem")}, "ca_file:"},
		"ca not a cert": {Config{URL: "http://x", CAFile: writeFile(t, "junk.pem", "not a certificate")}, "no certificates found"},
	}
	for name, tc := range cases {
		if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
	if w, err := New(Config{URL: "https://x"}); err != nil || w.Type() != Type || w.timeout != defaultTimeout || w.maxBody != defaultMaxBytes || len(w.methods) != len(Methods) {
		t.Errorf("defaults: %+v, %v", w, err)
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	w, err := New(Config{URL: "http://x/api"})
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := New(Config{URL: "http://x/api", Methods: []string{"get"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		w    *Worker
		req  map[string]any
		want string // "" = valid
	}{
		"minimal":              {w, map[string]any{"path": "/x"}, ""},
		"unknown key":          {w, map[string]any{"path": "/x", "url": "http://evil"}, "unknown field(s) url (allowed: method, path, query, headers, body, timeout, parse, result, ok_status)"},
		"no path":              {w, map[string]any{"method": "GET"}, "request.path: required"},
		"method template":      {w, map[string]any{"method": "{{ .m }}", "path": "/x"}, "request.method: must be a literal, not a template"},
		"method unsupported":   {w, map[string]any{"method": "HEAD", "path": "/x"}, "request.method: must be one of GET, POST, PUT, PATCH, DELETE"},
		"method not allowed":   {readOnly, map[string]any{"method": "delete", "path": "/x"}, "request.method: DELETE is not allowed by this worker instance (methods: GET)"},
		"method lower-case ok": {w, map[string]any{"method": "post", "path": "/x"}, ""},
		"path with query":      {w, map[string]any{"path": "/x?y=1"}, "request.path: must not contain ? or #"},
		"path with fragment":   {w, map[string]any{"path": "/x#y"}, "request.path: must not contain ? or #"},
		"path absolute":        {w, map[string]any{"path": "http://evil/x"}, "request.path: must be relative to the instance url"},
		"path protocol-rel":    {w, map[string]any{"path": "//evil/x"}, "request.path: must be relative to the instance url"},
		"path template":        {w, map[string]any{"path": "/x/{{ pathEscape .id }}"}, ""},
		"query not mapping":    {w, map[string]any{"path": "/x", "query": []any{"a"}}, "request.query: must be a mapping"},
		"query nested":         {w, map[string]any{"path": "/x", "query": map[string]any{"a": map[string]any{}}}, "request.query.a: must be a scalar or a list of scalars"},
		"query list item":      {w, map[string]any{"path": "/x", "query": map[string]any{"a": []any{"ok", []any{}}}}, "request.query.a[1]: must be a scalar"},
		"query ok":             {w, map[string]any{"path": "/x", "query": map[string]any{"a": "{{ .a }}", "n": 1, "l": []any{"x", 2, true}, "nil": nil}}, ""},
		"headers not mapping":  {w, map[string]any{"path": "/x", "headers": "x"}, "request.headers: must be a mapping"},
		"headers nested":       {w, map[string]any{"path": "/x", "headers": map[string]any{"X": []any{}}}, "request.headers.X: must be a string"},
		"parse bad":            {w, map[string]any{"path": "/x", "parse": "lines"}, "request.parse: must be json or raw"},
		"result bad":           {w, map[string]any{"path": "/x", "result": "status"}, "request.result: must be body or full"},
		"ok_status not list":   {w, map[string]any{"path": "/x", "ok_status": 404}, "request.ok_status: must be a list"},
		"ok_status range":      {w, map[string]any{"path": "/x", "ok_status": []any{200, 999}}, "request.ok_status[1]: 999 is not an HTTP status code"},
		"ok_status string":     {w, map[string]any{"path": "/x", "ok_status": []any{"404"}}, "request.ok_status[0]: must be an integer status code"},
		"ok_status float":      {w, map[string]any{"path": "/x", "ok_status": []any{404.5}}, "request.ok_status[0]: must be an integer status code"},
		"timeout bad":          {w, map[string]any{"path": "/x", "timeout": "soon"}, "request.timeout:"},
		"timeout zero":         {w, map[string]any{"path": "/x", "timeout": "0s"}, "request.timeout: must be positive"},
		"body with GET":        {w, map[string]any{"path": "/x", "body": map[string]any{}}, "request.body: not allowed with method GET"},
		"body literal bad":     {w, map[string]any{"method": "POST", "path": "/x", "body": "{not json"}, "request.body: not valid JSON"},
		"body literal ok":      {w, map[string]any{"method": "POST", "path": "/x", "body": `{"a": 1}`}, ""},
		"body template":        {w, map[string]any{"method": "POST", "path": "/x", "body": `{"a": {{ .a }}}`}, ""},
		"body text ok":         {w, map[string]any{"method": "POST", "path": "/x", "body": "a=1", "headers": map[string]any{"content-type": "application/x-www-form-urlencoded"}}, ""},
		"body map non-json ct": {w, map[string]any{"method": "POST", "path": "/x", "body": map[string]any{"a": 1}, "headers": map[string]any{"Content-Type": "text/plain"}}, "request.body: a mapping or list is sent as JSON, but Content-Type is \"text/plain\""},
		"body map template ct": {w, map[string]any{"method": "POST", "path": "/x", "body": map[string]any{"a": 1}, "headers": map[string]any{"Content-Type": "{{ .ct }}"}}, ""},
		"body with DELETE":     {w, map[string]any{"method": "DELETE", "path": "/x", "body": []any{1}}, ""},
		"full request": {w, map[string]any{
			"method": "PATCH", "path": "/items/{{ .id }}", "query": map[string]any{"dry": true},
			"headers": map[string]any{"X-Request-Id": "{{ .rid }}"}, "body": map[string]any{"name": "{{ .name }}", "n": 2},
			"timeout": "5s", "parse": "json", "result": "full", "ok_status": []any{409},
		}, ""},
	}
	for name, tc := range cases {
		err := tc.w.Validate(tc.req)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestExecuteMethods(t *testing.T) {
	w, s := server(t, "/api/v2", Config{}, func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			rw.WriteHeader(http.StatusCreated)
		}
		_, _ = rw.Write([]byte(`{"method":"` + r.Method + `"}`))
	})
	// GET returns the body itself.
	out := run(t, w, map[string]any{"path": "/items"})
	if !reflect.DeepEqual(out, map[string]any{"method": "GET"}) || s.Method != "GET" || s.Path != "/api/v2/items" {
		t.Errorf("GET: out = %#v, seen = %+v", out, s)
	}
	// Every other method returns {status, headers, body} by default.
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		out := run(t, w, map[string]any{"method": m, "path": "/items/1", "body": map[string]any{"m": m}})
		full, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("%s: out = %#v", m, out)
		}
		wantStatus := 200
		if m == "POST" {
			wantStatus = 201
		}
		if full["status"] != wantStatus || !reflect.DeepEqual(full["body"], map[string]any{"method": m}) {
			t.Errorf("%s: out = %#v", m, out)
		}
		if hdrs := full["headers"].(map[string]any); hdrs["content-type"] != "application/json" {
			t.Errorf("%s: headers = %#v", m, hdrs)
		}
		if s.Method != m || s.Body != `{"m":"`+m+`"}` || s.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: seen = %+v", m, s)
		}
	}
	// result: body on an action, result: full on a read.
	if out := run(t, w, map[string]any{"method": "DELETE", "path": "/items/1", "result": "body"}); !reflect.DeepEqual(out, map[string]any{"method": "DELETE"}) {
		t.Errorf("DELETE result body = %#v", out)
	}
	if out := run(t, w, map[string]any{"path": "/items", "result": "full"}).(map[string]any); out["status"] != 200 {
		t.Errorf("GET result full = %#v", out)
	}
}

func TestExecuteURL(t *testing.T) {
	w, s := server(t, "/api/v2", Config{}, nil)
	run(t, w, map[string]any{
		"path":  "/silence/a%2Fb",
		"query": map[string]any{"filter": `alertname="X"`, "limit": 10, "flag": true, "empty": "", "multi": []any{"a", "", "b"}},
	})
	if s.Path != "/api/v2/silence/a%2Fb" {
		t.Errorf("escaped segment must be kept: path = %q", s.Path)
	}
	if s.RawQuery != "filter=alertname%3D%22X%22&flag=true&limit=10&multi=a&multi=b" {
		t.Errorf("query = %q", s.RawQuery)
	}
	if s.Header.Get("Accept") != "application/json" || s.Header.Get("User-Agent") != userAgent {
		t.Errorf("default headers = %v", s.Header)
	}
	// A trailing slash is kept, the base path itself is reachable.
	run(t, w, map[string]any{"path": "/"})
	if s.Path != "/api/v2/" {
		t.Errorf("path = %q", s.Path)
	}
	// Leaving the base path is refused before any request is made.
	s.Path = ""
	mustFail(t, w, map[string]any{"path": "/../../admin"}, `request.path: "/../../admin" leaves the instance url path /api/v2`)
	mustFail(t, w, map[string]any{"path": "../v1/x"}, "leaves the instance url path")
	mustFail(t, w, map[string]any{"path": "/x?y=1"}, "request.path: must not contain ? or #")
	mustFail(t, w, map[string]any{"path": "http://evil/x"}, "request.path: must be relative to the instance url")
	if s.Path != "" {
		t.Errorf("a refused path must not reach the server, got %q", s.Path)
	}
	// Base URL without a path and with a trailing slash.
	w2, s2 := server(t, "", Config{}, nil)
	run(t, w2, map[string]any{"path": "x"})
	if s2.Path != "/x" {
		t.Errorf("no base path: %q", s2.Path)
	}
	w3, s3 := server(t, "/api/", Config{}, nil)
	run(t, w3, map[string]any{"path": "x/y"})
	if s3.Path != "/api/x/y" {
		t.Errorf("trailing slash base: %q", s3.Path)
	}
}

func TestExecuteHeaders(t *testing.T) {
	w, s := server(t, "", Config{Headers: map[string]string{"Authorization": "Bearer secret", "X-Instance": "1"}}, nil)
	run(t, w, map[string]any{
		"method":  "POST",
		"path":    "/x",
		"headers": map[string]any{"authorization": "Bearer from-tool", "X-Tool": "yes", "Accept": "text/plain", "Content-Type": "text/plain"},
		"body":    "plain text", // not JSON, allowed because the tool set a text Content-Type
	})
	if s.Header.Get("Authorization") != "Bearer secret" {
		t.Errorf("instance headers must win: %v", s.Header)
	}
	if s.Header.Get("X-Instance") != "1" || s.Header.Get("X-Tool") != "yes" || s.Header.Get("Accept") != "text/plain" {
		t.Errorf("headers = %v", s.Header)
	}
	// The body above is not JSON, so it needs a non-JSON Content-Type.
	mustFail(t, w, map[string]any{"method": "POST", "path": "/x", "body": "plain text"}, "request.body: not valid JSON after rendering")
	run(t, w, map[string]any{"method": "POST", "path": "/x", "body": "a=1&b=2", "headers": map[string]any{"Content-Type": "application/x-www-form-urlencoded"}})
	if s.Body != "a=1&b=2" || s.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("form body: %+v", s)
	}
}

func TestExecuteBodyEncoding(t *testing.T) {
	w, s := server(t, "", Config{}, nil)
	run(t, w, map[string]any{"method": "PUT", "path": "/x", "body": map[string]any{
		"name": "a<b>&c", "n": 3, "f": 1.5, "ok": true, "list": []any{1, "two"}, "nested": map[string]any{"k": nil},
	}})
	var got map[string]any
	if err := json.Unmarshal([]byte(s.Body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", s.Body, err)
	}
	want := map[string]any{"name": "a<b>&c", "n": float64(3), "f": 1.5, "ok": true, "list": []any{float64(1), "two"}, "nested": map[string]any{"k": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %s", s.Body)
	}
	if escaped := string(rune(92)) + "u003c"; strings.Contains(s.Body, escaped) {
		t.Errorf("HTML escaping must be off: %s", s.Body)
	}
	// A rendered string body is sent verbatim.
	run(t, w, map[string]any{"method": "POST", "path": "/x", "body": "{\n  \"a\": 1\n}\n"})
	if s.Body != "{\n  \"a\": 1\n}\n" {
		t.Errorf("string body = %q", s.Body)
	}
	// An empty string body means no body.
	run(t, w, map[string]any{"method": "POST", "path": "/x", "body": "  "})
	if s.Body != "" || s.Header.Get("Content-Type") != "" {
		t.Errorf("empty body: %q %q", s.Body, s.Header.Get("Content-Type"))
	}
}

func TestExecuteStatus(t *testing.T) {
	w, _ := server(t, "/api", Config{}, func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/missing":
			jsonResponse(404, `{"error": "silence not found"}`)(rw, r)
		case "/api/moved":
			rw.Header().Set("Location", "http://elsewhere/x")
			rw.WriteHeader(http.StatusFound)
		case "/api/boom":
			rw.WriteHeader(500)
			_, _ = rw.Write([]byte(strings.Repeat("x", 2000)))
		case "/api/nocontent":
			rw.WriteHeader(http.StatusNoContent)
		default:
			jsonResponse(200, `[1, 2]`)(rw, r)
		}
	})
	mustFail(t, w, map[string]any{"path": "/missing"}, `GET /api/missing: 404 Not Found: {"error": "silence not found"}`)
	// ok_status turns the same response into data.
	if out := run(t, w, map[string]any{"path": "/missing", "ok_status": []any{404}}); !reflect.DeepEqual(out, map[string]any{"error": "silence not found"}) {
		t.Errorf("ok_status: %#v", out)
	}
	if out := run(t, w, map[string]any{"path": "/missing", "ok_status": []any{404}, "result": "full"}).(map[string]any); out["status"] != 404 {
		t.Errorf("ok_status full: %#v", out)
	}
	mustFail(t, w, map[string]any{"path": "/moved"}, "GET /api/moved: 302 Found (redirects are not followed; Location: http://elsewhere/x)")
	_, err := w.Execute(context.Background(), map[string]any{"path": "/boom"})
	if err == nil || !strings.HasPrefix(err.Error(), "GET /api/boom: 500 Internal Server Error: xxx") || !strings.HasSuffix(err.Error(), "…") || len(err.Error()) > 600 {
		t.Errorf("long body must be cut: %v", err)
	}
	// 204 with no body: null for GET, {status: 204, body: null} for an action.
	if out := run(t, w, map[string]any{"path": "/nocontent"}); out != nil {
		t.Errorf("204 GET = %#v", out)
	}
	out := run(t, w, map[string]any{"method": "DELETE", "path": "/nocontent"}).(map[string]any)
	if out["status"] != 204 || out["body"] != nil {
		t.Errorf("204 DELETE = %#v", out)
	}
	if out := run(t, w, map[string]any{"path": "/list"}); !reflect.DeepEqual(out, []any{float64(1), float64(2)}) {
		t.Errorf("array body = %#v", out)
	}
}

func TestExecuteParse(t *testing.T) {
	w, _ := server(t, "", Config{MaxResponseBytes: 64}, func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = rw.Write([]byte("<html>\n<body>oops</body></html>"))
		case "/big":
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"pad":"` + strings.Repeat("x", 100) + `"}`))
		case "/mislabelled":
			rw.Header().Set("Content-Type", "text/plain")
			_, _ = rw.Write([]byte(`{"n": 1.5}`))
		}
	})
	mustFail(t, w, map[string]any{"path": "/html"}, `GET /html: response is not valid JSON (content-type "text/html; charset=utf-8"): <html> <body>oops</body></html> (use parse: raw for text responses)`)
	if out := run(t, w, map[string]any{"path": "/html", "parse": "raw"}); out != "<html>\n<body>oops</body></html>" {
		t.Errorf("raw = %#v", out)
	}
	mustFail(t, w, map[string]any{"path": "/big"}, "GET /big: response exceeded 64 bytes")
	mustFail(t, w, map[string]any{"path": "/big", "parse": "raw"}, "response exceeded 64 bytes")
	if out := run(t, w, map[string]any{"path": "/mislabelled"}); !reflect.DeepEqual(out, map[string]any{"n": 1.5}) {
		t.Errorf("JSON is parsed regardless of Content-Type: %#v", out)
	}
}

func TestExecuteTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})
	w, _ := server(t, "", Config{Timeout: 50 * time.Millisecond}, func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		rw.WriteHeader(200)
	})
	t.Cleanup(func() { close(release) })
	mustFail(t, w, map[string]any{"path": "/slow"}, "GET /slow: request timed out after 50ms")
	mustFail(t, w, map[string]any{"path": "/slow", "timeout": "20ms"}, "request timed out after 20ms")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := w.Execute(ctx, map[string]any{"path": "/slow", "timeout": "10s"})
	if err == nil || !strings.Contains(err.Error(), "GET /slow: cancelled") || !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}

func TestExecuteTransportErrorHidesURL(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	w, err := New(Config{URL: url + "/api", Headers: map[string]string{"Authorization": "Bearer secret"}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Execute(context.Background(), map[string]any{"path": "/x", "query": map[string]any{"token": "q-secret"}})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "GET /api/x: ") || strings.Contains(msg, "http://") || strings.Contains(msg, "q-secret") || strings.Contains(msg, "secret") {
		t.Errorf("error must name method and path only: %q", msg)
	}
}

func TestExecuteMethodAllowlist(t *testing.T) {
	w, s := server(t, "", Config{Methods: []string{"GET", "POST"}}, nil)
	run(t, w, map[string]any{"method": "POST", "path": "/x"})
	mustFail(t, w, map[string]any{"method": "DELETE", "path": "/x"}, "request.method: DELETE is not allowed by this worker instance (methods: GET, POST)")
	if s.Method != "POST" {
		t.Errorf("refused method must not reach the server: %q", s.Method)
	}
}

func TestTLS(t *testing.T) {
	ts := httptest.NewUnstartedServer(jsonResponse(200, `{"tls": true}`))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // the strict client below fails the handshake on purpose
	ts.StartTLS()
	t.Cleanup(ts.Close)
	logger := slog.New(slog.DiscardHandler)

	strict, err := New(Config{URL: ts.URL, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Execute(context.Background(), map[string]any{"path": "/"}); err == nil || !strings.Contains(err.Error(), "GET /: ") {
		t.Errorf("unknown CA must fail verification: %v", err)
	}

	insecure, err := New(Config{URL: ts.URL, InsecureSkipVerify: true, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if out := run(t, insecure, map[string]any{"path": "/"}); !reflect.DeepEqual(out, map[string]any{"tls": true}) {
		t.Errorf("insecure = %#v", out)
	}

	pemFile := writeFile(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})))
	trusted, err := New(Config{URL: ts.URL, CAFile: pemFile, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if out := run(t, trusted, map[string]any{"path": "/"}); !reflect.DeepEqual(out, map[string]any{"tls": true}) {
		t.Errorf("ca_file = %#v", out)
	}
	if _, err := x509.ParseCertificate(ts.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
}

func TestResultIsJSONCompatible(t *testing.T) {
	w, _ := server(t, "", Config{}, func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Add("X-Multi", "a")
		rw.Header().Add("X-Multi", "b")
		_, _ = rw.Write([]byte(`{"n": 1, "s": "x", "l": [true, null]}`))
	})
	out := run(t, w, map[string]any{"method": "POST", "path": "/x"})
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("result must be JSON-compatible: %v", err)
	}
	full := out.(map[string]any)
	if full["headers"].(map[string]any)["x-multi"] != "a, b" {
		t.Errorf("multi-value headers: %#v", full["headers"])
	}
}
