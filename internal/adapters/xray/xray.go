// Package xray pushes test executions to Xray Cloud (Jira Cloud) via its
// GraphQL API. Credentials are read from RELAY_XRAY_CLIENT_ID and
// RELAY_XRAY_CLIENT_SECRET environment variables.
//
// Xray Cloud GraphQL endpoint: https://xray.cloud.getxray.app/api/v2/graphql
// Auth endpoint:                https://xray.cloud.getxray.app/api/v2/authenticate
package xray

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/muhaymien96/relay/internal/adapters/tm"
)

const (
	defaultAuthURL = "https://xray.cloud.getxray.app/api/v2/authenticate"
	defaultGQLURL  = "https://xray.cloud.getxray.app/api/v2/graphql"
	tokenCacheTTL  = 55 * time.Minute
)

// Config holds the Xray Cloud connection parameters.
type Config struct {
	ClientID     string // from RELAY_XRAY_CLIENT_ID
	ClientSecret string // from RELAY_XRAY_CLIENT_SECRET
	AuthURL      string // override for testing
	GQLURL       string // override for testing

	// Jira REST settings, used for requirement linking (Xray's GraphQL API
	// has no issue-link mutation; that's plain Jira). JiraBaseURL/JiraEmail
	// are not secret and may come from store settings; JiraAPIToken is a
	// secret and is only ever read from the environment.
	JiraBaseURL  string // e.g. "https://yourorg.atlassian.net"
	JiraEmail    string
	JiraAPIToken string // from RELAY_JIRA_API_TOKEN
}

type Issue struct {
	IssueID string `json:"issueId,omitempty"`
	Key     string `json:"key,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ConfigFromEnv reads credentials from standard env vars.
func ConfigFromEnv() Config {
	return Config{
		ClientID:     os.Getenv("RELAY_XRAY_CLIENT_ID"),
		ClientSecret: os.Getenv("RELAY_XRAY_CLIENT_SECRET"),
		JiraBaseURL:  os.Getenv("RELAY_JIRA_BASE_URL"),
		JiraEmail:    os.Getenv("RELAY_JIRA_EMAIL"),
		JiraAPIToken: os.Getenv("RELAY_JIRA_API_TOKEN"),
	}
}

// Client is an Xray Cloud adapter implementing tm.Adapter.
type Client struct {
	cfg        Config
	httpClient *http.Client
	token      string
	tokenExp   time.Time
}

// New creates a new Xray Cloud client.
func New(cfg Config) *Client {
	if cfg.AuthURL == "" {
		cfg.AuthURL = defaultAuthURL
	}
	if cfg.GQLURL == "" {
		cfg.GQLURL = defaultGQLURL
	}
	return &Client{cfg: cfg, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) TestConnection() error {
	_, err := c.doGQL(`query RelayConnectionCheck { getTests(jql: "issueType = Test", limit: 1) { total } }`, map[string]any{})
	return err
}

func (c *Client) CreateTestSet(projectKey, summary string, testKeys []string) (*Issue, error) {
	if projectKey == "" {
		return nil, fmt.Errorf("project key required")
	}
	if summary == "" {
		return nil, fmt.Errorf("test set summary required")
	}
	vars := map[string]any{
		"testSetIssue": map[string]any{"projectKey": projectKey, "summary": summary},
		"tests":        testKeys,
	}
	body, err := c.doGQL(`mutation CreateTestSet($testSetIssue: TestSetIssueInput!, $tests: [String!]) { createTestSet(testSetIssue: $testSetIssue, tests: $tests) { testSet { issueId jira(fields: ["key", "summary"]) } warnings } }`, vars)
	if err != nil {
		return nil, err
	}
	var out struct {
		CreateTestSet struct {
			TestSet struct {
				IssueID string         `json:"issueId"`
				Jira    map[string]any `json:"jira"`
			} `json:"testSet"`
		} `json:"createTestSet"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("xray: bad create test set response: %w", err)
	}
	created := out.CreateTestSet.TestSet
	issueOut := &Issue{IssueID: created.IssueID}
	if v, ok := created.Jira["key"].(string); ok {
		issueOut.Key = v
	}
	if v, ok := created.Jira["summary"].(string); ok {
		issueOut.Summary = v
	}
	if issueOut.Key == "" {
		return nil, fmt.Errorf("xray create test set returned no key")
	}
	return issueOut, nil
}

