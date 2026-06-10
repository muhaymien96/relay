package runner

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// WriteJUnit renders the report as JUnit XML for CI systems.
func WriteJUnit(w io.Writer, r *Report) error {
	type failure struct {
		Message string `xml:"message,attr"`
		Body    string `xml:",chardata"`
	}
	type testcase struct {
		Name      string   `xml:"name,attr"`
		Classname string   `xml:"classname,attr"`
		Time      string   `xml:"time,attr"`
		Failure   *failure `xml:"failure,omitempty"`
	}
	type testsuite struct {
		XMLName  xml.Name   `xml:"testsuite"`
		Name     string     `xml:"name,attr"`
		Tests    int        `xml:"tests,attr"`
		Failures int        `xml:"failures,attr"`
		Time     string     `xml:"time,attr"`
		Cases    []testcase `xml:"testcase"`
	}

	suite := testsuite{
		Name:     r.Root,
		Tests:    len(r.Results),
		Failures: r.Failures(),
		Time:     fmt.Sprintf("%.3f", r.Duration.Seconds()),
	}
	for _, res := range r.Results {
		tc := testcase{
			Name:      res.Name,
			Classname: res.File,
			Time:      fmt.Sprintf("%.3f", res.Duration.Seconds()),
		}
		if res.Failed() {
			tc.Failure = &failure{
				Message: failureSummary(res),
				Body:    failureDetail(res),
			}
		}
		suite.Cases = append(suite.Cases, tc)
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(suite); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteJSON renders the report as JSON.
func WriteJSON(w io.Writer, r *Report) error {
	type assertionJSON struct {
		Type    string `json:"type"`
		Passed  bool   `json:"passed"`
		Message string `json:"message"`
	}
	type resultJSON struct {
		Name       string          `json:"name"`
		File       string          `json:"file"`
		Method     string          `json:"method"`
		URL        string          `json:"url"`
		Status     int             `json:"status,omitempty"`
		DurationMs int64           `json:"duration_ms"`
		Error      string          `json:"error,omitempty"`
		Passed     bool            `json:"passed"`
		Assertions []assertionJSON `json:"assertions,omitempty"`
	}
	out := struct {
		Root       string       `json:"root"`
		Started    string       `json:"started"`
		DurationMs int64        `json:"duration_ms"`
		Tests      int          `json:"tests"`
		Failures   int          `json:"failures"`
		Results    []resultJSON `json:"results"`
	}{
		Root:       r.Root,
		Started:    r.Started.UTC().Format("2006-01-02T15:04:05Z"),
		DurationMs: r.Duration.Milliseconds(),
		Tests:      len(r.Results),
		Failures:   r.Failures(),
	}
	for _, res := range r.Results {
		rj := resultJSON{
			Name:       res.Name,
			File:       res.File,
			Method:     res.Method,
			URL:        res.URL,
			Status:     res.Status,
			DurationMs: res.Duration.Milliseconds(),
			Passed:     !res.Failed(),
		}
		if res.Err != nil {
			rj.Error = res.Err.Error()
		}
		for _, a := range res.Assertions {
			rj.Assertions = append(rj.Assertions, assertionJSON{
				Type: a.Assertion.Type, Passed: a.Passed, Message: a.Message,
			})
		}
		out.Results = append(out.Results, rj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func failureSummary(res RequestResult) string {
	if res.Err != nil {
		return res.Err.Error()
	}
	for _, a := range res.Assertions {
		if !a.Passed {
			return a.Message
		}
	}
	return "failed"
}

func failureDetail(res RequestResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", res.Method, res.URL)
	if res.Err != nil {
		fmt.Fprintf(&b, "error: %v\n", res.Err)
	}
	for _, a := range res.Assertions {
		mark := "PASS"
		if !a.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", mark, a.Assertion.Type, a.Message)
	}
	return b.String()
}
