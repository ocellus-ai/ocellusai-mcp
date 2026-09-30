// Package worker defines the Worker interface, the single extension point of
// the proxy, plus a registry and helpers shared by implementations.
//
// A worker *type* (prometheus, shell) is the code that executes a request; a
// worker *instance* is one configured value of that type (a URL, an allowlist).
// Tools reference instances by name (`worker: prom_prod`), the registry maps
// instance names to workers, and Type() tells which code sits behind a name.
package worker

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Worker executes a rendered request section of a tool.
type Worker interface {
	// Type is the worker type set in config (`type: prometheus`). Several
	// instances of one type may be registered under different names.
	Type() string
	// Validate checks a tool's raw (unrendered) request section at catalog load time.
	Validate(req map[string]any) error
	// Execute runs an already rendered request and returns a JSON-compatible
	// value (map[string]any / []any / primitive). Every worker returns JSON;
	// that is the contract jq relies on.
	Execute(ctx context.Context, req map[string]any) (any, error)
}

// Registry maps instance names (`worker: <name>` in YAML) to workers.
type Registry map[string]Worker

// Register adds w under name; a duplicate name is an error.
func (r Registry) Register(name string, w Worker) error {
	if _, dup := r[name]; dup {
		return fmt.Errorf("worker %q registered twice", name)
	}
	r[name] = w
	return nil
}

// Names returns the registered instance names, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for n := range r {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// CheckKeys returns an error naming request keys that are not in allowed.
func CheckKeys(req map[string]any, allowed ...string) error {
	var unknown []string
	for k := range req {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("request: unknown field(s) %s (allowed: %s)", strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// String fetches an optional string field. A present non-string value is an error.
func String(req map[string]any, key string) (string, error) {
	v, ok := req[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("request.%s: must be a string, got %T", key, v)
	}
	return s, nil
}

// IsTemplate reports whether s contains a template action and therefore can
// only be checked after rendering, i.e. in Execute rather than Validate.
func IsTemplate(s string) bool {
	return strings.Contains(s, "{{")
}

// RequiredString fetches a mandatory non-empty string field.
func RequiredString(req map[string]any, key string) (string, error) {
	s, err := String(req, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("request.%s: required", key)
	}
	return s, nil
}