func (c *Client) AddTestsToTestPlan(testPlanKey string, testKeys []string) error {
	if testPlanKey == "" || len(testKeys) == 0 {
		return nil
	}
	_, err := c.doGQL(`mutation AddTestsToPlan($testPlanKey: String!, $tests: [String!]!) { addTestsToTestPlan(testPlanIssueKey: $testPlanKey, testIssueKeys: $tests) { addedTests warnings } }`,
		map[string]any{"testPlanKey": testPlanKey, "tests": testKeys})
	return err
}

// PushExecution creates a new Xray test execution from the normalised run data
// and returns the resulting Jira issue key.
func (c *Client) PushExecution(exec tm.Execution) (string, error) {
	token, err := c.authenticate()
	if err != nil {
		return "", fmt.Errorf("xray auth: %w", err)
	}

	results := make([]map[string]any, 0, len(exec.Results))
	for _, r := range exec.Results {
		entry := map[string]any{
			"testKey": r.TestKey,
			"status":  r.Status,
			"comment": stepComment(r),
		}
		if r.TestKey == "" {
			// Tests without an Xray key are identified by name only —
			// Xray will create generic test issues when the project has
			// the "auto-provision" setting enabled.
			entry["summary"] = r.Name
			delete(entry, "testKey")
		}
		results = append(results, entry)
	}

	mutation := `
	mutation CreateTestExecution($testExecIssue: TestExecIssueInput, $tests: [TestExecTestInput!]) {
		createTestExecution(testExecIssue: $testExecIssue, tests: $tests) {
			testExecution { issueId jira(fields: ["key"]) }
			createdTests { issueId }
			warnings
		}
	}`

	vars := map[string]any{
		"testExecIssue": map[string]any{
			"projectKey": exec.ProjectKey,
			"summary":    exec.Summary,
			"startDate":  exec.StartedAt.UTC().Format(time.RFC3339),
			"finishDate": exec.FinishedAt.UTC().Format(time.RFC3339),
		},
		"tests": results,
	}
	if exec.TestPlanKey != "" {
		te := vars["testExecIssue"].(map[string]any)
		te["testPlanKey"] = exec.TestPlanKey
	}

	payload, err := json.Marshal(map[string]any{
		"query":     mutation,
		"variables": vars,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", c.cfg.GQLURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("xray gql: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("xray gql: HTTP %d: %s", resp.StatusCode, body)
	}

	var gqlResp struct {
		Data struct {
			CreateTestExecution struct {
				TestExecution struct {
					Jira map[string]any `json:"jira"`
				} `json:"testExecution"`
				Warnings []string `json:"warnings"`
			} `json:"createTestExecution"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return "", fmt.Errorf("xray: bad response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return "", fmt.Errorf("xray gql errors: %s", strings.Join(msgs, "; "))
	}

	key := ""
	if j := gqlResp.Data.CreateTestExecution.TestExecution.Jira; j != nil {
		if k, ok := j["key"].(string); ok {
			key = k
		}
	}
	return key, nil
}

func stepComment(r tm.TestResult) string {
	var parts []string
	if strings.TrimSpace(r.Comment) != "" {
		parts = append(parts, strings.TrimSpace(r.Comment))
	}
	for _, st := range r.Steps {
		line := strings.TrimSpace(st.Name)
		if line == "" {
			line = strings.TrimSpace(st.Type)
		}
		if line == "" {
			line = "step"
		}
		if st.Status != "" {
			line += ": " + st.Status
		}
		if st.Comment != "" {
			line += " - " + st.Comment
		}
		if st.Expected != "" {
			line += " (expected: " + st.Expected + ")"
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// doGQL authenticates, posts a GraphQL request, and returns the raw "data"
// payload (or an error built from transport failures or the "errors" array).
func (c *Client) doGQL(query string, vars map[string]any) (json.RawMessage, error) {
	token, err := c.authenticate()
	if err != nil {
		return nil, fmt.Errorf("xray auth: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.cfg.GQLURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xray gql: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("xray gql: HTTP %d: %s", resp.StatusCode, body)
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("xray: bad response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("xray gql errors: %s", strings.Join(msgs, "; "))
	}
	return env.Data, nil
}

// GetTest looks up an existing Xray test issue by Jira key.
func (c *Client) GetTest(key string) (*tm.TestRef, error) {
	query := `
	query GetTests($jql: String!) {
		getTests(jql: $jql, limit: 1) {
			results { issueId jira(fields: ["key", "summary"]) }
		}
	}`
	data, err := c.doGQL(query, map[string]any{"jql": fmt.Sprintf("key = %q", key)})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		GetTests struct {
			Results []struct {
				IssueID string         `json:"issueId"`
				Jira    map[string]any `json:"jira"`
			} `json:"results"`
		} `json:"getTests"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("xray: bad getTests response: %w", err)
	}
	if len(parsed.GetTests.Results) == 0 {
		return nil, nil
	}
	j := parsed.GetTests.Results[0].Jira
	ref := &tm.TestRef{IssueID: parsed.GetTests.Results[0].IssueID}
	if k, ok := j["key"].(string); ok {
		ref.Key = k
	}
	if s, ok := j["summary"].(string); ok {
		ref.Summary = s
	}
	return ref, nil
}

func (c *Client) UpdateTest(ref tm.TestRef, t tm.NewTest) error {
	if ref.IssueID == "" {
		return fmt.Errorf("xray: missing issueId for %s", ref.Key)
	}
	testType := t.TestType
	if testType == "" {
		testType = "Generic"
	}
	_, err := c.doGQL(`
	mutation UpdateTest($issueId: String!, $testType: UpdateTestTypeInput, $unstructured: String, $jira: JSON!) {
		updateTest(issueId: $issueId, testType: $testType, unstructured: $unstructured, jira: $jira) {
			test { issueId jira(fields: ["key"]) }
			warnings
		}
	}`, map[string]any{
		"issueId":      ref.IssueID,
		"testType":     map[string]any{"name": testType},
		"unstructured": t.Steps,
		"jira": map[string]any{"fields": map[string]any{
			"summary": t.Summary,
		}},
	})
	return err
}

// CreateTest creates a new Xray test issue and returns its Jira key.
func (c *Client) CreateTest(t tm.NewTest) (string, error) {
	testType := t.TestType
	if testType == "" {
		testType = "Generic"
	}
	mutation := `
	mutation CreateTest($testType: UpdateTestTypeInput, $unstructured: String, $jira: JSON!) {
		createTest(testType: $testType, unstructured: $unstructured, jira: $jira) {
			test { issueId jira(fields: ["key"]) }
			warnings
		}
	}`
	vars := map[string]any{
		"testType":     map[string]any{"name": testType},
		"unstructured": t.Steps,
		"jira": map[string]any{
			"fields": map[string]any{
				"summary": t.Summary,
				"project": map[string]any{"key": t.ProjectKey},
			},
		},
	}
	data, err := c.doGQL(mutation, vars)
	if err != nil {
		return "", err
	}
	var parsed struct {
		CreateTest struct {
			Test struct {
				Jira map[string]any `json:"jira"`
			} `json:"test"`
			Warnings []string `json:"warnings"`
		} `json:"createTest"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("xray: bad createTest response: %w", err)
	}
	key, _ := parsed.CreateTest.Test.Jira["key"].(string)
	if key == "" {
		return "", fmt.Errorf("xray: createTest did not return a key")
	}
	return key, nil
}

// LinkRequirements links a test issue to one or more requirement issues
// using Jira's "Tests" issue link type (the test "tests" the requirement;
// the requirement "is tested by" the test). This is a plain Jira REST call
// — Xray Cloud's GraphQL API has no issue-link mutation — so it needs Jira
// basic-auth credentials (email + API token) rather than the Xray client
// id/secret used for GraphQL.
func (c *Client) LinkRequirements(testKey string, requirementKeys []string) error {
	if c.cfg.JiraBaseURL == "" {
		return fmt.Errorf("xray: Jira base URL not configured")
	}
	if c.cfg.JiraEmail == "" || c.cfg.JiraAPIToken == "" {
		return fmt.Errorf("RELAY_JIRA_EMAIL and RELAY_JIRA_API_TOKEN must be set")
	}
	url := strings.TrimRight(c.cfg.JiraBaseURL, "/") + "/rest/api/2/issueLink"
	for _, reqKey := range requirementKeys {
		if reqKey == "" {
			continue
		}
		body, err := json.Marshal(map[string]any{
			"type":         map[string]string{"name": "Tests"},
			"inwardIssue":  map[string]string{"key": reqKey},
			"outwardIssue": map[string]string{"key": testKey},
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.cfg.JiraEmail, c.cfg.JiraAPIToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("jira issueLink %s: %w", reqKey, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			return fmt.Errorf("jira issueLink %s: HTTP %d: %s", reqKey, resp.StatusCode, respBody)
		}
	}
	return nil
}

// authenticate returns a cached bearer token, refreshing if needed.
func (c *Client) authenticate() (string, error) {
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	if c.cfg.ClientID == "" || c.cfg.ClientSecret == "" {
		return "", fmt.Errorf("RELAY_XRAY_CLIENT_ID and RELAY_XRAY_CLIENT_SECRET must be set")
	}
	payload, _ := json.Marshal(map[string]string{
		"client_id":     c.cfg.ClientID,
		"client_secret": c.cfg.ClientSecret,
	})
	resp, err := c.httpClient.Post(c.cfg.AuthURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	// Response is a bare quoted string: "eyJ..."
	token := strings.Trim(strings.TrimSpace(string(body)), `"`)
	if token == "" {
		return "", fmt.Errorf("empty token from Xray auth endpoint")
	}
	c.token = token
	c.tokenExp = time.Now().Add(tokenCacheTTL)
	return token, nil
}

// BuildExecution converts a runner.Report-like structure to tm.Execution.
// Import this from the xray package rather than importing runner (avoids
// a circular dependency: runner → xray → runner).
type RunnerReport interface {
	GetResults() []RunnerResult
	GetStarted() time.Time
	GetFinished() time.Time
}

// RunnerResult is the per-request data the adapter needs.
type RunnerResult interface {
	GetName() string
	GetTestKey() string // meta.xray.test_key if set, else ""
	IsFailed() bool
	GetComment() string
	GetDurationMs() float64
}

// FromRunnerReport converts a RunnerReport into a tm.Execution.
func FromRunnerReport(report RunnerReport, projectKey, testPlanKey, summary string) tm.Execution {
	exec := tm.Execution{
		ProjectKey:  projectKey,
		TestPlanKey: testPlanKey,
		Summary:     summary,
		StartedAt:   report.GetStarted(),
		FinishedAt:  report.GetFinished(),
	}
	for _, r := range report.GetResults() {
		status := tm.StatusPASS
		if r.IsFailed() {
			status = tm.StatusFAIL
		}
		exec.Results = append(exec.Results, tm.TestResult{
			TestKey:    r.GetTestKey(),
			Name:       r.GetName(),
			Status:     status,
			Comment:    r.GetComment(),
			DurationMs: r.GetDurationMs(),
		})
	}
	return exec
}
