// Package ui serves the embedded local-first web UI from the relay binary:
// a request builder, collection tree, and response viewer over the same
// engine the CLI uses. It binds to localhost only; the workspace directory
// is the trust boundary for all file operations.
package ui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/runner"
	"github.com/muhaymien96/relay/internal/vars"
)

//go:embed index.html
var indexHTML []byte

// Server hosts the UI for one workspace directory.
type Server struct {
	Root   string
	Engine engine.Options
	Getenv func(string) string
}

// Handler returns the HTTP handler (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/workspace", s.handleWorkspace)
	mux.HandleFunc("GET /api/file", s.handleFileGet)
	mux.HandleFunc("POST /api/file", s.handleFileSave)
	mux.HandleFunc("POST /api/send", s.handleSend)
	return mux
}

// ListenAndServe binds to 127.0.0.1:port (port 0 picks a free one), prints
// the URL, and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	fmt.Printf("relay ui: http://%s  (workspace: %s)\n", ln.Addr(), s.Root)
	srv := &http.Server{Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

type treeEntry struct {
	Path string `json:"path"` // workspace-relative, slash-separated
	Name string `json:"name"`
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	var entries []treeEntry
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != s.Root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".req.toml") {
			rel, err := filepath.Rel(s.Root, path)
			if err != nil {
				return err
			}
			name := d.Name()
			if req, err := dsl.LoadRequest(path); err == nil && req.Name != "" {
				name = req.Name
			}
			entries = append(entries, treeEntry{Path: filepath.ToSlash(rel), Name: name})
		}
		return nil
	})
	if err != nil {
		httpError(w, 500, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var envs []string
	if matches, _ := filepath.Glob(filepath.Join(s.Root, "environments", "*.toml")); matches != nil {
		for _, m := range matches {
			envs = append(envs, strings.TrimSuffix(filepath.Base(m), ".toml"))
		}
	}
	writeJSON(w, map[string]any{
		"root":         s.Root,
		"requests":     entries,
		"environments": envs,
	})
}

// resolvePath confines a workspace-relative request path to the workspace
// root and to .toml files.
func (s *Server) resolvePath(rel string) (string, error) {
	if !strings.HasSuffix(rel, ".toml") {
		return "", fmt.Errorf("only .toml files can be accessed")
	}
	abs := filepath.Join(s.Root, filepath.FromSlash(rel))
	abs = filepath.Clean(abs)
	rootAbs, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	absAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if absAbs != rootAbs && !strings.HasPrefix(absAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return absAbs, nil
}

func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	path, err := s.resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, 400, err)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	writeJSON(w, map[string]string{"content": string(data)})
}

func (s *Server) handleFileSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	path, err := s.resolvePath(in.Path)
	if err != nil {
		httpError(w, 400, err)
		return
	}
	// Validate before writing so a save can't corrupt a request file.
	if strings.HasSuffix(path, ".req.toml") {
		tmp := path + ".relay-validate"
		if err := os.WriteFile(tmp, []byte(in.Content), 0o644); err != nil {
			httpError(w, 500, err)
			return
		}
		_, verr := dsl.LoadRequest(tmp)
		_ = os.Remove(tmp)
		if verr != nil {
			httpError(w, 422, verr)
			return
		}
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
		Env  string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	path, err := s.resolvePath(in.Path)
	if err != nil {
		httpError(w, 400, err)
		return
	}
	req, err := dsl.LoadRequest(path)
	if err != nil {
		httpError(w, 422, err)
		return
	}

	var env *dsl.Environment
	if in.Env != "" {
		env, err = dsl.LoadEnvironment(filepath.Join(s.Root, "environments", in.Env+".toml"))
		if err != nil {
			httpError(w, 422, err)
			return
		}
	}
	getenv := s.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	inherited, scopeVars, err := runner.InheritedConfig(s.Root, path)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	scope := vars.NewScope(req.Vars, scopeVars)
	if err := scope.AddEnvironment(env, getenv); err != nil {
		httpError(w, 422, err)
		return
	}
	resolved, err := vars.Resolve(req, inherited, scope)
	if err != nil {
		httpError(w, 422, err)
		return
	}

	start := time.Now()
	result, err := engine.Send(r.Context(), resolved, s.Engine)
	if err != nil {
		httpError(w, 502, err)
		return
	}
	_ = start

	// Secret-bearing header values are masked before they reach the UI.
	reqHeaders := map[string]string{}
	for k := range resolved.Headers {
		reqHeaders[k] = scope.MaskSecrets(resolved.Headers.Get(k))
	}
	respHeaders := map[string]string{}
	for k := range result.Headers {
		respHeaders[k] = result.Headers.Get(k)
	}

	const uiBodyCap = 2 << 20
	body := result.Body
	truncated := false
	if len(body) > uiBodyCap {
		body = body[:uiBodyCap]
		truncated = true
	}

	writeJSON(w, map[string]any{
		"method":         resolved.Method,
		"url":            scope.MaskSecrets(resolved.URL),
		"requestHeaders": reqHeaders,
		"headerOrigin":   resolved.HeaderOrigin,
		"status":         result.Status,
		"statusText":     result.StatusText,
		"proto":          result.Proto,
		"headers":        respHeaders,
		"body":           string(body),
		"truncated":      truncated,
		"size":           result.Size,
		"timing": map[string]float64{
			"dns":      ms(result.Timing.DNS),
			"connect":  ms(result.Timing.Connect),
			"tls":      ms(result.Timing.TLS),
			"ttfb":     ms(result.Timing.TTFB),
			"download": ms(result.Timing.Download),
			"total":    ms(result.Timing.Total),
		},
	})
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
