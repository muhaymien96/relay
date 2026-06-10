package assert

import (
	"net/http"
	"testing"
	"time"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
)

func result() *engine.Result {
	return &engine.Result{
		Status:  200,
		Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:    []byte(`{"result":{"status":"VERIFIED","score":98.5,"items":[{"id":1},{"id":2}]},"ok":true}`),
		Timing:  engine.Timing{Total: 40 * time.Millisecond},
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		a    dsl.Assertion
		pass bool
	}{
		{dsl.Assertion{Type: "status", Equals: int64(200)}, true},
		{dsl.Assertion{Type: "status", Equals: int64(201)}, false},
		{dsl.Assertion{Type: "jsonpath", Path: "$.result.status", Equals: "VERIFIED"}, true},
		{dsl.Assertion{Type: "jsonpath", Path: "$.result.status", Equals: "REJECTED"}, false},
		{dsl.Assertion{Type: "jsonpath", Path: "$.result.score", Equals: 98.5}, true},
		{dsl.Assertion{Type: "jsonpath", Path: "$.result.items[1].id", Equals: int64(2)}, true},
		{dsl.Assertion{Type: "jsonpath", Path: `$["result"]["status"]`, Equals: "VERIFIED"}, true},
		{dsl.Assertion{Type: "jsonpath", Path: "$.ok", Equals: true}, true},
		{dsl.Assertion{Type: "jsonpath", Path: "$.missing", Equals: "x"}, false},
		{dsl.Assertion{Type: "jsonpath", Path: "$.result.items[9]", Equals: "x"}, false},
		{dsl.Assertion{Type: "header", Name: "Content-Type", Contains: "json"}, true},
		{dsl.Assertion{Type: "header", Name: "Content-Type", Equals: "text/html"}, false},
		{dsl.Assertion{Type: "contains", Contains: "VERIFIED"}, true},
		{dsl.Assertion{Type: "contains", Contains: "nope"}, false},
		{dsl.Assertion{Type: "max_ms", MaxMs: 100}, true},
		{dsl.Assertion{Type: "max_ms", MaxMs: 10}, false},
		{dsl.Assertion{Type: "bogus"}, false},
	}
	for _, c := range cases {
		out := Evaluate([]dsl.Assertion{c.a}, result())
		if out[0].Passed != c.pass {
			t.Errorf("%+v: passed=%v (%s), want %v", c.a, out[0].Passed, out[0].Message, c.pass)
		}
	}
}

func TestJSONPathOnNonJSON(t *testing.T) {
	res := &engine.Result{Status: 200, Body: []byte("<html>")}
	out := Evaluate([]dsl.Assertion{{Type: "jsonpath", Path: "$.x", Equals: "y"}}, res)
	if out[0].Passed {
		t.Error("jsonpath on non-JSON must fail, not pass")
	}
}
