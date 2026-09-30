// Package rest implements the "rest" worker: one HTTP request to a JSON API
// behind a configured base URL. A tool names the instance and a path relative
// to that URL; the host, credentials and limits live in the instance, so tool
// YAML can neither reach arbitrary hosts nor carry secrets. Every response is
// turned into a JSON-compatible value or into an error, never anything else.
package rest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ocellus-ai/ocellusai-mcp/internal/worker"
)

// Type is the worker type referenced from config (`type: ...`); a config
// entry whose name equals the type may omit it.
const Type = "rest"

// Methods lists the HTTP methods a request may use, in display order.
var Methods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// Parse modes supported in request.parse.
const (
	ParseJSON = "json"
	ParseRaw  = "raw"
)

// Result shapes supported in request.result. Without an explicit value GET
// returns the response body and every other method returns the full shape
// {status, headers, body}, because an action needs its status code.
const (
	ResultBody = "body"
	ResultFull = "full"
)

const (
	defaultTimeout  = 15 * time.Second
	defaultMaxBytes = 1 << 20
	snippetLimit    = 512
	userAgent       = "ocellusai-mcp"
	jsonContentType = "application/json"
)

var requestKeys = []string{"method", "path", "query", "headers", "body", "timeout", "parse", "result", "ok_status"}

// Config configures the worker.
type Config struct {
	URL                string            // base URL; request.path is joined to it
	Timeout            time.Duration     // per request, default 15s
	Headers            map[string]string // sent with every request, win over request.headers, never logged
	MaxResponseBytes   int               // default 1 MiB; a larger response is an error
	Methods            []string          // allowed methods; empty = all of Methods
	CAFile             string            // PEM bundle used to verify the server, optional
	InsecureSkipVerify bool              // skip TLS verification, optional
	Logger             *slog.Logger
}

// Worker sends HTTP requests to one base URL.
type Worker struct {
	base    *url.URL
	prefix  string // base path with a trailing slash; every request path must stay under it
	client  *http.Client
	timeout time.Duration
	headers map[string]string
	maxBody int
	methods map[string]struct{}
	logger  *slog.Logger
}

// New creates a worker. Redirects are never followed: a 3xx response is an
// error, so credentials cannot be sent to a host the operator did not configure.
func New(cfg Config) (*Worker, error) {
	base, err := ParseBaseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	methods := make(map[string]struct{}, len(Methods))
	if len(cfg.Methods) == 0 {
		for _, m := range Methods {
			methods[m] = struct{}{}
		}
	}
	for _, m := range cfg.Methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if !slices.Contains(Methods, m) {
			return nil, fmt.Errorf("rest: methods: unsupported value %q (want %s)", m, strings.Join(Methods, ", "))
		}
		methods[m] = struct{}{}
	}
	transport, err := newTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	prefix := base.Path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Worker{
		base:    base,
		prefix:  prefix,
		client:  client,
		timeout: cfg.Timeout,
		headers: headers,
		maxBody: cfg.MaxResponseBytes,
		methods: methods,
		logger:  cfg.Logger,
	}, nil
}

// ParseBaseURL checks an instance url: an absolute http(s) URL with a host and
// without query or fragment. An empty path becomes "/".
func ParseBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url: scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("url: host is required")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("url: must not contain a query or fragment (tools add query parameters via request.query)")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

func newTransport(cfg Config) (http.RoundTripper, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile == "" && !cfg.InsecureSkipVerify {
		return t, nil
	}
	// InsecureSkipVerify is an explicit operator opt-in for internal endpoints.
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file: no certificates found in %s", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	t.TLSClientConfig = tc
	return t, nil
}

// Type implements worker.Worker.
func (w *Worker) Type() string { return Type }

