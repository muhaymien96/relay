package porter

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/vars"
)

const postmanFixture = `{
  "info": {
    "name": "AML Verification",
    "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"
  },
  "variable": [{"key": "baseUrl", "value": "https://sit.example.com"}],
  "item": [
    {
      "name": "Verify",
      "item": [
        {
          "name": "Verify Individual",
          "request": {
            "method": "POST",
            "header": [
              {"key": "Content-Type", "value": "application/json"},
              {"key": "X-Off", "value": "x", "disabled": true}
            ],
            "url": {
              "raw": "{{baseUrl}}/aml/v2/verify",
              "host": ["{{baseUrl}}"],
              "path": ["aml", "v2", "verify"]
            },
            "body": {
              "mode": "raw",
              "raw": "{ \"idNumber\": \"{{testIdNumber}}\" }",
              "options": {"raw": {"language": "json"}}
            },
            "auth": {
              "type": "bearer",
              "bearer": [{"key": "token", "value": "{{token}}"}]
            }
          }
        }
      ]
    },
    {
      "name": "Health",
      "request": {
        "method": "GET",
        "url": "https://sit.example.com/health"
      }
    }
  ]
}`

func TestImportPostman(t *testing.T) {
	out := filepath.Join(t.TempDir(), "aml")
	n, err := ImportPostman([]byte(postmanFixture), out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("imported %d requests", n)
	}

	cfg, err := dsl.LoadConfig(filepath.Join(out, "collection.toml"))
	if err != nil || cfg == nil {
		t.Fatalf("collection.toml: %v", err)
	}
	if cfg.Vars["baseUrl"] != "https://sit.example.com" {
		t.Errorf("collection vars = %v", cfg.Vars)
	}

	req, err := dsl.LoadRequest(filepath.Join(out, "verify", "01-verify-individual.req.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL != "{{baseUrl}}/aml/v2/verify" {
		t.Errorf("req = %+v", req)
	}
	if req.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v", req.Headers)
	}
	if _, ok := req.Headers["X-Off"]; ok {
		t.Error("disabled header should be dropped")
	}
	if req.Body == nil || req.Body.Type != "json" || !strings.Contains(req.Body.Content, "testIdNumber") {
		t.Errorf("body = %+v", req.Body)
	}
	if req.Auth == nil || req.Auth.Type != "bearer" || req.Auth.Token != "{{token}}" {
		t.Errorf("auth = %+v", req.Auth)
	}

	if _, err := dsl.LoadRequest(filepath.Join(out, "02-health.req.toml")); err != nil {
		t.Errorf("string-url request: %v", err)
	}
}

func TestImportPostmanRejectsGarbage(t *testing.T) {
	if _, err := ImportPostman([]byte(`{"foo": 1}`), t.TempDir()); err == nil {
		t.Error("expected error for non-collection JSON")
	}
	if _, err := ImportPostman([]byte(`not json`), t.TempDir()); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestCurl(t *testing.T) {
	r := &vars.Resolved{
		Method: "POST",
		URL:    "https://api.example.com/verify?x=a b",
		Headers: http.Header{
			"Content-Type":  []string{"application/json"},
			"Authorization": []string{"Bearer tok"},
		},
		Body: []byte(`{"id":"42"}`),
	}
	got := Curl(r)
	want := `curl -X POST -H 'Authorization: Bearer tok' -H 'Content-Type: application/json' --data-raw '{"id":"42"}' 'https://api.example.com/verify?x=a b'`
	if got != want {
		t.Errorf("curl:\n got %s\nwant %s", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("quote = %s", got)
	}
	if got := shellQuote("plain"); got != "plain" {
		t.Errorf("quote = %s", got)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
