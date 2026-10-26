package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mozership/probe/actionref"
	"github.com/mozership/probe/actionrpc"
)

// server answers every request with code and body, and records the last
// request it got.
func server(t *testing.T, code int, contentType, body string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &rec.payload)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, rec
}

type recorded struct {
	method  string
	header  http.Header
	payload map[string]any
}

func TestRun(t *testing.T) {
	ts, rec := server(t, 200, "application/json", `{"data":{"viewer":{"login":"linyows"}}}`)

	ret, err := (&Action{}).Run(map[string]any{
		"url":            ts.URL,
		"query":          "query Me($n: Int) { viewer { login } }",
		"variables":      map[string]any{"n": 1},
		"operation_name": "Me",
		"headers":        map[string]any{"Authorization": "Bearer x"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if rec.method != http.MethodPost {
		t.Errorf("method = %s, want POST", rec.method)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer x" {
		t.Errorf("Authorization = %q", got)
	}
	if rec.payload["query"] != "query Me($n: Int) { viewer { login } }" || rec.payload["operationName"] != "Me" {
		t.Errorf("payload = %v", rec.payload)
	}
	if vars, _ := rec.payload["variables"].(map[string]any); vars["n"] != float64(1) {
		t.Errorf("variables = %v", rec.payload["variables"])
	}

	if ret["status"] != 0 {
		t.Errorf("status = %v, want 0", ret["status"])
	}
	res := ret["res"].(map[string]any)
	if res["code"] != 200 {
		t.Errorf("res.code = %v", res["code"])
	}
	login := res["data"].(map[string]any)["viewer"].(map[string]any)["login"]
	if login != "linyows" {
		t.Errorf("res.data.viewer.login = %v", login)
	}
	if errs := res["errors"].([]any); len(errs) != 0 {
		t.Errorf("res.errors = %v, want none", errs)
	}
	if _, err := time.ParseDuration(ret["rt"].(string)); err != nil {
		t.Errorf("rt = %v: %v", ret["rt"], err)
	}
}

func TestRunOmitsUnsetFields(t *testing.T) {
	ts, rec := server(t, 200, "application/json", `{"data":{}}`)
	if _, err := (&Action{}).Run(map[string]any{"url": ts.URL, "query": "{ a }"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.payload["variables"]; ok {
		t.Error("variables sent although unset")
	}
	if _, ok := rec.payload["operationName"]; ok {
		t.Error("operationName sent although unset")
	}
}

// A response is a result even when it reports a failure, so that a test can
// assert on it.
func TestRunFailedResponses(t *testing.T) {
	tests := []struct {
		name        string
		code        int
		contentType string
		body        string
		check       func(t *testing.T, res map[string]any)
	}{
		{
			name: "graphql errors", code: 200, contentType: "application/json",
			body: `{"data":null,"errors":[{"message":"not authorized"}]}`,
			check: func(t *testing.T, res map[string]any) {
				errs := res["errors"].([]any)
				if len(errs) != 1 || errs[0].(map[string]any)["message"] != "not authorized" {
					t.Errorf("res.errors = %v", errs)
				}
			},
		},
		{
			name: "http error", code: 500, contentType: "application/json",
			body: `{"errors":[{"message":"boom"}]}`,
			check: func(t *testing.T, res map[string]any) {
				if res["code"] != 500 {
					t.Errorf("res.code = %v", res["code"])
				}
			},
		},
		{
			name: "not json", code: 502, contentType: "text/html",
			body: `<h1>Bad Gateway</h1>`,
			check: func(t *testing.T, res map[string]any) {
				if res["body"] != "<h1>Bad Gateway</h1>" {
					t.Errorf("res.body = %v", res["body"])
				}
				if res["data"] != nil {
					t.Errorf("res.data = %v, want nil", res["data"])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := server(t, tt.code, tt.contentType, tt.body)
			ret, err := (&Action{}).Run(map[string]any{"url": ts.URL, "query": "{ a }"})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if ret["status"] != 1 {
				t.Errorf("status = %v, want 1", ret["status"])
			}
			tt.check(t, ret["res"].(map[string]any))
		})
	}
}

func TestRunNoResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	if _, err := (&Action{}).Run(map[string]any{"url": url, "query": "{ a }"}); err == nil {
		t.Fatal("Run() succeeded without a server")
	}
}

func TestRunTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(ts.Close)
	_, err := (&Action{}).Run(map[string]any{"url": ts.URL, "query": "{ a }", "timeout": "50ms"})
	if err == nil || !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("Run() error = %v, want a timeout", err)
	}
}

func TestParseRequestErrors(t *testing.T) {
	tests := []struct {
		name    string
		with    map[string]any
		wantErr string
	}{
		{name: "no url", with: map[string]any{"query": "{ a }"}, wantErr: "with.url"},
		{name: "bad scheme", with: map[string]any{"url": "ftp://x", "query": "{ a }"}, wantErr: "http or https"},
		{name: "no query", with: map[string]any{"url": "http://x"}, wantErr: "with.query"},
		{name: "blank query", with: map[string]any{"url": "http://x", "query": "  "}, wantErr: "with.query"},
		{name: "variables", with: map[string]any{"url": "http://x", "query": "{ a }", "variables": "x"}, wantErr: "with.variables"},
		{name: "operation_name", with: map[string]any{"url": "http://x", "query": "{ a }", "operation_name": 1}, wantErr: "with.operation_name"},
		{name: "headers", with: map[string]any{"url": "http://x", "query": "{ a }", "headers": "x"}, wantErr: "with.headers"},
		{name: "timeout", with: map[string]any{"url": "http://x", "query": "{ a }", "timeout": "soon"}, wantErr: "with.timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRequest(tt.with)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseRequest() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		in   any
		want time.Duration
	}{
		{"10s", 10 * time.Second},
		{5, 5 * time.Second},
		{1.5, 1500 * time.Millisecond},
		{"0", 0},
	}
	for _, tt := range tests {
		got, err := parseTimeout(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("parseTimeout(%v) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
}

func TestRunStepReadOnly(t *testing.T) {
	ts, rec := server(t, 200, "application/json", `{"data":{}}`)
	host := strings.TrimPrefix(ts.URL, "http://")
	guard := actionrpc.Guard{ReadOnly: true, AllowHosts: []string{host}}
	tests := []struct {
		name          string
		query         string
		operationName string
		refused       string
	}{
		{name: "query", query: "query Me { viewer { login } }"},
		{name: "shorthand", query: "{ viewer { login } }"},
		{name: "named query of several", query: "query A { a } mutation B { b }", operationName: "A"},
		{name: "mutation", query: "mutation { deleteUser(id: 1) }", refused: "the mutation may write"},
		{name: "subscription", query: "subscription { events }", refused: "the subscription may write"},
		{name: "named mutation of several", query: "query A { a } mutation B { b }", operationName: "B", refused: "the mutation may write"},
		{name: "several without a name", query: "query A { a } query B { b }", refused: "has 2 operations"},
		{name: "unknown name", query: "query A { a }", operationName: "Z", refused: "no operation named Z"},
		{name: "does not parse", query: "query {", refused: "does not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.method = ""
			with := map[string]any{"url": ts.URL, "query": tt.query}
			if tt.operationName != "" {
				with["operation_name"] = tt.operationName
			}
			_, _, err := (&Action{}).RunStep(actionrpc.Call{With: with, Guard: guard})
			if tt.refused == "" {
				if err != nil {
					t.Fatalf("RunStep() error = %v, want the query sent", err)
				}
				if rec.method != http.MethodPost {
					t.Error("the query was not sent")
				}
				return
			}
			if !actionrpc.IsRefused(err) || !strings.Contains(err.Error(), tt.refused) {
				t.Errorf("RunStep() error = %v, want a refusal saying %q", err, tt.refused)
			}
			if rec.method != "" {
				t.Error("a refused operation was sent")
			}
		})
	}

	// Without the guard a mutation is sent as it is.
	rec.method = ""
	if _, _, err := (&Action{}).RunStep(actionrpc.Call{With: map[string]any{"url": ts.URL, "query": "mutation { x }"}}); err != nil || rec.method != http.MethodPost {
		t.Errorf("RunStep() without a guard: error = %v, sent = %v", err, rec.method != "")
	}
}

func TestRunStepAllowHost(t *testing.T) {
	ts, rec := server(t, 200, "application/json", `{"data":{}}`)
	u, _ := url.Parse(ts.URL)
	with := map[string]any{"url": ts.URL, "query": "{ a }"}

	_, _, err := (&Action{}).RunStep(actionrpc.Call{With: with, Guard: actionrpc.Guard{AllowHosts: []string{"api.example.com"}}})
	if !actionrpc.IsRefused(err) || rec.method != "" {
		t.Errorf("RunStep() error = %v, sent = %v; want a refusal before sending", err, rec.method != "")
	}

	// A host without a port is any port of it.
	if _, _, err := (&Action{}).RunStep(actionrpc.Call{With: with, Guard: actionrpc.Guard{AllowHosts: []string{u.Hostname()}}}); err != nil {
		t.Errorf("RunStep() error = %v for an allowed host", err)
	}

	// A redirect to a host the run does not allow is refused.
	front := httptest.NewServer(http.RedirectHandler(ts.URL, http.StatusTemporaryRedirect))
	t.Cleanup(front.Close)
	fu, _ := url.Parse(front.URL)
	rec.method = ""
	_, _, err = (&Action{}).RunStep(actionrpc.Call{
		With:  map[string]any{"url": front.URL, "query": "{ a }"},
		Guard: actionrpc.Guard{AllowHosts: []string{fu.Host}},
	})
	if !actionrpc.IsRefused(err) || rec.method != "" {
		t.Errorf("RunStep() error = %v, sent = %v; want the redirect refused", err, rec.method != "")
	}
}

func TestRunRefusesUnknownKey(t *testing.T) {
	_, err := (&Action{}).Run(map[string]any{"url": "http://localhost", "query": "{ a }", "varables": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "not varables") {
		t.Errorf("Run() error = %v, want one naming varables", err)
	}
}

// TestManifestsDeclare checks that action.yml, and the one the e2e workflows
// run the action by, declare the params and the guard the action has, so
// that probe check and the guard of a run take it as it is. probe manifest
// keeps what action.yml declares in the one of a release.
func TestManifestsDeclare(t *testing.T) {
	for _, name := range []string{"action.yml", "e2e/action/action.yml"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		m, err := actionref.ParseManifest(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(m.Params, params) || !slices.Equal(m.Guard, keeps) {
			t.Errorf("%s declares params %v and guard %v, want %v and %v", name, m.Params, m.Guard, params, keeps)
		}
	}
}
