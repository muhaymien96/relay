// Package runner executes a collection directory sequentially: every
// *.req.toml under the root, in lexical order, with headers and vars
// inherited from collection.toml/folder.toml files along the path.
package runner

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muhaymien96/relay/internal/assert"
	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/vars"
)

// Options configure a run.
type Options struct {
	Env    *dsl.Environment
	Getenv func(string) string
	Delay  time.Duration // pause between requests
	Bail   bool          // stop at first failure
	// Data makes the run data-driven: the whole collection executes once
	// per row, with the row's values as the highest-precedence variables.
	Data    []map[string]string
	Engine  engine.Options
	OnStart func(name string) // optional progress hooks
	OnDone  func(rr RequestResult)
}

// RequestResult is the outcome of one request in the run.
type RequestResult struct {
	Name       string
	File       string
	Method     string
	URL        string
	Status     int
	Iteration  int // 1-based when the run is data-driven, else 0
	Duration   time.Duration
	Err        error // transport/resolution error; assertions not evaluated
	Assertions []assert.Outcome
}

// Failed reports whether the request errored or any assertion failed.
func (r RequestResult) Failed() bool {
	if r.Err != nil {
		return true
	}
	for _, a := range r.Assertions {
		if !a.Passed {
			return true
		}
	}
	return false
}

// Report is a completed run.
type Report struct {
	Root     string
	Started  time.Time
	Duration time.Duration
	Results  []RequestResult
}

// Failures counts failed requests.
func (r *Report) Failures() int {
	n := 0
	for _, res := range r.Results {
		if res.Failed() {
			n++
		}
	}
	return n
}

// Run executes every request under root.
func Run(ctx context.Context, root string, opts Options) (*Report, error) {
	files, err := Collect(root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.req.toml files under %s", root)
	}

	rows := opts.Data
	if len(rows) == 0 {
		rows = []map[string]string{nil}
	}

	report := &Report{Root: root, Started: time.Now()}
	first := true
loop:
	for iter, row := range rows {
		for _, f := range files {
			if !first && opts.Delay > 0 {
				select {
				case <-time.After(opts.Delay):
				case <-ctx.Done():
					return report, ctx.Err()
				}
			}
			first = false
			rr := runOne(ctx, root, f, row, opts)
			if len(opts.Data) > 0 {
				rr.Iteration = iter + 1
				rr.Name = fmt.Sprintf("%s [%d]", rr.Name, rr.Iteration)
			}
			if opts.OnDone != nil {
				opts.OnDone(rr)
			}
			report.Results = append(report.Results, rr)
			if opts.Bail && rr.Failed() {
				break loop
			}
		}
	}
	report.Duration = time.Since(report.Started)
	return report, nil
}

func runOne(ctx context.Context, root, file string, row map[string]string, opts Options) RequestResult {
	rr := RequestResult{File: file, Name: filepath.Base(file)}
	req, err := dsl.LoadRequest(file)
	if err != nil {
		rr.Err = err
		return rr
	}
	if req.Name != "" {
		rr.Name = req.Name
	}
	rr.Method = req.Method
	if opts.OnStart != nil {
		opts.OnStart(rr.Name)
	}

	headers, scopeVars, err := InheritedConfig(root, file)
	if err != nil {
		rr.Err = err
		return rr
	}
	getenv := opts.Getenv
	scope := vars.NewScope(row, req.Vars, scopeVars)
	if err := scope.AddEnvironment(opts.Env, getenv); err != nil {
		rr.Err = err
		return rr
	}

	resolved, err := vars.Resolve(req, headers, scope)
	if err != nil {
		rr.Err = err
		return rr
	}
	rr.URL = resolved.URL

	start := time.Now()
	result, err := engine.Send(ctx, resolved, opts.Engine)
	rr.Duration = time.Since(start)
	if err != nil {
		rr.Err = err
		return rr
	}
	rr.Status = result.Status
	resolvedAsserts, err := resolveAssertions(req.Assertions, scope)
	if err != nil {
		rr.Err = err
		return rr
	}
	rr.Assertions = assert.Evaluate(resolvedAsserts, result)
	return rr
}

// resolveAssertions interpolates {{variables}} in assertion fields so
// data-driven rows can drive expected values, not just inputs.
func resolveAssertions(in []dsl.Assertion, scope *vars.Scope) ([]dsl.Assertion, error) {
	out := make([]dsl.Assertion, len(in))
	for i, a := range in {
		var err error
		if s, ok := a.Equals.(string); ok {
			if a.Equals, err = scope.Interpolate(s); err != nil {
				return nil, fmt.Errorf("assertion %s: %w", a.Type, err)
			}
		}
		for _, f := range []*string{&a.Path, &a.Name, &a.Contains} {
			if *f == "" {
				continue
			}
			if *f, err = scope.Interpolate(*f); err != nil {
				return nil, fmt.Errorf("assertion %s: %w", a.Type, err)
			}
		}
		out[i] = a
	}
	return out, nil
}

// InheritedConfig merges headers and vars from collection.toml/folder.toml
// files on the path from root down to the request's directory. Deeper files
// win. Exported for the porters, which walk collections the same way.
func InheritedConfig(root, file string) (headers, scopeVars map[string]string, err error) {
	headers = map[string]string{}
	scopeVars = map[string]string{}
	rel, err := filepath.Rel(root, filepath.Dir(file))
	if err != nil {
		return nil, nil, err
	}
	dirs := []string{root}
	if rel != "." {
		cur := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			dirs = append(dirs, cur)
		}
	}
	for _, d := range dirs {
		for _, name := range []string{"collection.toml", "folder.toml"} {
			cfg, err := dsl.LoadConfig(filepath.Join(d, name))
			if err != nil {
				return nil, nil, err
			}
			if cfg == nil {
				continue
			}
			for k, v := range cfg.Headers {
				headers[k] = v
			}
			for k, v := range cfg.Vars {
				scopeVars[k] = v
			}
		}
	}
	return headers, scopeVars, nil
}

// Collect returns every *.req.toml under root in lexical order.
func Collect(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".req.toml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
