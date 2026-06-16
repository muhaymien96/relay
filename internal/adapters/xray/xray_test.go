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
