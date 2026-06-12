package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/store"
)

// newServer seeds a store with one collection (folder + preset + secret)
// pointed at a live echo API.
func newServer(t *testing.T) (*Server, int64) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"auth":    r.Header.Get("Authorization"),
			"channel": r.Header.Get("X-Channel"),
			"team":    r.Header.Get("X-Team"),
		})
	}))
	t.Cleanup(api.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	col := &store.Collection{Name: "AML", Headers: map[string]string{"X-Team": "qe"},
		Vars: map[string]string{"baseUrl": api.URL}}
	if err := db.CreateCollection(col); err != nil {
		t.Fatal(err)
	}
	req := &store.Request{CollectionID: col.ID, Spec: &dsl.Request{
		Name: "Echo", Method: "GET", URL: "{{baseUrl}}/echo",
		Auth: &dsl.Auth{Type: "bearer", Token: "{{apiToken}}"},
		Assertions: []dsl.Assertion{
			{Type: "status", Equals: float64(200)}, // JSON round-trip makes numbers float64
			{Type: "jsonpath", Path: "$.team", Equals: "qe"},
		},
	}}
	if err := db.CreateRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEnvironment(&store.Environment{Name: "local",
		Vars: map[string]string{}, Secrets: []string{"apiToken"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePreset(&store.Preset{Name: "gw",
		Headers:     []store.PresetHeader{{Key: "X-Channel", Value: "MOBILE_IOS"}, {Key: "X-Gw-Key", Value: "topsecret", Secret: true}},
		Attachments: []store.Attachment{{CollectionID: &col.ID}}}); err != nil {
		t.Fatal(err)
	}

	return &Server{
		DB: db, Engine: engine.NewOptions(),
		Getenv: func(k string) string {
			if k == "RELAY_SECRET_APITOKEN" {
				return "hunter2"
			}
			return ""
		},
	}, req.ID
}

func call(t *testing.T, s *Server, method, url, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, rd)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

func TestState(t *testing.T) {
	s, _ := newServer(t)
	rec, doc := call(t, s, "GET", "/api/state", "")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	cols := doc["collections"].([]any)
	if len(cols) != 1 {
		t.Fatalf("collections = %d", len(cols))
	}
	col := cols[0].(map[string]any)
	if col["name"] != "AML" || len(col["requests"].([]any)) != 1 {
		t.Errorf("col = %v", col)
	}
	// Secret preset values must not appear in state.
	if strings.Contains(rec.Body.String(), "topsecret") {
		t.Error("secret preset value leaked in /api/state")
	}
	presets := doc["presets"].([]any)
	if len(presets) != 1 {
		t.Errorf("presets = %v", presets)
	}
}

func TestRequestCRUDOverHTTP(t *testing.T) {
	s, _ := newServer(t)
	rec, doc := call(t, s, "POST", "/api/requests",
		`{"collectionId":1,"spec":{"name":"New","method":"POST","url":"{{baseUrl}}/x"}}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %v", rec.Code, doc)
	}
	id := int64(doc["id"].(float64))

	rec, doc = call(t, s, "PUT", "/api/requests/"+itoa(id),
		`{"spec":{"name":"Renamed","method":"PUT","url":"{{baseUrl}}/y"}}`)
	if rec.Code != 200 {
		t.Fatalf("update: %d %v", rec.Code, doc)
	}
	rec, doc = call(t, s, "GET", "/api/requests/"+itoa(id), "")
	spec := doc["spec"].(map[string]any)
	if spec["name"] != "Renamed" || spec["method"] != "PUT" {
		t.Errorf("spec = %v", spec)
	}
	rec, _ = call(t, s, "DELETE", "/api/requests/"+itoa(id), "")
	if rec.Code != 200 {
		t.Errorf("delete: %d", rec.Code)
	}
	rec, _ = call(t, s, "GET", "/api/requests/"+itoa(id), "")
	if rec.Code != 404 {
		t.Errorf("get deleted: %d", rec.Code)
	}
}

func TestSendWithPresetsAndSecrets(t *testing.T) {
	s, reqID := newServer(t)
	rec, doc := call(t, s, "POST", "/api/send", `{"requestId":`+itoa(reqID)+`,"env":"local"}`)
	if rec.Code != 200 {
		t.Fatalf("send: %d %v", rec.Code, doc)
	}
	body := doc["body"].(string)
	// Upstream got the real bearer token and the preset header.
	if !strings.Contains(body, "Bearer hunter2") || !strings.Contains(body, "MOBILE_IOS") {
		t.Errorf("upstream body = %s", body)
	}
	// Display headers are masked: env secret and secret preset value.
	rh := doc["requestHeaders"].(map[string]any)
	if strings.Contains(rh["Authorization"].(string), "hunter2") {
		t.Errorf("env secret leaked: %v", rh["Authorization"])
	}
	if got := rh["X-Gw-Key"].(string); strings.Contains(got, "topsecret") {
		t.Errorf("preset secret leaked: %v", got)
	}
	// Collection header inherited.
	if rh["X-Team"].(string) != "qe" {
		t.Errorf("inherited header missing: %v", rh)
	}
	// Assertions evaluated.
	asserts := doc["assertions"].([]any)
	if len(asserts) != 2 {
		t.Fatalf("assertions = %v", asserts)
	}
	for _, a := range asserts {
		if !a.(map[string]any)["passed"].(bool) {
			t.Errorf("assertion failed: %v", a)
		}
	}

	// History recorded; stored body retrievable; list carries no body.
	_, list := call(t, s, "GET", "/api/history", "")
	_ = list
	rec, _ = call(t, s, "GET", "/api/history", "")
	var entries []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 1 {
		t.Fatalf("history = %v", entries)
	}
	id := int64(entries[0]["id"].(float64))
	rec, doc = call(t, s, "GET", "/api/history/"+itoa(id), "")
	if rec.Code != 200 || !strings.Contains(doc["body"].(string), "MOBILE_IOS") {
		t.Errorf("history entry: %d %v", rec.Code, doc)
	}

	// Stats reflect the send.
	rec, doc = call(t, s, "GET", "/api/requests/"+itoa(reqID)+"/stats", "")
	if rec.Code != 200 || doc["count"].(float64) != 1 || doc["successRate"].(float64) != 1 {
		t.Errorf("stats: %d %v", rec.Code, doc)
	}
}

func TestRunCollection(t *testing.T) {
	s, _ := newServer(t)
	rec, doc := call(t, s, "POST", "/api/run", `{"collectionId":1,"env":"local"}`)
	if rec.Code != 200 {
		t.Fatalf("run: %d %v", rec.Code, doc)
	}
	if doc["executed"].(float64) != 1 || doc["passed"].(float64) != 1 || doc["failed"].(float64) != 0 {
		t.Errorf("summary = %v", doc)
	}
}

func TestImportPostmanAndExportK6(t *testing.T) {
	s, _ := newServer(t)
	postman := `{"info":{"name":"Imported"},"item":[{"name":"Ping","request":{"method":"GET","url":"https://x.test/ping"}}]}`
	rec, doc := call(t, s, "POST", "/api/import/postman", postman)
	if rec.Code != 200 || doc["requests"].(float64) != 1 {
		t.Fatalf("import: %d %v", rec.Code, doc)
	}
	colID := int64(doc["collectionId"].(float64))

	req := httptest.NewRequest("GET", "/api/export?format=k6&collection="+itoa(colID), nil)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("export: %d %s", out.Code, out.Body.String())
	}
	script := out.Body.String()
	if !strings.Contains(script, "k6/http") || !strings.Contains(script, "https://x.test/ping") {
		t.Errorf("k6 script missing content:\n%s", script)
	}
}

func TestEnvironmentAndPresetEndpoints(t *testing.T) {
	s, _ := newServer(t)
	rec, _ := call(t, s, "PUT", "/api/environments/sit", `{"vars":{"baseUrl":"https://sit"},"secrets":["k"]}`)
	if rec.Code != 200 {
		t.Fatalf("env put: %d", rec.Code)
	}
	rec, _ = call(t, s, "GET", "/api/environments", "")
	if !strings.Contains(rec.Body.String(), `"sit"`) {
		t.Error("env list missing sit")
	}
	rec, _ = call(t, s, "DELETE", "/api/environments/sit", "")
	if rec.Code != 200 {
		t.Errorf("env delete: %d", rec.Code)
	}

	rec, doc := call(t, s, "POST", "/api/presets", `{"name":"json-defaults","headers":[{"key":"Accept","value":"application/json"}]}`)
	if rec.Code != 200 {
		t.Fatalf("preset create: %d %v", rec.Code, doc)
	}
	id := int64(doc["id"].(float64))
	// Updating a secret header with empty value keeps the stored value.
	rec, _ = call(t, s, "PUT", "/api/presets/1",
		`{"name":"gw","headers":[{"key":"X-Gw-Key","value":"","secret":true}],"attachments":[{"collectionId":1}]}`)
	if rec.Code != 200 {
		t.Fatalf("preset update: %d", rec.Code)
	}
	ps, _ := s.DB.Presets()
	for _, p := range ps {
		if p.Name == "gw" && p.Headers[0].Value != "topsecret" {
			t.Errorf("secret wiped on masked round-trip: %+v", p.Headers)
		}
	}
	rec, _ = call(t, s, "DELETE", "/api/presets/"+itoa(id), "")
	if rec.Code != 200 {
		t.Errorf("preset delete: %d", rec.Code)
	}
}

func TestIndexServed(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "API Workbench") {
		t.Errorf("index: %d", rec.Code)
	}
}

func itoa(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestRequestCurl(t *testing.T) {
	s, reqID := newServer(t)
	rec, doc := call(t, s, "GET", "/api/requests/"+itoa(reqID)+"/curl?env=local", "")
	if rec.Code != 200 {
		t.Fatalf("curl: %d %v", rec.Code, doc)
	}
	cmd := doc["curl"].(string)
	if !strings.HasPrefix(cmd, "curl") || !strings.Contains(cmd, "/echo") {
		t.Errorf("curl = %s", cmd)
	}
	// Env secret becomes a shell reference; preset secret value is masked.
	if strings.Contains(cmd, "hunter2") || !strings.Contains(cmd, "$RELAY_SECRET_APITOKEN") {
		t.Errorf("env secret handling wrong: %s", cmd)
	}
	if strings.Contains(cmd, "topsecret") {
		t.Errorf("preset secret leaked: %s", cmd)
	}
	// Inherited preset/collection headers present.
	if !strings.Contains(cmd, "MOBILE_IOS") || !strings.Contains(cmd, "X-Team: qe") {
		t.Errorf("inherited headers missing: %s", cmd)
	}
}

func TestImportCurlEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec, doc := call(t, s, "POST", "/api/import/curl",
		`{"collectionId":1,"curl":"curl -X POST -H 'Content-Type: application/json' --data-raw '{\"a\":1}' https://x.test/v1/do"}`)
	if rec.Code != 200 {
		t.Fatalf("import curl: %d %v", rec.Code, doc)
	}
	spec := doc["spec"].(map[string]any)
	if spec["method"] != "POST" || spec["url"] != "https://x.test/v1/do" {
		t.Errorf("spec = %v", spec)
	}
	if spec["body"].(map[string]any)["type"] != "json" {
		t.Errorf("body = %v", spec["body"])
	}

	rec, doc = call(t, s, "POST", "/api/import/curl", `{"collectionId":1,"curl":"wget https://x.test"}`)
	if rec.Code != 422 {
		t.Errorf("non-curl should 422, got %d %v", rec.Code, doc)
	}
}

func TestExportPostmanEndpoint(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest("GET", "/api/export?format=postman&collection=1", nil)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("export postman: %d %s", out.Code, out.Body.String())
	}
	var doc struct {
		Info struct{ Name, Schema string }
		Item []struct{ Name string }
	}
	if err := json.Unmarshal(out.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid postman json: %v", err)
	}
	if doc.Info.Name != "AML" || len(doc.Item) != 1 || doc.Item[0].Name != "Echo" {
		t.Errorf("doc = %+v", doc)
	}
	// The secret preset value must not appear in the exported collection.
	if strings.Contains(out.Body.String(), "topsecret") {
		t.Error("preset secret leaked into postman export")
	}
	// Non-secret preset + collection headers are flattened in.
	if !strings.Contains(out.Body.String(), "MOBILE_IOS") || !strings.Contains(out.Body.String(), "X-Team") {
		t.Error("inherited headers missing from postman export")
	}
}

func TestMoveRequestToFolder(t *testing.T) {
	s, reqID := newServer(t)
	rec, doc := call(t, s, "POST", "/api/folders", `{"collectionId":1,"name":"verify","headers":{},"vars":{}}`)
	if rec.Code != 200 {
		t.Fatalf("folder create: %d %v", rec.Code, doc)
	}
	folderID := int64(doc["id"].(float64))

	rec, _ = call(t, s, "PUT", "/api/requests/"+itoa(reqID),
		`{"folderId":`+itoa(folderID)+`,"spec":{"name":"Echo","method":"GET","url":"{{baseUrl}}/echo"}}`)
	if rec.Code != 200 {
		t.Fatalf("move: %d", rec.Code)
	}
	rec, doc = call(t, s, "GET", "/api/requests/"+itoa(reqID), "")
	if rec.Code != 200 || doc["folderId"] == nil || int64(doc["folderId"].(float64)) != folderID {
		t.Errorf("folderId after move = %v", doc["folderId"])
	}
	// Folder rename via PATCH.
	rec, _ = call(t, s, "PATCH", "/api/folders/"+itoa(folderID), `{"collectionId":1,"name":"renamed","headers":{"X-F":"1"},"vars":{}}`)
	if rec.Code != 200 {
		t.Errorf("folder patch: %d", rec.Code)
	}
}
