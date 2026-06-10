// Package assert evaluates the [[assertions]] blocks of a request against a
// response. The jsonpath support is a deliberate subset: dot fields and
// numeric indexes ($.result.items[0].status), which covers the common case
// without a dependency.
package assert

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
)

// Outcome is one evaluated assertion.
type Outcome struct {
	Assertion dsl.Assertion
	Passed    bool
	Message   string // human-readable failure (or pass) description
}

// Evaluate runs every assertion against the result.
func Evaluate(assertions []dsl.Assertion, res *engine.Result) []Outcome {
	out := make([]Outcome, 0, len(assertions))
	for _, a := range assertions {
		o := Outcome{Assertion: a}
		o.Passed, o.Message = evalOne(a, res)
		out = append(out, o)
	}
	return out
}

func evalOne(a dsl.Assertion, res *engine.Result) (bool, string) {
	switch a.Type {
	case "status":
		want, ok := toInt(a.Equals)
		if !ok {
			return false, fmt.Sprintf("status assertion needs an integer `equals`, got %v", a.Equals)
		}
		if int64(res.Status) == want {
			return true, fmt.Sprintf("status is %d", res.Status)
		}
		return false, fmt.Sprintf("expected status %d, got %d", want, res.Status)

	case "jsonpath":
		var doc any
		if err := json.Unmarshal(res.Body, &doc); err != nil {
			return false, fmt.Sprintf("response is not valid JSON: %v", err)
		}
		val, err := jsonPath(doc, a.Path)
		if err != nil {
			return false, err.Error()
		}
		if equal(val, a.Equals) {
			return true, fmt.Sprintf("%s == %v", a.Path, a.Equals)
		}
		return false, fmt.Sprintf("%s: expected %v, got %v", a.Path, a.Equals, val)

	case "header":
		got := res.Headers.Get(a.Name)
		want, _ := a.Equals.(string)
		if a.Contains != "" {
			if strings.Contains(got, a.Contains) {
				return true, fmt.Sprintf("header %s contains %q", a.Name, a.Contains)
			}
			return false, fmt.Sprintf("header %s = %q does not contain %q", a.Name, got, a.Contains)
		}
		if got == want {
			return true, fmt.Sprintf("header %s == %q", a.Name, want)
		}
		return false, fmt.Sprintf("header %s: expected %q, got %q", a.Name, want, got)

	case "contains":
		if strings.Contains(string(res.Body), a.Contains) {
			return true, fmt.Sprintf("body contains %q", a.Contains)
		}
		return false, fmt.Sprintf("body does not contain %q", a.Contains)

	case "max_ms":
		ms := res.Timing.Total.Milliseconds()
		if ms <= a.MaxMs {
			return true, fmt.Sprintf("took %dms (limit %dms)", ms, a.MaxMs)
		}
		return false, fmt.Sprintf("took %dms, limit %dms", ms, a.MaxMs)

	default:
		return false, fmt.Sprintf("unknown assertion type %q", a.Type)
	}
}

// jsonPath walks a subset of JSONPath: `$`, `.field`, `["field"]`, `[N]`.
func jsonPath(doc any, path string) (any, error) {
	p := strings.TrimSpace(path)
	if !strings.HasPrefix(p, "$") {
		return nil, fmt.Errorf("jsonpath must start with $: %q", path)
	}
	p = p[1:]
	cur := doc
	for len(p) > 0 {
		switch {
		case strings.HasPrefix(p, "."):
			p = p[1:]
			end := strings.IndexAny(p, ".[")
			if end == -1 {
				end = len(p)
			}
			field := p[:end]
			p = p[end:]
			if field == "" {
				return nil, fmt.Errorf("empty field in jsonpath %q", path)
			}
			cur2, err := getField(cur, field, path)
			if err != nil {
				return nil, err
			}
			cur = cur2
		case strings.HasPrefix(p, "["):
			end := strings.Index(p, "]")
			if end == -1 {
				return nil, fmt.Errorf("unclosed [ in jsonpath %q", path)
			}
			idx := p[1:end]
			p = p[end+1:]
			if len(idx) >= 2 && (idx[0] == '"' || idx[0] == '\'') {
				cur2, err := getField(cur, idx[1:len(idx)-1], path)
				if err != nil {
					return nil, err
				}
				cur = cur2
				continue
			}
			n, err := strconv.Atoi(idx)
			if err != nil {
				return nil, fmt.Errorf("bad index %q in jsonpath %q", idx, path)
			}
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: not an array at [%d]", path, n)
			}
			if n < 0 || n >= len(arr) {
				return nil, fmt.Errorf("%s: index %d out of range (len %d)", path, n, len(arr))
			}
			cur = arr[n]
		default:
			return nil, fmt.Errorf("unexpected %q in jsonpath %q", p[:1], path)
		}
	}
	return cur, nil
}

func getField(cur any, field, path string) (any, error) {
	obj, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not an object at %q", path, field)
	}
	v, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("%s: field %q not found", path, field)
	}
	return v, nil
}

// equal compares a JSON value with a TOML literal, normalizing numbers
// (JSON decodes to float64, TOML to int64).
func equal(got, want any) bool {
	if gn, ok := toFloat(got); ok {
		if wn, ok := toFloat(want); ok {
			return gn == wn
		}
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}
