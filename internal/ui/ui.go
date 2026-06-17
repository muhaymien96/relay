// Package ui serves the Relay workbench (embedded single-page app) from the
// relay binary: collections, request builder, runner, history, environments
// and header presets, all backed by the SQLite store. It binds to localhost
// only. The same handler powers `relay ui` (browser) and relay-app (Wails
// desktop window).
package ui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muhaymien96/relay/internal/adapters/tm"
	"github.com/muhaymien96/relay/internal/adapters/xray"
	"github.com/muhaymien96/relay/internal/assert"
	"github.com/muhaymien96/relay/internal/dsl"
	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/porter"
	"github.com/muhaymien96/relay/internal/runner"
	"github.com/muhaymien96/relay/internal/script"
	"github.com/muhaymien96/relay/internal/store"
	"github.com/muhaymien96/relay/internal/vars"
)

//go:embed index.html
var indexHTML []byte

// Server hosts the workbench for one store.
type Server struct {
	DB     *store.Store
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
	mux.HandleFunc("GET /api/state", s.handleState)

	mux.HandleFunc("POST /api/collections", s.handleCollectionCreate)
	mux.HandleFunc("PATCH /api/collections/{id}", s.handleCollectionUpdate)
	mux.HandleFunc("DELETE /api/collections/{id}", s.handleCollectionDelete)

	mux.HandleFunc("POST /api/folders", s.handleFolderCreate)
	mux.HandleFunc("PATCH /api/folders/{id}", s.handleFolderUpdate)
	mux.HandleFunc("DELETE /api/folders/{id}", s.handleFolderDelete)

	mux.HandleFunc("POST /api/requests", s.handleRequestCreate)
	mux.HandleFunc("GET /api/requests/{id}", s.handleRequestGet)
	mux.HandleFunc("PUT /api/requests/{id}", s.handleRequestUpdate)
	mux.HandleFunc("DELETE /api/requests/{id}", s.handleRequestDelete)
	mux.HandleFunc("GET /api/requests/{id}/stats", s.handleRequestStats)
	mux.HandleFunc("GET /api/requests/{id}/curl", s.handleRequestCurl)

	mux.HandleFunc("GET /api/environments", s.handleEnvList)
	mux.HandleFunc("PUT /api/environments/{name}", s.handleEnvPut)
	mux.HandleFunc("DELETE /api/environments/{name}", s.handleEnvDelete)

	mux.HandleFunc("GET /api/presets", s.handlePresetList)
	mux.HandleFunc("POST /api/presets", s.handlePresetCreate)
	mux.HandleFunc("PUT /api/presets/{id}", s.handlePresetUpdate)
	mux.HandleFunc("DELETE /api/presets/{id}", s.handlePresetDelete)

	mux.HandleFunc("POST /api/send", s.handleSend)
	mux.HandleFunc("POST /api/run", s.handleRun)

	mux.HandleFunc("GET /api/history", s.handleHistoryList)
	mux.HandleFunc("GET /api/history/{id}", s.handleHistoryGet)

	mux.HandleFunc("GET /api/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsPut)

	mux.HandleFunc("POST /api/import/postman", s.handleImportPostman)
	mux.HandleFunc("POST /api/import/curl", s.handleImportCurl)
	mux.HandleFunc("POST /api/import/openapi", s.handleImportOpenAPI)
	mux.HandleFunc("GET /api/export", s.handleExport)

	mux.HandleFunc("GET /api/xray/settings", s.handleXraySettingsGet)
	mux.HandleFunc("PUT /api/xray/settings", s.handleXraySettingsPut)
	mux.HandleFunc("POST /api/xray/push", s.handleXrayPush)
	return mux
}

// ListenAndServe binds to 127.0.0.1:port (0 picks a free one) and serves
// until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	fmt.Printf("relay ui: http://%s\n", ln.Addr())
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

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	set, err := s.DB.Settings()
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, set)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var set store.Settings
	if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
		httpError(w, 400, err)
		return
	}
	if err := s.DB.SaveSettings(set); err != nil {
		httpError(w, 422, err)
		return
	}
	writeJSON(w, set)
}