// Validate implements worker.Worker for the raw request section. Literal
// values are checked here; values that are templates are checked after
// rendering, in Execute.
func (w *Worker) Validate(req map[string]any) error {
	if err := worker.CheckKeys(req, requestKeys...); err != nil {
		return err
	}
	method, err := w.method(req)
	if err != nil {
		return err
	}
	p, err := worker.RequiredString(req, "path")
	if err != nil {
		return err
	}
	if !worker.IsTemplate(p) {
		if err := checkPath(p); err != nil {
			return err
		}
	}
	if _, err := queryValues(req); err != nil {
		return err
	}
	hdrs, err := headerMap(req)
	if err != nil {
		return err
	}
	if _, err := parseMode(req); err != nil {
		return err
	}
	if _, err := resultMode(req); err != nil {
		return err
	}
	if _, err := okStatus(req); err != nil {
		return err
	}
	if _, err := w.requestTimeout(req); err != nil {
		return err
	}
	_, _, err = w.encodeBody(req, method, hdrs, true)
	return err
}

// Execute implements worker.Worker.
func (w *Worker) Execute(ctx context.Context, req map[string]any) (any, error) {
	method, err := w.method(req)
	if err != nil {
		return nil, err
	}
	p, err := worker.RequiredString(req, "path")
	if err != nil {
		return nil, err
	}
	u, err := w.buildURL(p, req)
	if err != nil {
		return nil, err
	}
	hdrs, err := headerMap(req)
	if err != nil {
		return nil, err
	}
	mode, err := parseMode(req)
	if err != nil {
		return nil, err
	}
	shape, err := resultMode(req)
	if err != nil {
		return nil, err
	}
	accepted, err := okStatus(req)
	if err != nil {
		return nil, err
	}
	timeout, err := w.requestTimeout(req)
	if err != nil {
		return nil, err
	}
	body, contentType, err := w.encodeBody(req, method, hdrs, false)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	loc := method + " " + u.Path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", loc, err)
	}
	// Header precedence: worker defaults < request.headers < instance headers.
	// The instance wins so a tool cannot replace the operator's credentials.
	httpReq.Header.Set("Accept", jsonContentType)
	httpReq.Header.Set("User-Agent", userAgent)
	if body != nil && contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdrs {
		httpReq.Header.Set(k, v)
	}
	for k, v := range w.headers {
		httpReq.Header.Set(k, v)
	}

	w.logger.Debug("http request", "method", method, "path", u.Path)
	start := time.Now()
	resp, err := w.client.Do(httpReq)
	if err != nil {
		return nil, requestError(loc, err, timeout)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(w.maxBody)+1))
	if err != nil {
		return nil, requestError(loc, err, timeout)
	}
	w.logger.Debug("http response", "method", method, "path", u.Path, "status", resp.StatusCode, "bytes", len(data), "duration_ms", time.Since(start).Milliseconds())
	if len(data) > w.maxBody {
		return nil, fmt.Errorf("%s: response exceeded %d bytes", loc, w.maxBody)
	}
	if !statusOK(resp.StatusCode, accepted) {
		msg := loc + ": " + resp.Status
		if s := snippet(data); s != "" {
			msg += ": " + s
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			msg += " (redirects are not followed"
			if l := resp.Header.Get("Location"); l != "" {
				msg += "; Location: " + l
			}
			msg += ")"
		}
		return nil, errors.New(msg)
	}
	value, err := parseBody(mode, data, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", loc, err)
	}
	if shape == ResultFull || (shape == "" && method != http.MethodGet) {
		return map[string]any{"status": resp.StatusCode, "headers": headerValues(resp.Header), "body": value}, nil
	}
	return value, nil
}

// method reads request.method: a literal, upper-cased, one of Methods and
// allowed by the instance. Default GET.
func (w *Worker) method(req map[string]any) (string, error) {
	s, err := worker.String(req, "method")
	if err != nil {
		return "", err
	}
	if s == "" {
		s = http.MethodGet
	}
	if worker.IsTemplate(s) {
		return "", errors.New("request.method: must be a literal, not a template")
	}
	m := strings.ToUpper(strings.TrimSpace(s))
	if !slices.Contains(Methods, m) {
		return "", fmt.Errorf("request.method: must be one of %s, got %q", strings.Join(Methods, ", "), s)
	}
	if _, ok := w.methods[m]; !ok {
		return "", fmt.Errorf("request.method: %s is not allowed by this worker instance (methods: %s)", m, strings.Join(w.allowedMethods(), ", "))
	}
	return m, nil
}

