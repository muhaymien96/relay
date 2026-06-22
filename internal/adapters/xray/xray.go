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
}

type Issue struct {
	IssueID string `json:"issueId,omitempty"`
	Key     string `json:"key,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type TestInput struct {
	ProjectKey   string
	Summary      string
	Description  string
	Labels       []string
	Components   []string
	Priority     string
	TestType     string
	Steps        []tm.TestStep
	Requirements []string
	TestPlanKey  string
	TestSetKey   string
}

// ConfigFromEnv reads credentials from standard env vars.
func ConfigFromEnv() Config {
	return Config{
		ClientID:     os.Getenv("RELAY_XRAY_CLIENT_ID"),
		ClientSecret: os.Getenv("RELAY_XRAY_CLIENT_SECRET"),
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
	_, err := c.doGraphQL(`query RelayConnectionCheck { getTests(jql: "issueType = Test", limit: 1) { total } }`, map[string]any{})
	return err
}

func (c *Client) GetTest(key string) (*Issue, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("test key required")
	}
	body, err := c.doGraphQL(`query GetTest($jql: String!) { getTests(jql: $jql, limit: 1) { results { issueId jira(fields: ["key", "summary"]) } } }`,
		map[string]any{"jql": "key = " + key})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			GetTests struct {
				Results []struct {
					IssueID string         `json:"issueId"`
					Jira    map[string]any `json:"jira"`
				} `json:"results"`
			} `json:"getTests"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("xray: bad get test response: %w", err)
	}
	if len(out.Data.GetTests.Results) == 0 {
		return nil, fmt.Errorf("xray test %s not found", key)
	}
	r := out.Data.GetTests.Results[0]
	issue := &Issue{IssueID: r.IssueID}
	if v, ok := r.Jira["key"].(string); ok {
		issue.Key = v
	}
	if v, ok := r.Jira["summary"].(string); ok {
		issue.Summary = v
	}
	return issue, nil
}

func (c *Client) CreateTest(in TestInput) (*Issue, error) {
	if strings.TrimSpace(in.ProjectKey) == "" {
		return nil, fmt.Errorf("project key required")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return nil, fmt.Errorf("test summary required")
	}
	testType := in.TestType
	if testType == "" {
		testType = "Manual"
	}
	issue := map[string]any{
		"projectKey":  in.ProjectKey,
		"summary":     in.Summary,
		"description": in.Description,
		"testType":    map[string]any{"name": testType},
	}
	if len(in.Labels) > 0 {
		issue["labels"] = in.Labels
	}
	steps := make([]map[string]any, 0, len(in.Steps))
	for i, st := range in.Steps {
		action := st.Name
		if action == "" {
			action = fmt.Sprintf("Step %d", i+1)
		}
		steps = append(steps, map[string]any{
			"action": action,
			"data":   strings.TrimSpace(st.Type + " " + st.Actual),
			"result": st.Expected,
		})
	}
	vars := map[string]any{"testIssue": issue}
	if len(steps) > 0 {
		vars["steps"] = steps
	}
	body, err := c.doGraphQL(`mutation CreateTest($testIssue: TestIssueInput!, $steps: [CreateStepInput!]) { createTest(testIssue: $testIssue, steps: $steps) { test { issueId jira(fields: ["key", "summary"]) } warnings } }`, vars)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			CreateTest struct {
				Test struct {
					IssueID string         `json:"issueId"`
					Jira    map[string]any `json:"jira"`
				} `json:"test"`
			} `json:"createTest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("xray: bad create test response: %w", err)
	}
	created := out.Data.CreateTest.Test
	issueOut := &Issue{IssueID: created.IssueID}
	if v, ok := created.Jira["key"].(string); ok {
		issueOut.Key = v
	}
	if v, ok := created.Jira["summary"].(string); ok {
		issueOut.Summary = v
	}
	if issueOut.Key == "" {
		return nil, fmt.Errorf("xray create test returned no key")
	}
	return issueOut, nil
}

func (c *Client) LinkRequirements(testKey string, requirementKeys []string) error {
	if testKey == "" || len(requirementKeys) == 0 {
		return nil
	}
	_, err := c.doGraphQL(`mutation LinkRequirements($testKey: String!, $requirements: [String!]!) { addRequirementsToTestIssue(testIssueKey: $testKey, requirementIssueKeys: $requirements) { addedRequirements warnings } }`,
		map[string]any{"testKey": testKey, "requirements": requirementKeys})
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
	body, err := c.doGraphQL(`mutation CreateTestSet($testSetIssue: TestSetIssueInput!, $tests: [String!]) { createTestSet(testSetIssue: $testSetIssue, tests: $tests) { testSet { issueId jira(fields: ["key", "summary"]) } warnings } }`, vars)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			CreateTestSet struct {
				TestSet struct {
					IssueID string         `json:"issueId"`
					Jira    map[string]any `json:"jira"`
				} `json:"testSet"`
			} `json:"createTestSet"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("xray: bad create test set response: %w", err)
	}
	created := out.Data.CreateTestSet.TestSet
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
	_, err := c.doGraphQL(`mutation AddTestsToPlan($testPlanKey: String!, $tests: [String!]!) { addTestsToTestPlan(testPlanIssueKey: $testPlanKey, testIssueKeys: $tests) { addedTests warnings } }`,
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
	if r.Comment != "" {
		parts = append(parts, r.Comment)
	}
	for _, st := range r.Steps {
		status := st.Status
		if status == "" {
			status = tm.StatusPASS
		}
		line := fmt.Sprintf("[%s] %s", status, st.Name)
		if st.Comment != "" {
			line += ": " + st.Comment
		}
		if st.Expected != "" {
			line += " (expected: " + st.Expected + ")"
		}
		if st.Actual != "" {
			line += " (actual: " + st.Actual + ")"
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

func (c *Client) doGraphQL(query string, vars map[string]any) ([]byte, error) {
	token, err := c.authenticate()
	if err != nil {
		return nil, fmt.Errorf("xray auth: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": vars,
	})
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
	var gqlResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, fmt.Errorf("xray: bad response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("xray gql errors: %s", strings.Join(msgs, "; "))
	}
	return body, nil
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