// engineOptions applies the stored workspace settings on top of the
// server's defaults for each send.
func (s *Server) engineOptions() engine.Options {
	opts := s.Engine
	set, err := s.DB.Settings()
	if err != nil {
		return opts
	}
	opts.Timeout = time.Duration(set.TimeoutSeconds) * time.Second
	opts.FollowRedirects = set.FollowRedirects
	opts.Insecure = set.Insecure
	return opts
}

func (s *Server) getenv() func(string) string {
	if s.Getenv != nil {
		return s.Getenv
	}
	return os.Getenv
}

// resolveStored resolves a stored request: preset headers (collection then
// folder level), collection/folder headers and vars, environment, auth.
// Returns the resolved request, the scope, and any preset secret values
// that must be masked in display surfaces.
func (s *Server) resolveStored(req *store.Request, envName string) (*vars.Resolved, *vars.Scope, []string, error) {
	col, err := s.DB.Collection(req.CollectionID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("collection: %w", err)
	}
	var folder *store.Folder
	if req.FolderID != nil {
		if folder, err = s.DB.Folder(*req.FolderID); err != nil {
			return nil, nil, nil, fmt.Errorf("folder: %w", err)
		}
	}
	presetHeaders, presetSecrets, err := s.DB.PresetHeadersFor(req.CollectionID, req.FolderID)
	if err != nil {
		return nil, nil, nil, err
	}

	inherited := map[string]string{}
	for k, v := range presetHeaders {
		inherited[k] = v
	}
	for k, v := range col.Headers {
		inherited[k] = v
	}
	if folder != nil {
		for k, v := range folder.Headers {
			inherited[k] = v
		}
	}

	var folderVars map[string]string
	if folder != nil {
		folderVars = folder.Vars
	}
	scope := vars.NewScope(req.Spec.Vars, folderVars, col.Vars)
	if envName != "" {
		env, err := s.DB.Environment(envName)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("environment %q not found", envName)
		}
		if err := scope.AddEnvironment(&dsl.Environment{Vars: env.Vars, Secrets: env.Secrets}, s.getenv()); err != nil {
			return nil, nil, nil, err
		}
	}

	resolved, err := vars.Resolve(req.Spec, inherited, scope)
	if err != nil {
		return nil, nil, nil, err
	}
	return resolved, scope, presetSecrets, nil
}

func mask(s string, scope *vars.Scope, extraSecrets []string) string {
	s = scope.MaskSecrets(s)
	for _, v := range extraSecrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "••••••")
		}
	}
	return s
}

// handleRequestCurl returns a copy-pasteable curl command for a request.
// Environment secret values are replaced with $RELAY_SECRET_* shell
// references and preset secret values are masked, so the command never
// carries secret material.
func (s *Server) handleRequestCurl(w http.ResponseWriter, r *http.Request) {
	id, err := atoi64(r.PathValue("id"))
	if err != nil {
		httpError(w, 400, fmt.Errorf("bad id %q", r.PathValue("id")))
		return
	}
	req, err := s.DB.Request(id)
	if err != nil {
		httpError(w, 404, fmt.Errorf("request %d not found", id))
		return
	}
	resolved, scope, presetSecrets, err := s.resolveStored(req, r.URL.Query().Get("env"))
	if err != nil {
		httpError(w, 422, err)
		return
	}
	cmd := porter.Curl(resolved)
	for name, value := range scope.SecretValues() {
		if value != "" {
			// `'"$VAR"'` closes any surrounding single-quoted segment, lets
			// the shell expand the variable, and reopens the quote — correct
			// both inside and outside quoted arguments.
			cmd = strings.ReplaceAll(cmd, value, `'"$`+vars.SecretEnvVar(name)+`"'`)
		}
	}
	for _, value := range presetSecrets {
		if value != "" {
			cmd = strings.ReplaceAll(cmd, value, "••••••")
		}
	}
	writeJSON(w, map[string]string{"curl": cmd})
}

