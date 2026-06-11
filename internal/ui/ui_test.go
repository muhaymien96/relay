package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muhaymien96/relay/internal/engine"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"auth": r.Header.Get("Authorization"),
		})
	}))
	t.Cleanup(api.Close)

	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("01-echo.req.toml", `name = "Echo"
url = "{{baseUrl}}/echo"
[auth]
type = "bearer"
token = "{{apiToken}}"
`)
	write("environments/local.toml", "secrets = [\"apiToken\"]\n\n[vars]\nbaseUrl = \""+api.URL+"\"\n")

	return &Server{
		Root:   root,
		Engine: engine.NewOptions(),
		Getenv: func(k string) string {
			if k == "RELAY_SECRET_APITOKEN" {
				return "hunter2"
			}
			return ""
		},
	}, api
}

func get(t *testing.T, h http.Handler, url string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return do(t, h, httptest.NewRequest("GET", url, nil))
}

func post(t *testing.T, h http.Handler, url, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h, req)
}

func do(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("non-JSON response (%d): %s", rec.Code, rec.Body.String())
	}
	return rec, doc
}

func TestWorkspaceListing(t *testing.T) {
	s, _ := newServer(t)
	rec, doc := get(t, s.Handler(), "/api/workspace")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	reqs := doc["requests"].([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["name"] != "Echo" {
		t.Errorf("requests = %v", reqs)
	}
	envs := doc["environments"].([]any)
	if len(envs) != 1 || envs[0] != "local" {
		t.Errorf("environments = %v", envs)
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	s, _ := newServer(t)
	for _, p := range []string{"../../etc/passwd.toml", "/etc/passwd", "x.req.json"} {
		rec, _ := get(t, s.Handler(), "/api/file?path="+p)
		if rec.Code != 400 && rec.Code != 404 {
			t.Errorf("%s: status %d, want 400/404", p, rec.Code)
		}
	}
	// Direct escape via save too.
	rec, _ := post(t, s.Handler(), "/api/file", `{"path":"../evil.toml","content":"x"}`)
	if rec.Code != 400 {
		t.Errorf("save escape: status %d", rec.Code)
	}
}

func TestSaveValidatesRequests(t *testing.T) {
	s, _ := newServer(t)
	rec, _ := post(t, s.Handler(), "/api/file", `{"path":"01-echo.req.toml","content":"not [valid toml"}`)
	if rec.Code != 422 {
		t.Errorf("invalid content: status %d, want 422", rec.Code)
	}
	rec, _ = post(t, s.Handler(), "/api/file", `{"path":"01-echo.req.toml","content":"name = \"Echo2\"\nurl = \"http://x.test\"\n"}`)
	if rec.Code != 200 {
		t.Errorf("valid save: status %d", rec.Code)
	}
	data, _ := os.ReadFile(filepath.Join(s.Root, "01-echo.req.toml"))
	if !strings.Contains(string(data), "Echo2") {
		t.Error("save did not persist")
	}
}

func TestSendMasksSecrets(t *testing.T) {
	s, api := newServer(t)
	rec, doc := post(t, s.Handler(), "/api/send", `{"path":"01-echo.req.toml","env":"local"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %v", rec.Code, doc)
	}
	if doc["status"].(float64) != 200 {
		t.Errorf("upstream status = %v", doc["status"])
	}
	// The server actually received the real token...
	if !strings.Contains(doc["body"].(string), "Bearer hunter2") {
		t.Errorf("upstream did not get the token: %v", doc["body"])
	}
	// ...but the request headers shown to the UI are masked.
	rh := doc["requestHeaders"].(map[string]any)
	auth := rh["Authorization"].(string)
	if strings.Contains(auth, "hunter2") || !strings.Contains(auth, "••••••") {
		t.Errorf("Authorization shown to UI = %q", auth)
	}
	if doc["timing"].(map[string]any)["total"].(float64) <= 0 {
		t.Error("timing missing")
	}
	_ = api
}

func TestIndexServed(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "RELAY") {
		t.Errorf("index: %d", rec.Code)
	}
}