func (w *Worker) allowedMethods() []string {
	out := make([]string, 0, len(w.methods))
	for _, m := range Methods {
		if _, ok := w.methods[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// checkPath rejects paths that would leave the instance url or carry a query.
func checkPath(p string) error {
	switch {
	case strings.ContainsAny(p, "?#"):
		return errors.New("request.path: must not contain ? or # (put parameters under request.query)")
	case strings.Contains(p, "://") || strings.HasPrefix(p, "//"):
		return errors.New("request.path: must be relative to the instance url (no scheme or host)")
	}
	return nil
}

// buildURL joins the rendered path to the base URL, refuses paths that
// escape the base path (via ..) and encodes request.query.
func (w *Worker) buildURL(p string, req map[string]any) (*url.URL, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	u := w.base.JoinPath(p)
	if !strings.HasPrefix(u.Path, w.prefix) && u.Path+"/" != w.prefix {
		return nil, fmt.Errorf("request.path: %q leaves the instance url path %s", p, w.base.Path)
	}
	q, err := queryValues(req)
	if err != nil {
		return nil, err
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u, nil
}

// queryValues reads request.query: a mapping of scalar or list-of-scalar
// values. Values that render to an empty string are omitted, so optional
// parameters can be written as `filter: "{{ .filter }}"`.
func queryValues(req map[string]any) (url.Values, error) {
	v, ok := req["query"]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("request.query: must be a mapping, got %T", v)
	}
	out := url.Values{}
	for k, val := range m {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("request.query: empty parameter name")
		}
		switch x := val.(type) {
		case nil:
		case []any:
			for i, item := range x {
				s, err := scalar(item)
				if err != nil {
					return nil, fmt.Errorf("request.query.%s[%d]: must be a scalar, got %T", k, i, item)
				}
				if s != "" {
					out.Add(k, s)
				}
			}
		default:
			s, err := scalar(x)
			if err != nil {
				return nil, fmt.Errorf("request.query.%s: must be a scalar or a list of scalars, got %T", k, val)
			}
			if s != "" {
				out.Add(k, s)
			}
		}
	}
	return out, nil
}

// headerMap reads request.headers: a mapping of scalar values.
func headerMap(req map[string]any) (map[string]string, error) {
	v, ok := req["headers"]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("request.headers: must be a mapping, got %T", v)
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if strings.TrimSpace(k) == "" {
			return nil, errors.New("request.headers: empty header name")
		}
		s, err := scalar(val)
		if err != nil {
			return nil, fmt.Errorf("request.headers.%s: must be a string, got %T", k, val)
		}
		out[k] = s
	}
	return out, nil
}

func scalar(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	default:
		return "", fmt.Errorf("not a scalar: %T", v)
	}
}

func parseMode(req map[string]any) (string, error) {
	mode, err := worker.String(req, "parse")
	if err != nil {
		return "", err
	}
	switch mode {
	case "":
		return ParseJSON, nil
	case ParseJSON, ParseRaw:
		return mode, nil
	default:
		return "", fmt.Errorf("request.parse: must be json or raw, got %q", mode)
	}
}

// resultMode reads request.result; "" means "by method" (see ResultBody).
func resultMode(req map[string]any) (string, error) {
	shape, err := worker.String(req, "result")
	if err != nil {
		return "", err
	}
	switch shape {
	case "", ResultBody, ResultFull:
		return shape, nil
	default:
		return "", fmt.Errorf("request.result: must be body or full, got %q", shape)
	}
}

// okStatus reads request.ok_status: status codes accepted in addition to 2xx.
func okStatus(req map[string]any) ([]int, error) {
	v, ok := req["ok_status"]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("request.ok_status: must be a list of status codes, got %T", v)
	}
	out := make([]int, 0, len(list))
	for i, item := range list {
		var code int
		switch x := item.(type) {
		case int:
			code = x
		case int64:
			code = int(x)
		case float64:
			code = int(x)
			if float64(code) != x {
				return nil, fmt.Errorf("request.ok_status[%d]: must be an integer status code, got %v", i, x)
			}
		default:
			return nil, fmt.Errorf("request.ok_status[%d]: must be an integer status code, got %T", i, item)
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("request.ok_status[%d]: %d is not an HTTP status code", i, code)
		}
		out = append(out, code)
	}
	return out, nil
}

