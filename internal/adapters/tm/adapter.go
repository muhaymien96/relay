// Package tm defines the test-management adapter interface and the shared
// result types that adapters accept. Concrete implementations (Xray, Zephyr,
// TestRail) live in sibling packages and register themselves here.
package tm

import "time"

// Status values for a test result.
const (
	StatusPASS = "PASSED"
	StatusFAIL = "FAILED"
	StatusSKIP = "SKIPPED"
)

// TestResult is one request's outcome in a normalised form suitable for any
// test-management system.
type TestResult struct {
	TestKey    string // e.g. "AML-T142" from meta.xray.test_key (optional)
	Name       string // human name shown in the execution
	Status     string // StatusPASS | StatusFAIL | StatusSKIP
	Comment    string // failure details / assertion messages
	DurationMs float64
	Steps      []TestStep
}

type TestStep struct {
	Name     string
	Type     string
	Expected string
	Actual   string
	Status   string
	Comment  string
}

// Execution is a completed collection run ready to be pushed.
type Execution struct {
	ProjectKey  string // Jira project key, e.g. "AML"
	TestPlanKey string // optional existing test-plan issue key
	Summary     string // test execution summary line
	StartedAt   time.Time
	FinishedAt  time.Time
	Results     []TestResult
}

// Adapter is the generic interface for pushing executions to a TM system.
type Adapter interface {
	// PushExecution creates a new test execution and returns the issue key.
	PushExecution(exec Execution) (executionKey string, err error)
}
