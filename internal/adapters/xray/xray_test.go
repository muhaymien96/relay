package xray_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muhaymien96/relay/internal/adapters/tm"
	"github.com/muhaymien96/relay/internal/adapters/xray"
)

func TestPushExecution(t *testing.T) {
	// Stub auth endpoint returns a bearer token.
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`"test-bearer-token"`))
	}))
	defer authSrv.Close()

	// Stub GraphQL endpoint records the request and returns a canned response.
	var gotBody map[string]any
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"data": {
				"createTestExecution": {
					"testExecution": {
						"issueId": "10042",
						"jira": {"key": "AML-999"}
					},
					"createdTests": [],
					"warnings": []
				}
			}
		}`))
	}))
	defer gqlSrv.Close()

	client := xray.New(xray.Config{
		ClientID:     "test-id",
		ClientSecret: "test-secret",
		AuthURL:      authSrv.URL,
		GQLURL:       gqlSrv.URL,
	})

	exec := tm.Execution{
		ProjectKey:  "AML",
		TestPlanKey: "AML-88",
		Summary:     "Relay automated run",
		StartedAt:   time.Now().Add(-5 * time.Second),
		FinishedAt:  time.Now(),
		Results: []tm.TestResult{
			{TestKey: "AML-T142", Name: "Verify Individual", Status: tm.StatusPASS, DurationMs: 312},
			{TestKey: "AML-T143", Name: "Verify Entity", Status: tm.StatusFAIL, Comment: "status 500", DurationMs: 98},
		},
	}

	key, err := client.PushExecution(exec)
	if err != nil {
		t.Fatalf("PushExecution: %v", err)
	}
	if key != "AML-999" {
		t.Errorf("expected key AML-999, got %q", key)
	}
	// Verify the request contained the test plan key.
	vars, _ := gotBody["variables"].(map[string]any)
	te, _ := vars["testExecIssue"].(map[string]any)
	if te["testPlanKey"] != "AML-88" {
		t.Errorf("expected testPlanKey AML-88, got %v", te["testPlanKey"])
	}
}

func TestGetTest(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"test-bearer-token"`))
	}))
	defer authSrv.Close()

	var gotBody map[string]any
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"getTests": {
					"results": [{"issueId": "1", "jira": {"key": "AML-T142", "summary": "Verify Individual"}}]
				}
			}
		}`))
	}))
	defer gqlSrv.Close()

	client := xray.New(xray.Config{ClientID: "id", ClientSecret: "secret", AuthURL: authSrv.URL, GQLURL: gqlSrv.URL})
	ref, err := client.GetTest("AML-T142")
	if err != nil {
		t.Fatalf("GetTest: %v", err)
	}
	if ref == nil || ref.Key != "AML-T142" || ref.Summary != "Verify Individual" {
		t.Errorf("unexpected ref: %+v", ref)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	if vars["jql"] != `key = "AML-T142"` {
		t.Errorf("unexpected jql: %v", vars["jql"])
	}
}

func TestGetTestNotFound(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"test-bearer-token"`))
	}))
	defer authSrv.Close()
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"getTests": {"results": []}}}`))
	}))
	defer gqlSrv.Close()

	client := xray.New(xray.Config{ClientID: "id", ClientSecret: "secret", AuthURL: authSrv.URL, GQLURL: gqlSrv.URL})
	ref, err := client.GetTest("AML-T999")
	if err != nil {
		t.Fatalf("GetTest: %v", err)
	}
	if ref != nil {
		t.Errorf("expected nil ref for not-found test, got %+v", ref)
	}
}

func TestCreateTest(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"test-bearer-token"`))
	}))
	defer authSrv.Close()

	var gotBody map[string]any
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{
			"data": { "createTest": { "test": {"issueId": "55", "jira": {"key": "AML-T200"}}, "warnings": [] } }
		}`))
	}))
	defer gqlSrv.Close()

	client := xray.New(xray.Config{ClientID: "id", ClientSecret: "secret", AuthURL: authSrv.URL, GQLURL: gqlSrv.URL})
	key, err := client.CreateTest(tm.NewTest{ProjectKey: "AML", Summary: "New test", Steps: "do the thing"})
	if err != nil {
		t.Fatalf("CreateTest: %v", err)
	}
	if key != "AML-T200" {
		t.Errorf("expected key AML-T200, got %q", key)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	jira, _ := vars["jira"].(map[string]any)
	fields, _ := jira["fields"].(map[string]any)
	if fields["summary"] != "New test" {
		t.Errorf("expected summary New test, got %v", fields["summary"])
	}
}

func TestLinkRequirements(t *testing.T) {
	var gotLinks []map[string]any
	jiraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/issueLink" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "me@example.com" || pass != "tok123" {
			t.Errorf("missing/wrong basic auth: %s/%s", user, pass)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotLinks = append(gotLinks, body)
		w.WriteHeader(201)
	}))
	defer jiraSrv.Close()

	client := xray.New(xray.Config{
		JiraBaseURL:  jiraSrv.URL,
		JiraEmail:    "me@example.com",
		JiraAPIToken: "tok123",
	})
	err := client.LinkRequirements("AML-T142", []string{"AML-1", "AML-2"})
	if err != nil {
		t.Fatalf("LinkRequirements: %v", err)
	}
	if len(gotLinks) != 2 {
		t.Fatalf("expected 2 link requests, got %d", len(gotLinks))
	}
	outward, _ := gotLinks[0]["outwardIssue"].(map[string]any)
	if outward["key"] != "AML-T142" {
		t.Errorf("expected outward AML-T142, got %v", outward["key"])
	}
}

func TestLinkRequirementsMissingConfig(t *testing.T) {
	client := xray.New(xray.Config{})
	if err := client.LinkRequirements("AML-T142", []string{"AML-1"}); err == nil {
		t.Fatal("expected error for missing Jira config")
	}
}

func TestAuthError(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error": "invalid credentials"}`))
	}))
	defer authSrv.Close()

	client := xray.New(xray.Config{
		ClientID:     "bad",
		ClientSecret: "bad",
		AuthURL:      authSrv.URL,
		GQLURL:       "http://unused",
	})

	_, err := client.PushExecution(tm.Execution{})
	if err == nil {
		t.Fatal("expected auth error")
	}
}
