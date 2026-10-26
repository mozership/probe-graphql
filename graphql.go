package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/mozership/probe/actionrpc"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// DefaultTimeout bounds a request whose step does not set with.timeout.
const DefaultTimeout = 30 * time.Second

// maxRedirects is how many redirects a request follows, as many as
// net/http follows when it is not told otherwise.
const maxRedirects = 10

// params are the keys the action takes in with. action.yml declares them,
// for probe check to report a key the action does not take.
var params = []string{"url", "query", "variables", "operation_name", "headers", "timeout"}

// keeps are the kinds of guard the action keeps to, as action.yml declares
// them: it sends only a query under read-only, and connects only to a host
// the run allows.
var keeps = []string{actionrpc.KindReadOnly, actionrpc.KindAllowHost}

// Action sends the GraphQL query a step describes and returns the response.
type Action struct {
	log hclog.Logger
	// client sends the request. It defaults to http.DefaultTransport.
	client *http.Client
}

// Run sends the query in with, under no guard.
func (a *Action) Run(with map[string]any) (map[string]any, error) {
	ret, _, err := a.RunStep(actionrpc.Call{With: with})
	return ret, err
}

// RunStep sends the query in call.With under the guard of the run. A
// response the server sends is a result, whatever its status code or
// GraphQL errors; only a request that gets no response, or one the guard
// refuses, is an error.
func (a *Action) RunStep(call actionrpc.Call) (map[string]any, map[string]any, error) {
	if a.log == nil {
		a.log = hclog.NewNullLogger()
	}
	actionrpc.LogParams(a.log, "received request parameters", call.With)

	req, err := parseRequest(call.With)
	if err != nil {
		return nil, nil, err
	}
	if err := checkReadOnly(call.Guard, req); err != nil {
		return nil, nil, err
	}
	ret, err := a.do(req, call.Guard)
	actionrpc.LogOutcome(a.log, "graphql request", ret, err)
	return ret, nil, err
}

// checkReadOnly returns a Refused error when the run is read-only and the
// operation to run is not a query, or cannot be told: a mutation or a
// subscription, a document that does not parse, or one with no operation
// that operation_name selects.
func checkReadOnly(guard actionrpc.Guard, r *request) error {
	if !guard.ReadOnly {
		return nil
	}
	doc, err := parser.ParseQuery(&ast.Source{Input: r.query})
	if err != nil {
		return actionrpc.Refuse("the run is read-only, and the query does not parse, so it cannot be told to read: %v", err)
	}
	op := doc.Operations.ForName(r.operationName)
	if op == nil {
		if r.operationName == "" {
			return actionrpc.Refuse("the run is read-only, and the document has %d operations; name the one to run with operation_name", len(doc.Operations))
		}
		return actionrpc.Refuse("the run is read-only, and the document has no operation named %s", r.operationName)
	}
	if op.Operation != ast.Query {
		return actionrpc.Refuse("the run is read-only, and the %s may write; only a query is sent", op.Operation)
	}
	return nil
}

// checkHost returns a Refused error when the guard does not allow the host
// of u. A host without a port is taken at the port of its scheme.
func checkHost(guard actionrpc.Guard, u *url.URL) error {
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	return guard.CheckHost(host)
}

type request struct {
	url           string
	query         string
	variables     map[string]any
	operationName string
	headers       map[string]string
	timeout       time.Duration
}

func parseRequest(with map[string]any) (*request, error) {
	r := &request{timeout: DefaultTimeout, headers: map[string]string{}}

	for k := range with {
		if !slices.Contains(params, k) {
			return nil, fmt.Errorf("graphql action takes %s, not %s", strings.Join(params, ", "), k)
		}
	}

	r.url, _ = with["url"].(string)
	if r.url == "" {
		return nil, errors.New("graphql action requires with.url")
	}
	u, err := url.Parse(r.url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("with.url must be an http or https URL, not %q", r.url)
	}

	r.query, _ = with["query"].(string)
	if strings.TrimSpace(r.query) == "" {
		return nil, errors.New("graphql action requires with.query")
	}

	if v, ok := with["variables"]; ok && v != nil {
		vars, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("with.variables must be an object, not %T", v)
		}
		r.variables = vars
	}

	if v, ok := with["operation_name"]; ok {
		if r.operationName, ok = v.(string); !ok {
			return nil, fmt.Errorf("with.operation_name must be a string, not %T", v)
		}
	}

	if v, ok := with["headers"]; ok && v != nil {
		headers, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("with.headers must be an object, not %T", v)
		}
		for k, v := range headers {
			r.headers[k] = fmt.Sprint(v)
		}
	}

	if v, ok := with["timeout"]; ok {
		if r.timeout, err = parseTimeout(v); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// parseTimeout accepts a duration string such as "10s" or a number of
// seconds, as the http action does.
func parseTimeout(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		d, err := time.ParseDuration(t)
		if err != nil {
			return 0, fmt.Errorf("with.timeout: %w", err)
		}
		return d, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("with.timeout must be a duration or a number of seconds, not %T", v)
	}
}

func (a *Action) do(r *request, guard actionrpc.Guard) (map[string]any, error) {
	payload := map[string]any{"query": r.query}
	if r.variables != nil {
		payload["variables"] = r.variables
	}
	if r.operationName != "" {
		payload["operationName"] = r.operationName
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("with.variables cannot be sent as JSON: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/graphql-response+json, application/json")
	httpReq.Header.Set("User-Agent", "probe-graphql/"+version)
	for k, v := range r.headers {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{}
	if a.client != nil {
		c := *a.client
		client = &c
	}
	client.Timeout = r.timeout
	// A redirect is followed only to a host the guard allows.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return checkHost(guard, req.URL)
	}
	if err := checkHost(guard, httpReq.URL); err != nil {
		return nil, err
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		// A redirect the guard refused is a refusal, not a failure to reach
		// the server.
		if refused := (*actionrpc.Refused)(nil); errors.As(err, &refused) {
			return nil, refused
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	rt := time.Since(start)
	if err != nil {
		return nil, err
	}

	res := map[string]any{
		"code":    resp.StatusCode,
		"status":  resp.Status,
		"headers": flattenHeaders(resp.Header),
		"body":    string(raw),
		"data":    nil,
		"errors":  []any{},
	}

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		res["body"] = parsed
		res["rawbody"] = string(raw)
		if obj, isObj := parsed.(map[string]any); isObj {
			res["data"] = obj["data"]
			if errs, isList := obj["errors"].([]any); isList {
				res["errors"] = errs
			}
		}
	} else {
		ok = false
	}
	if errs := res["errors"].([]any); len(errs) > 0 {
		ok = false
	}

	status := 0
	if !ok {
		status = 1
	}

	return map[string]any{
		"req": map[string]any{
			"url":            r.url,
			"query":          r.query,
			"variables":      r.variables,
			"operation_name": r.operationName,
			"headers":        flattenHeaders(httpReq.Header),
		},
		"res":    res,
		"rt":     rt.String(),
		"status": status,
	}, nil
}

func flattenHeaders(h http.Header) map[string]any {
	m := make(map[string]any, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}
