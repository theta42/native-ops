package server

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The OpenAPI document and the routes in server.go describe the same API: every route is in the
// document with its method, and the document has nothing the daemon does not serve.
func TestTheOpenAPIDocumentMatchesTheRoutes(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/[^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		method, path := strings.ToLower(m[1]), m[2]
		if method == "options" { // CORS preflight for the OAuth endpoints, not an operation
			continue
		}
		if path == "/{$}" || path == "/index.html" || path == "/docs" || strings.HasPrefix(path, "/static") {
			continue
		}
		served[method+" "+path] = true
		op, ok := doc.Paths[path][method]
		if !ok {
			t.Errorf("%s %s is served but not in openapi.yaml", m[1], path)
			continue
		}
		for _, field := range []string{"operationId", "summary", "responses", "x-native-ops-role", "tags"} {
			if op[field] == nil {
				t.Errorf("%s %s: no %s", m[1], path, field)
			}
		}
		// An unquoted comma in a flow mapping ({ description: a, b }) silently makes a key of "b".
		responses, _ := op["responses"].(map[string]any)
		for code, r := range responses {
			for k := range r.(map[string]any) {
				if k != "description" && k != "content" && k != "headers" && k != "$ref" {
					t.Errorf("%s %s: response %s has a stray key %q (quote the description)", m[1], path, code, k)
				}
			}
		}
	}
	var extra []string
	ids := map[string]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if !served[method+" "+path] {
				extra = append(extra, method+" "+path)
			}
			id, _ := op["operationId"].(string)
			if prev, dup := ids[id]; dup {
				t.Errorf("operationId %s is used by %s and %s %s", id, prev, method, path)
			}
			ids[id] = method + " " + path
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("openapi.yaml describes what server.go does not serve: %v", extra)
	}
	if len(served) < 45 {
		t.Fatalf("found only %d routes; the test's pattern is out of date", len(served))
	}
}

func TestTheOpenAPIDocumentIsServedWithoutATokenAndNamesTheVersion(t *testing.T) {
	f := setup(t, time.Minute, nil)
	res, body := f.do(t, "GET", "/openapi.json", "")
	var doc map[string]any
	if res.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &doc) != nil {
		t.Fatalf("%d %.200s", res.StatusCode, body)
	}
	if doc["openapi"] != "3.1.0" || doc["info"].(map[string]any)["version"] != "test" {
		t.Fatalf("openapi %v, info %v", doc["openapi"], doc["info"])
	}
	if res, body := f.do(t, "GET", "/openapi.yaml", ""); res.StatusCode != 200 || !strings.Contains(body, "version: test") {
		t.Fatalf("yaml: %d %.200s", res.StatusCode, body)
	}
	if f.calls.Load() != 0 {
		t.Fatal("serving the document must not read the host")
	}
}

// /docs draws the OpenAPI document for people. It is as public as the document, runs under the UI's strict CSP,
// and loads nothing from anywhere else.
func TestTheAPIReferenceIsServedWithoutATokenAndLoadsNothingRemote(t *testing.T) {
	f := setup(t, time.Minute, nil)
	res, body := f.do(t, "GET", "/docs", "")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/docs: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("csp: %q", res.Header.Get("Content-Security-Policy"))
	}
	if !strings.Contains(body, "/static/js/docs.js") {
		t.Fatalf("the page does not load its script: %.300s", body)
	}
	for _, bad := range []string{"http://", "https://", " style=", "<script>"} {
		if strings.Contains(strings.ReplaceAll(body, "<script>", ""), bad) && bad != "<script>" {
			t.Fatalf("the page must not contain %q (CSP, nothing remote)", bad)
		}
	}
	for _, asset := range []string{"/static/js/docs.js", "/static/css/docs.css"} {
		if res, _ := f.do(t, "GET", asset, ""); res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", asset, res.StatusCode)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("serving the reference must not read the host")
	}
}