type sendResult struct {
	Method         string             `json:"method"`
	URL            string             `json:"url"`
	RequestHeaders map[string]string  `json:"requestHeaders"`
	HeaderOrigin   map[string]string  `json:"headerOrigin"`
	Status         int                `json:"status"`
	StatusText     string             `json:"statusText"`
	Proto          string             `json:"proto"`
	Headers        map[string]string  `json:"headers"`
	Body           string             `json:"body"`
	Truncated      bool               `json:"truncated"`
	Size           int64              `json:"size"`
	Timing         map[string]float64 `json:"timing"`
	Assertions     []assertionResult  `json:"assertions,omitempty"`
	ScriptTests    []scriptTestResult `json:"scriptTests,omitempty"`
}

type scriptTestResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}

type assertionResult struct {
	Type    string `json:"type"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID int64  `json:"requestId"`
		Env       string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	req, err := s.DB.Request(in.RequestID)
	if err != nil {
		httpError(w, 404, fmt.Errorf("request %d not found", in.RequestID))
		return
	}
	out, status, err := s.execute(r.Context(), req, in.Env, true)
	if err != nil {
		httpError(w, status, err)
		return
	}
	writeJSON(w, out)
}

// execute resolves, sends, asserts, and (optionally) records history.
func (s *Server) execute(ctx context.Context, req *store.Request, envName string, record bool) (*sendResult, int, error) {
	resolved, scope, presetSecrets, err := s.resolveStored(req, envName)
	if err != nil {
		return nil, 422, err
	}
	result, err := engine.Send(ctx, resolved, s.engineOptions())
	if err != nil {
		return nil, 502, err
	}

	asserts, err := runner.ResolveAssertions(req.Spec.Assertions, scope)
	if err != nil {
		return nil, 422, err
	}
	outcomes := assert.Evaluate(asserts, result)

	out := &sendResult{
		Method:         resolved.Method,
		URL:            mask(resolved.URL, scope, presetSecrets),
		RequestHeaders: map[string]string{},
		HeaderOrigin:   resolved.HeaderOrigin,
		Status:         result.Status,
		StatusText:     result.StatusText,
		Proto:          result.Proto,
		Headers:        map[string]string{},
		Size:           result.Size,
		Timing: map[string]float64{
			"dns":      ms(result.Timing.DNS),
			"connect":  ms(result.Timing.Connect),
			"tls":      ms(result.Timing.TLS),
			"ttfb":     ms(result.Timing.TTFB),
			"download": ms(result.Timing.Download),
			"total":    ms(result.Timing.Total),
		},
	}
	for k := range resolved.Headers {
		out.RequestHeaders[k] = mask(resolved.Headers.Get(k), scope, presetSecrets)
	}
	for k := range result.Headers {
		out.Headers[k] = result.Headers.Get(k)
	}
	const uiBodyCap = 2 << 20
	body := result.Body
	if len(body) > uiBodyCap {
		body, out.Truncated = body[:uiBodyCap], true
	}
	out.Body = string(body)
	for _, o := range outcomes {
		out.Assertions = append(out.Assertions, assertionResult{Type: o.Assertion.Type, Passed: o.Passed, Message: o.Message})
	}

	if req.Spec.Scripts != nil && strings.TrimSpace(req.Spec.Scripts.Tests) != "" {
		respHeaders := map[string]string{}
		for k := range result.Headers {
			respHeaders[k] = result.Headers.Get(k)
		}

		envVars := map[string]string{}
		if envName != "" {
			if env, err := s.DB.Environment(envName); err == nil {
				for k, v := range env.Vars {
					envVars[k] = v
				}
			}
		}
		colVars := map[string]string{}
		if col, err := s.DB.Collection(req.CollectionID); err == nil {
			for k, v := range col.Vars {
				colVars[k] = v
			}
			if req.FolderID != nil {
				if folder, err := s.DB.Folder(*req.FolderID); err == nil {
					for k, v := range folder.Vars {
						colVars[k] = v
					}
				}
			}
		}
		reqVars := map[string]string{}
		for k, v := range req.Spec.Vars {
			reqVars[k] = v
		}

		sr := script.RunTests(req.Spec.Scripts.Tests, &script.Scope{
			Env:        envVars,
			Collection: colVars,
			Request:    reqVars,
		}, &script.Response{
			Code:       result.Status,
			Status:     result.StatusText,
			Headers:    respHeaders,
			Body:       result.Body,
			DurationMs: out.Timing["total"],
		})
		for _, t := range sr.Tests {
			out.ScriptTests = append(out.ScriptTests, scriptTestResult{Name: t.Name, Passed: t.Passed, Error: t.Error})
		}
		for _, errMsg := range sr.Errors {
			out.ScriptTests = append(out.ScriptTests, scriptTestResult{Name: "Script runtime", Passed: false, Error: errMsg})
		}
	}

	if record {
		_ = s.DB.AddHistory(&store.HistoryEntry{
			RequestID:   &req.ID,
			RequestName: req.Spec.Name,
			Method:      resolved.Method,
			URL:         out.URL,
			Status:      result.Status,
			DurationMs:  out.Timing["total"],
			RespHeaders: out.Headers,
			RespBody:    body,
			Timing:      out.Timing,
			SentAt:      time.Now(),
		})
	}
	return out, 200, nil
}

type runResult struct {
	RequestID  int64             `json:"requestId"`
	Name       string            `json:"name"`
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Status     int               `json:"status"`
	DurationMs float64           `json:"durationMs"`
	Passed     bool              `json:"passed"`
	Error      string            `json:"error,omitempty"`
	Assertions []assertionResult `json:"assertions,omitempty"`
	ScriptTests []scriptTestResult `json:"scriptTests,omitempty"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CollectionID int64  `json:"collectionId"`
		Env          string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	requests, err := s.DB.Requests(in.CollectionID)
	if err != nil || len(requests) == 0 {
		httpError(w, 404, fmt.Errorf("collection %d has no requests", in.CollectionID))
		return
	}

	var results []runResult
	var durations []float64
	passed := 0
	started := time.Now()
	for i := range requests {
		req := &requests[i]
		rr := runResult{RequestID: req.ID, Name: req.Spec.Name, Method: req.Spec.Method, URL: req.Spec.URL}
		out, _, err := s.execute(r.Context(), req, in.Env, false)
		if err != nil {
			rr.Error = err.Error()
		} else {
			rr.URL = out.URL
			rr.Status = out.Status
			rr.DurationMs = out.Timing["total"]
			rr.Assertions = out.Assertions
			rr.ScriptTests = out.ScriptTests
			rr.Passed = true
			for _, a := range out.Assertions {
				if !a.Passed {
					rr.Passed = false
				}
			}
			for _, t := range out.ScriptTests {
				if !t.Passed {
					rr.Passed = false
				}
			}
			durations = append(durations, rr.DurationMs)
		}
		if rr.Passed {
			passed++
		}
		results = append(results, rr)
	}

	writeJSON(w, map[string]any{
		"results":    results,
		"executed":   len(results),
		"passed":     passed,
		"failed":     len(results) - passed,
		"p95Ms":      p95(durations),
		"durationMs": float64(time.Since(started).Microseconds()) / 1000,
		"finishedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

func p95(durations []float64) float64 {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]float64(nil), durations...)
	for i := 1; i < len(sorted); i++ { // insertion sort; n is tiny
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	idx := (len(sorted)*95 + 99) / 100
	if idx > 0 {
		idx--
	}
	return sorted[idx]
}

func (s *Server) handleImportPostman(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(r, 20<<20)
	if err != nil {
		httpError(w, 400, err)
		return
	}
	tmp, err := os.MkdirTemp("", "relay-import-*")
	if err != nil {
		httpError(w, 500, err)
		return
	}
	defer os.RemoveAll(tmp)
	n, err := porter.ImportPostman(data, tmp)
	if err != nil {
		httpError(w, 422, err)
		return
	}
	colID, err := s.DB.SeedFromDir(tmp)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"collectionId": colID, "requests": n})
}

