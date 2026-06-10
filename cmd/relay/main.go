// Command relay is a lightweight, local-first API client: send single
// requests, run collections with assertions (JUnit/JSON reports for CI),
// import Postman collections, and export curl commands.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/porter"
	"github.com/muhaymien96/relay/internal/runner"
	"github.com/muhaymien96/relay/internal/vars"
)

var version = "0.1.0-dev"

const usage = `relay — lightweight, local-first API client

Usage:
  relay send <file.req.toml> [--env NAME] [-v] [--insecure] [--timeout 30s]
  relay run  <dir>           [--env NAME] [--report junit|json] [--out FILE]
                             [--delay 0ms] [--bail] [--insecure] [--timeout 30s]
  relay import postman <collection.json> [--out DIR]
  relay export curl <file.req.toml> [--env NAME]
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

	rep, err := runner.Run(context.Background(), root, runner.Options{
		Env:    env,
		Getenv: os.Getenv,
		Delay:  *delay,
		Bail:   *bail,
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
	if len(args) < 1 || args[0] != "postman" {
		return fmt.Errorf("usage: relay import postman <collection.json> [--out DIR]")
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

func cmdExport(args []string) error {
	if len(args) < 1 || args[0] != "curl" {
		return fmt.Errorf("usage: relay export curl <file.req.toml> [--env NAME]")
	}
	fs := flag.NewFlagSet("export curl", flag.ExitOnError)
	envName := fs.String("env", "", "environment name")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: relay export curl <file.req.toml>")
	}
	_, resolved, err := resolveFile(pos[0], *envName)
	if err != nil {
		return err
	}
	fmt.Println(porter.Curl(resolved))
	return nil
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
