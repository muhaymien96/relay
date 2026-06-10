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
	Env     *dsl.Environment
	Getenv  func(string) string
	Delay   time.Duration // pause between requests
	Bail    bool          // stop at first failure
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
	files, err := collectRequestFiles(root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.req.toml files under %s", root)
	}

	report := &Report{Root: root, Started: time.Now()}
	for i, f := range files {
		if i > 0 && opts.Delay > 0 {
			select {
			case <-time.After(opts.Delay):
			case <-ctx.Done():
				return report, ctx.Err()
			}
		}
		rr := runOne(ctx, root, f, opts)
		if opts.OnDone != nil {
			opts.OnDone(rr)
		}
		report.Results = append(report.Results, rr)
		if opts.Bail && rr.Failed() {
			break
		}
	}
	report.Duration = time.Since(report.Started)
	return report, nil
}

func runOne(ctx context.Context, root, file string, opts Options) RequestResult {
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

	headers, scopeVars, err := inherited(root, file)
	if err != nil {
		rr.Err = err
		return rr
	}
	getenv := opts.Getenv
	scope := vars.NewScope(req.Vars, scopeVars)
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
	rr.Assertions = assert.Evaluate(req.Assertions, result)
	return rr
}

// inherited merges headers and vars from collection.toml/folder.toml files
// on the path from root down to the request's directory. Deeper files win.
func inherited(root, file string) (headers, scopeVars map[string]string, err error) {
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

func collectRequestFiles(root string) ([]string, error) {
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
