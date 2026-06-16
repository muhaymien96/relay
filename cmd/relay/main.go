// Command relay is a lightweight, local-first API client: send single
// requests, run collections with assertions (JUnit/JSON reports for CI),
// import Postman collections, and export curl commands.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/porter"
	"github.com/muhaymien96/relay/internal/runner"
	"github.com/muhaymien96/relay/internal/store"
	"github.com/muhaymien96/relay/internal/ui"
	"github.com/muhaymien96/relay/internal/vars"
)

var version = "0.2.0"

var nonSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

const usage = `relay — lightweight, local-first API client

Usage:
  relay send <file.req.toml> [--env NAME] [-v] [--insecure] [--timeout 30s]
  relay run  <dir>           [--env NAME] [--report junit|json] [--out FILE]
                             [--data rows.csv|rows.json] [--delay 0ms] [--bail]
                             [--insecure] [--timeout 30s]
  relay import postman <collection.json> [--out DIR]
  relay import curl '<command>'          [--out FILE]   (or pipe via stdin)
  relay import openapi <spec.json>       [--out DIR]
  relay export curl <file.req.toml> [--env NAME]
  relay export postman <dir> [--out collection.json]
  relay export openapi <dir> [--out spec.json]
  relay export k6 <dir> [--env NAME] [--out script.js]
  relay export playwright <dir> [--env NAME] [--out api.spec.ts]
  relay ui [dir] [--db relay.db] [--port 7717]
  relay version

Environments are TOML files at environments/<NAME>.toml, found by walking up
from the request file. Secrets listed in an environment are read from
RELAY_SECRET_<NAME> process environment variables.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "send":
		err = cmdSend(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "ui":
		err = cmdUI(os.Args[2:])
	case "version", "--version":
		fmt.Println("relay", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

// parseInterleaved parses flags that may appear before or after positional
// arguments (stdlib flag stops at the first positional).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func engineFlags(fs *flag.FlagSet) func() engine.Options {
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	noRedirect := fs.Bool("no-redirect", false, "do not follow redirects")
	return func() engine.Options {
		o := engine.NewOptions()
		o.Insecure = *insecure
		o.Timeout = *timeout
		o.FollowRedirects = !*noRedirect
		return o
	}
}

// loadEnv finds environments/<name>.toml walking up from start.
func loadEnv(name, start string) (*dsl.Environment, error) {
	if name == "" {
		return nil, nil
	}
	if filepath.Ext(name) == ".toml" { // direct path
		return dsl.LoadEnvironment(name)
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		p := filepath.Join(dir, "environments", name+".toml")
		if _, err := os.Stat(p); err == nil {
			return dsl.LoadEnvironment(p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("environment %q not found (looked for environments/%s.toml up from %s)", name, name, start)
		}
		dir = parent
	}
}

func resolveFile(file, envName string) (*dsl.Request, *vars.Resolved, error) {
	req, err := dsl.LoadRequest(file)
	if err != nil {
		return nil, nil, err
	}
	env, err := loadEnv(envName, filepath.Dir(file))
	if err != nil {
		return nil, nil, err
	}
	scope := vars.NewScope(req.Vars)
	if err := scope.AddEnvironment(env, os.Getenv); err != nil {
		return nil, nil, err
	}
	resolved, err := vars.Resolve(req, nil, scope)
	if err != nil {
		return nil, nil, err
	}
	return req, resolved, nil
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	envName := fs.String("env", "", "environment name")
	verbose := fs.Bool("v", false, "print request and response headers")
	opts := engineFlags(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay send <file.req.toml>")
	}

	_, resolved, err := resolveFile(pos[0], *envName)
	if err != nil {
		return err
	}
	if *verbose {
		fmt.Printf("> %s %s\n", resolved.Method, resolved.URL)
		for k, v := range resolved.Headers {
			fmt.Printf("> %s: %s\n", k, v[0])
		}
		fmt.Println()
	}

	result, err := engine.Send(context.Background(), resolved, opts())
	if err != nil {
		return err
	}

	fmt.Printf("%s %s  %s  %s\n", resolved.Method, resolved.URL, result.StatusText, result.Timing.Total.Round(time.Millisecond))
	if *verbose {
		for k, v := range result.Headers {
			fmt.Printf("< %s: %s\n", k, v[0])
		}
		t := result.Timing
		fmt.Printf("< timing: dns=%s connect=%s tls=%s ttfb=%s download=%s total=%s\n",
			t.DNS.Round(time.Microsecond), t.Connect.Round(time.Microsecond),
			t.TLS.Round(time.Microsecond), t.TTFB.Round(time.Microsecond),
			t.Download.Round(time.Microsecond), t.Total.Round(time.Microsecond))
	}
	fmt.Println(string(prettyJSON(result.Body)))
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	envName := fs.String("env", "", "environment name")
	report := fs.String("report", "", "report format: junit or json")
	out := fs.String("out", "", "report output file (default stdout)")
	delay := fs.Duration("delay", 0, "delay between requests")
	bail := fs.Bool("bail", false, "stop at first failure")
	dataFile := fs.String("data", "", "CSV or JSON file of variable rows for data-driven runs")
	opts := engineFlags(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay run <dir>")
	}
	root := pos[0]

	env, err := loadEnv(*envName, root)
	if err != nil {
		return err
	}
	data, err := loadData(*dataFile)
	if err != nil {
		return err
	}

	rep, err := runner.Run(context.Background(), root, runner.Options{
		Env:    env,
		Getenv: os.Getenv,
		Delay:  *delay,
		Bail:   *bail,
		Data:   data,
		Engine: opts(),
		OnDone: func(rr runner.RequestResult) {
			mark := "PASS"
			if rr.Failed() {
				mark = "FAIL"
			}
			fmt.Printf("[%s] %-40s %s %s (%d, %s)\n", mark, rr.Name, rr.Method, rr.URL, rr.Status, rr.Duration.Round(time.Millisecond))
			if rr.Err != nil {
				fmt.Printf("       error: %v\n", rr.Err)
			}
			for _, a := range rr.Assertions {
				if !a.Passed {
					fmt.Printf("       assert %s: %s\n", a.Assertion.Type, a.Message)
				}
			}
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n%d requests, %d failed, %s\n", len(rep.Results), rep.Failures(), rep.Duration.Round(time.Millisecond))

	if *report != "" {
		w := os.Stdout
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		switch *report {
		case "junit":
			err = runner.WriteJUnit(w, rep)
		case "json":
			err = runner.WriteJSON(w, rep)
		default:
			err = fmt.Errorf("unknown report format %q (junit|json)", *report)
		}
		if err != nil {
			return err
		}
	}
	if rep.Failures() > 0 {
		os.Exit(1)
	}
	return nil
}

func cmdImport(args []string) error {
	if len(args) >= 1 && args[0] == "curl" {
		return cmdImportCurl(args[1:])
	}
	if len(args) >= 1 && args[0] == "openapi" {
		return cmdImportOpenAPI(args[1:])
	}
	if len(args) < 1 || args[0] != "postman" {
		return fmt.Errorf("usage: relay import postman|curl|openapi ...")
	}
	fs := flag.NewFlagSet("import postman", flag.ExitOnError)
	out := fs.String("out", "", "output directory (default: collection name)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay import postman <collection.json>")
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		var probe struct {
			Info struct {
				Name string `json:"name"`
			} `json:"info"`
		}
		_ = json.Unmarshal(data, &probe)
		if probe.Info.Name == "" {
			return fmt.Errorf("collection has no name; pass --out DIR")
		}
		dir = probe.Info.Name
	}
	n, err := porter.ImportPostman(data, dir)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d requests into %s/\n", n, dir)
	return nil
}

func cmdImportOpenAPI(args []string) error {
	fs := flag.NewFlagSet("import openapi", flag.ExitOnError)
	out := fs.String("out", "", "output directory (default: spec title slug)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay import openapi <spec.json>")
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		var probe struct {
			Info struct{ Title string `json:"title"` } `json:"info"`
		}
		_ = json.Unmarshal(data, &probe)
		if probe.Info.Title != "" {
			dir = nonSlugRun.ReplaceAllString(strings.ToLower(probe.Info.Title), "-")
			dir = strings.Trim(dir, "-")
		}
		if dir == "" {
			dir = "openapi-collection"
		}
	}
	n, err := porter.ImportOpenAPI(data, dir)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d requests into %s/\n", n, dir)
	return nil
}

// cmdImportCurl turns a pasted curl command (argument or stdin) into a
// .req.toml file.
func cmdImportCurl(args []string) error {
	fs := flag.NewFlagSet("import curl", flag.ExitOnError)
	out := fs.String("out", "", "output file (default derived from the URL path)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	command := strings.TrimSpace(strings.Join(pos, " "))
	if command == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		command = strings.TrimSpace(string(data))
	}
	if command == "" {
		return fmt.Errorf("usage: relay import curl '<command>' (or pipe the command via stdin)")
	}
	req, err := porter.ParseCurl(command)
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		slugged := strings.Trim(nonSlugRun.ReplaceAllString(strings.ToLower(req.Name), "-"), "-")
		if slugged == "" {
			slugged = "request"
		}
		path = slugged + ".req.toml"
	}
	if err := os.WriteFile(path, dsl.Marshal(req), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s %s)\n", path, req.Method, req.URL)
	return nil
}

func cmdExport(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: relay export curl|k6|playwright|postman <target>")
	}
	target := args[0]
	fs := flag.NewFlagSet("export "+target, flag.ExitOnError)
	envName := fs.String("env", "", "environment name")
	out := fs.String("out", "", "output file (default stdout)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay export %s <target>", target)
	}

	var script string
	switch target {
	case "curl":
		_, resolved, err := resolveFile(pos[0], *envName)
		if err != nil {
			return err
		}
		script = porter.Curl(resolved) + "\n"
	case "k6", "playwright":
		env, err := loadEnv(*envName, pos[0])
		if err != nil {
			return err
		}
		if target == "k6" {
			script, err = porter.K6(pos[0], env)
		} else {
			script, err = porter.Playwright(pos[0], env)
		}
		if err != nil {
			return err
		}
	case "postman":
		out, err := porter.ExportPostman(pos[0])
		if err != nil {
			return err
		}
		script = string(out)
	case "openapi":
		out, err := porter.ExportOpenAPI(pos[0])
		if err != nil {
			return err
		}
		script = string(out)
	default:
		return fmt.Errorf("unknown export target %q (curl|k6|playwright|postman|openapi)", target)
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(script), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", *out)
		return nil
	}
	fmt.Print(script)
	return nil
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	port := fs.Int("port", 7717, "port to bind on 127.0.0.1 (0 picks a free port)")
	dbPath := fs.String("db", "", "SQLite workspace database (default <dir>/relay.db)")
	opts := engineFlags(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	root := "."
	if len(pos) == 1 {
		root = pos[0]
	} else if len(pos) > 1 {
		return fmt.Errorf("usage: relay ui [dir]")
	}
	db, err := openWorkspaceDB(root, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	srv := &ui.Server{DB: db, Engine: opts()}
	return srv.ListenAndServe(context.Background(), *port)
}

// openWorkspaceDB opens the workspace database (default <dir>/relay.db) and,
// when the database is brand new and the directory holds .req.toml files,
// seeds it from them so existing file-based workspaces open seamlessly.
func openWorkspaceDB(dir, dbPath string) (*store.Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = filepath.Join(abs, "relay.db")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	empty, err := db.Empty()
	if err != nil {
		db.Close()
		return nil, err
	}
	if empty {
		if matches, _ := filepath.Glob(filepath.Join(abs, "*.req.toml")); len(matches) > 0 || hasNestedRequests(abs) {
			if _, err := db.SeedFromDir(abs); err != nil {
				db.Close()
				return nil, fmt.Errorf("seeding from %s: %w", abs, err)
			}
			fmt.Printf("relay: seeded workspace database from %s\n", abs)
		}
	}
	return db, nil
}

func hasNestedRequests(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && path != dir) {
			return filepath.SkipDir
		}
		if strings.HasSuffix(path, ".req.toml") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// loadData reads data-driven rows from a CSV (header row = variable names)
// or a JSON array of objects.
func loadData(path string) ([]map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".json") {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var rows []map[string]any
		if err := dec.Decode(&rows); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out := make([]map[string]string, 0, len(rows))
		for _, r := range rows {
			m := make(map[string]string, len(r))
			for k, v := range r {
				m[k] = fmt.Sprintf("%v", v)
			}
			out = append(out, m)
		}
		return out, nil
	}
	recs, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, fmt.Errorf("%s: need a header row and at least one data row", path)
	}
	header := recs[0]
	out := make([]map[string]string, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		m := make(map[string]string, len(header))
		for i, h := range header {
			if i < len(rec) {
				m[strings.TrimSpace(h)] = rec[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func prettyJSON(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return b
	}
	return out
}