func statusOK(code int, accepted []int) bool {
	return (code >= 200 && code < 300) || slices.Contains(accepted, code)
}

func (w *Worker) requestTimeout(req map[string]any) (time.Duration, error) {
	s, err := worker.String(req, "timeout")
	if err != nil {
		return 0, err
	}
	if s == "" {
		return w.timeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("request.timeout: %w", err)
	}
	if d <= 0 {
		return 0, errors.New("request.timeout: must be positive")
	}
	return d, nil
}

// contentType resolves the Content-Type a body is sent with: instance headers
// win over request.headers, the default is JSON. known is false when the
// request header is a template that has not been rendered yet.
func (w *Worker) contentType(hdrs map[string]string) (ct string, known bool) {
	if v, ok := lookupHeader(w.headers, "Content-Type"); ok {
		return v, true
	}
	if v, ok := lookupHeader(hdrs, "Content-Type"); ok {
		if worker.IsTemplate(v) {
			return "", false
		}
		return v, true
	}
	return jsonContentType, true
}

func lookupHeader(m map[string]string, name string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func isJSON(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "json")
}

// encodeBody turns request.body into bytes. A string is sent as is and, when
// the Content-Type is JSON, must be valid JSON; anything else (a YAML
// mapping or list) is serialised as JSON. At load time (load = true) a
// template string is not checked and a template Content-Type disables the
// JSON checks; both happen after rendering.
func (w *Worker) encodeBody(req map[string]any, method string, hdrs map[string]string, load bool) (body []byte, contentType string, err error) {
	v, ok := req["body"]
	if !ok || v == nil {
		return nil, "", nil
	}
	if method == http.MethodGet {
		return nil, "", errors.New("request.body: not allowed with method GET")
	}
	ct, known := w.contentType(hdrs)
	switch b := v.(type) {
	case string:
		if strings.TrimSpace(b) == "" {
			return nil, "", nil
		}
		if load && worker.IsTemplate(b) {
			return nil, ct, nil
		}
		if known && isJSON(ct) && !json.Valid([]byte(b)) {
			if load {
				return nil, "", errors.New("request.body: not valid JSON (for another format set headers.Content-Type)")
			}
			return nil, "", errors.New("request.body: not valid JSON after rendering (for another format set headers.Content-Type)")
		}
		return []byte(b), ct, nil
	default:
		if known && !isJSON(ct) {
			return nil, "", fmt.Errorf("request.body: a mapping or list is sent as JSON, but Content-Type is %q (use a string body for that format)", ct)
		}
		data, err := marshalJSON(b)
		if err != nil {
			return nil, "", fmt.Errorf("request.body: %w", err)
		}
		return data, ct, nil
	}
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// parseBody converts the response body according to mode. json: an empty
// body is null and invalid JSON is an error; raw: the body as one string.
func parseBody(mode string, data []byte, contentType string) (any, error) {
	if mode == ParseRaw {
		return string(data), nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("response is not valid JSON (content-type %q): %s (use parse: raw for text responses)", contentType, snippet(data))
	}
	return v, nil
}

// headerValues flattens response headers for the full result shape:
// lower-case names, multiple values joined with ", ".
func headerValues(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, vs := range h {
		out[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return out
}

// snippet returns the start of a body for error messages: whitespace
// collapsed, cut to snippetLimit bytes.
func snippet(data []byte) string {
	s := strings.Join(strings.Fields(string(data)), " ")
	if len(s) > snippetLimit {
		s = strings.ToValidUTF8(s[:snippetLimit], "") + "…"
	}
	return s
}

// requestError maps transport errors to short messages. The *url.Error
// wrapper is dropped: it repeats the full URL, query values included.
func requestError(loc string, err error, timeout time.Duration) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: request timed out after %s", loc, timeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: cancelled: %w", loc, err)
	}
	return fmt.Errorf("%s: %w", loc, err)
}