// handleImportCurl parses a pasted curl command into a new request in the
// given collection.
func (s *Server) handleImportCurl(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CollectionID int64  `json:"collectionId"`
		FolderID     *int64 `json:"folderId"`
		Curl         string `json:"curl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	spec, err := porter.ParseCurl(in.Curl)
	if err != nil {
		httpError(w, 422, err)
		return
	}
	if in.CollectionID == 0 {
		cols, err := s.DB.Collections()
		if err != nil {
			httpError(w, 500, err)
			return
		}
		if len(cols) == 0 {
			col := &store.Collection{Name: "Imported", Headers: map[string]string{}, Vars: map[string]string{}}
			if err := s.DB.CreateCollection(col); err != nil {
				httpError(w, 500, err)
				return
			}
			in.CollectionID = col.ID
		} else {
			in.CollectionID = cols[0].ID
		}
	}
	req := &store.Request{CollectionID: in.CollectionID, FolderID: in.FolderID, Spec: spec}
	if err := s.DB.CreateRequest(req); err != nil {
		httpError(w, 422, err)
		return
	}
	writeJSON(w, req)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "curl" {
		reqID, err := atoi64(r.URL.Query().Get("request"))
		if err != nil {
			httpError(w, 400, fmt.Errorf("request query param required for curl export"))
			return
		}
		req, err := s.DB.Request(reqID)
		if err != nil {
			httpError(w, 404, fmt.Errorf("request %d not found", reqID))
			return
		}
		resolved, scope, presetSecrets, err := s.resolveStored(req, r.URL.Query().Get("env"))
		if err != nil {
			httpError(w, 422, err)
			return
		}
		cmd := porter.Curl(resolved)
		for name, value := range scope.SecretValues() {
			if value != "" {
				cmd = strings.ReplaceAll(cmd, value, `'$`+vars.SecretEnvVar(name)+`'`)
			}
		}
		for _, value := range presetSecrets {
			if value != "" {
				cmd = strings.ReplaceAll(cmd, value, "••••••")
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(cmd))
		return
	}

	colID, err := atoi64(r.URL.Query().Get("collection"))
	if err != nil {
		httpError(w, 400, fmt.Errorf("collection query param required"))
		return
	}
	tmp, err := os.MkdirTemp("", "relay-export-*")
	if err != nil {
		httpError(w, 500, err)
		return
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "collection")
	if err := s.DB.ExportCollectionDir(colID, dir); err != nil {
		httpError(w, 500, err)
		return
	}

	var env *dsl.Environment
	if name := r.URL.Query().Get("env"); name != "" {
		e, err := s.DB.Environment(name)
		if err != nil {
			httpError(w, 404, fmt.Errorf("environment %q not found", name))
			return
		}
		env = &dsl.Environment{Vars: e.Vars, Secrets: e.Secrets}
	}

	var script string
	contentType := "text/plain; charset=utf-8"
	switch format {
	case "k6":
		script, err = porter.K6(dir, env)
	case "playwright":
		script, err = porter.Playwright(dir, env)
	case "postman":
		var out []byte
		out, err = porter.ExportPostman(dir)
		script, contentType = string(out), "application/json"
	case "openapi":
		var out []byte
		out, err = porter.ExportOpenAPI(dir)
		script, contentType = string(out), "application/json"
	default:
		httpError(w, 400, fmt.Errorf("format must be curl, k6, playwright, postman or openapi"))
		return
	}
	if err != nil {
		httpError(w, 422, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write([]byte(script))
}

// handleImportOpenAPI imports an OpenAPI 3.x JSON document as a collection.
func (s *Server) handleImportOpenAPI(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(r, 20<<20)
	if err != nil {
		httpError(w, 400, err)
		return
	}
	tmp, err := os.MkdirTemp("", "relay-import-oa-*")
	if err != nil {
		httpError(w, 500, err)
		return
	}
	defer os.RemoveAll(tmp)
	n, err := porter.ImportOpenAPI(data, tmp)
	if err != nil {
		httpError(w, 422, err)
		return
	}
	colID, err := s.DB.SeedFromDir(tmp)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"collectionId": colID, "requests": n})
}

// handleExport adds openapi to the existing export handler.
// (The original handler is preserved; this only extends the switch.)

// --- Xray Cloud settings ---

func (s *Server) handleXraySettingsGet(w http.ResponseWriter, r *http.Request) {
	set, err := s.DB.XraySettings()
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, set)
}

func (s *Server) handleXraySettingsPut(w http.ResponseWriter, r *http.Request) {
	var xs store.XraySettings
	if err := json.NewDecoder(r.Body).Decode(&xs); err != nil {
		httpError(w, 400, err)
		return
	}
	if err := s.DB.SaveXraySettings(xs); err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, xs)
}

// handleXrayPush runs the collection and pushes results to Xray Cloud.
func (s *Server) handleXrayPush(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CollectionID int64  `json:"collectionId"`
		Env          string `json:"env"`
		Summary      string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, 400, err)
		return
	}
	xs, err := s.DB.XraySettings()
	if err != nil || xs.ProjectKey == "" {
		httpError(w, 422, fmt.Errorf("xray not configured: set project key in Settings → Xray"))
		return
	}
	xrayCfg := xray.Config{
		ClientID:     os.Getenv("RELAY_XRAY_CLIENT_ID"),
		ClientSecret: os.Getenv("RELAY_XRAY_CLIENT_SECRET"),
	}
	if xrayCfg.ClientID == "" || xrayCfg.ClientSecret == "" {
		httpError(w, 422, fmt.Errorf("RELAY_XRAY_CLIENT_ID and RELAY_XRAY_CLIENT_SECRET must be set"))
		return
	}
	if xs.CloudURL != "" {
		xrayCfg.GQLURL = xs.CloudURL
	}

	// Run the collection.
	requests, err := s.DB.Requests(in.CollectionID)
	if err != nil || len(requests) == 0 {
		httpError(w, 404, fmt.Errorf("collection %d has no requests", in.CollectionID))
		return
	}
	started := time.Now()
	var results []tm.TestResult
	for i := range requests {
		req := &requests[i]
		out, _, execErr := s.execute(r.Context(), req, in.Env, false)
		status := tm.StatusPASS
		comment := ""
		if execErr != nil {
			status = tm.StatusFAIL
			comment = execErr.Error()
		} else {
			for _, a := range out.Assertions {
				if !a.Passed {
					status = tm.StatusFAIL
					if comment != "" {
						comment += "; "
					}
					comment += a.Message
				}
			}
		}
		results = append(results, tm.TestResult{
			Name:       req.Spec.Name,
			Status:     status,
			Comment:    comment,
			DurationMs: out.Timing["total"],
		})
	}
	finished := time.Now()

	summary := in.Summary
	if summary == "" {
		col, _ := s.DB.Collection(in.CollectionID)
		if col != nil {
			summary = "Relay run: " + col.Name
		} else {
			summary = "Relay automated test execution"
		}
	}

	exec := tm.Execution{
		ProjectKey:  xs.ProjectKey,
		TestPlanKey: xs.TestPlanKey,
		Summary:     summary,
		StartedAt:   started,
		FinishedAt:  finished,
		Results:     results,
	}

	client := xray.New(xrayCfg)
	key, err := client.PushExecution(exec)
	if err != nil {
		httpError(w, 502, fmt.Errorf("xray push failed: %w", err))
		return
	}
	writeJSON(w, map[string]string{"executionKey": key})
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
