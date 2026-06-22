# Relay — Implementation Plan

**Status:** Draft v0.1 (companion to PRD v0.1, June 2026)
**Scope:** Engineering plan to deliver PRD Phases 1–3 (~16–20 weeks)

> Current state note: this document preserves the original roadmap. The codebase has since landed a simpler implementation shape: stdlib `flag` CLI rather than cobra, embedded single-file browser UI plus Wails v2 desktop wrapper rather than a separate React frontend tree, SQLite-backed workbench state, file-based CLI runner, Postman/OpenAPI/curl import, Postman/OpenAPI/curl/k6/Playwright export, goja scripts, UI Test Management, and Xray UI/local API integration. Treat unimplemented items in this plan as roadmap, not current usage instructions.

This document translates the Relay PRD into a concrete build plan: repository layout, module boundaries, sequenced milestones with acceptance criteria, testing/CI strategy, and the key technical decisions that need to be locked in early.

---

## 1. Guiding Principles

1. **Core-first, UI-second.** Every feature lands in the Go core (`internal/`) with tests before it gets a UI. The CLI and the desktop app are two thin frontends over the same engine — this is what makes the headless CI runner "free" in Phase 2.
2. **The file format is the API.** The `.req.toml` DSL and workspace directory layout are the most expensive things to change after v1. They get a written spec, a versioned schema, and golden-file tests before any feature builds on them.
3. **Porters are pure functions.** Import/export codecs take bytes in, produce domain structs or bytes out, with no I/O or app state. This keeps them trivially testable against fixture corpora (real Postman/OpenAPI/HAR files).
4. **No CGO anywhere.** Pure-Go dependencies only (goja, modernc.org/sqlite, go-keyring) so cross-compilation for all three OSes stays a one-line `go build`.
5. **Performance is a feature with a budget.** Cold start, RAM, and request-overhead targets from PRD §7 are enforced by automated benchmarks in CI from Phase 1, not measured at the end.

---

## 2. Repository Layout

Single Go module monorepo. Wails v3 hosts the frontend; the CLI shares the core.

```
relay/
├── cmd/
│   ├── relay/              # CLI entrypoint (cobra): run, import, export, env
│   └── relay-app/          # Wails v3 desktop entrypoint
├── internal/
│   ├── engine/             # RequestEngine: http client, httptrace, redirects, proxies, TLS/mTLS
│   ├── auth/               # Auth providers: bearer, basic, apikey, oauth2, sigv4, mtls
│   ├── workspace/          # WorkspaceStore: DSL parse/serialize, fs layout, watcher
│   ├── dsl/                # .req.toml schema, versioning, validation (no fs access)
│   ├── vars/               # VariableResolver: scope chain, computed vars ({{$uuid}}…), secrets refs
│   ├── secrets/            # go-keyring wrapper + fallback encrypted file store
│   ├── script/             # goja runtime, sandbox, pm.* shim
│   ├── runner/             # Sequential runner, assertions, data-driven iteration, reporters
│   ├── assert/             # Assertion evaluators: status, jsonpath, xpath, header, latency
│   ├── porter/             # Import/export codecs
│   │   ├── postman/        # Collection v2.x + environments (import & export)
│   │   ├── openapi/        # OpenAPI 3.x / Swagger 2.0 (import & export)
│   │   ├── curlfmt/        # curl import & export
│   │   ├── har/            # HAR import & export
│   │   ├── k6/             # k6 script export
│   │   ├── playwright/     # Playwright API test export
│   │   ├── insomnia/       # Insomnia import
│   │   └── bruno/          # Bruno import
│   ├── adapters/           # Test-management adapters
│   │   ├── tm/             # Adapter interface + registry
│   │   └── xray/           # Xray Cloud GraphQL adapter
│   ├── history/            # SQLite-backed history + FTS search index (cache only, never source of truth)
│   └── app/                # Wails-bound application services (thin orchestration layer)
├── frontend/               # React + TypeScript + CodeMirror 6 (Vite)
│   └── src/
│       ├── components/     # request builder, header panel, response viewer, tree, runner UI
│       ├── state/          # store (zustand), Wails event bridge
│       └── bindings/       # generated Wails bindings
├── docs/
│   ├── PRD.md
│   ├── IMPLEMENTATION_PLAN.md
│   ├── dsl-spec.md         # versioned .req.toml + workspace layout spec
│   └── pm-shim-compat.md   # published pm.* compatibility matrix
├── testdata/               # fixture corpora: postman/, openapi/, har/, curl/, golden/
└── .github/workflows/      # ci.yml, bench.yml, release.yml
```

**Why `internal/` and not `pkg/`:** nothing is a public Go API in v1; keeping it internal preserves freedom to refactor. If a plugin story emerges (Zephyr/TestRail adapters), `internal/adapters/tm` graduates to `pkg/` then.

---

## 3. Foundational Decisions to Lock in Week 1

| Decision | Recommendation | Rationale |
|---|---|---|
| Frontend framework | **React + TypeScript** | Largest ecosystem for CodeMirror 6 integration, virtualized trees/lists (needed for large collections and 50MB responses); team familiarity over Svelte's marginal size win — system webview means bundle size matters less |
| Code editor | **CodeMirror 6** | Modular (~300KB used vs Monaco's multi-MB), first-class JSON/XML modes, custom decorations for `{{var}}` interpolation highlighting |
| DSL serialization | **TOML via `BurntSushi/toml` + custom ordered writer** | PRD's sketch is TOML; the writer must preserve key order and formatting so git diffs stay minimal (this is a hard requirement — naive marshaling reorders keys and breaks the "git-diffable" promise) |
| CLI framework | **cobra** | Standard, supports the `relay run collection/ --env sit --report junit` shape directly |
| Wails | **v3 alpha-pin with v2 fallback decision gate at end of Week 2** | Wails v3 is the target, but if a Week-1/2 spike reveals blocking instability, fall back to v2 (multi-window and systray aren't v1 requirements, so v2 suffices) |
| State management (frontend) | **zustand** | Minimal, no boilerplate, plays well with event-driven Wails bindings |
| SQLite | **modernc.org/sqlite** | Pure Go, per principle #4 |

**Week-1 spike (timeboxed, 3 days):** a Wails v3 walking skeleton — one window, a URL bar, a send button, response text — built and signed/notarized-dry-run on Windows + macOS + Linux in CI. This de-risks the single biggest unknown (webview/packaging) before any feature work.

---

## 4. Core Domain Model

Defined in `internal/dsl` and shared by everything:

```go
type Request struct {
    Name       string
    Method     string
    URL        string            // raw, with {{var}} placeholders
    Headers    []Header          // ordered; Header{Name, Value, Disabled, Secret bool}
    Inherit    InheritSpec       // preset refs + disable flags for inherited headers
    Body       Body              // tagged union: json | xml | form | urlencoded | raw | binary
    Auth       *AuthSpec
    Assertions []Assertion
    Scripts    Scripts           // PreRequest, PostResponse (goja source)
    Meta       Meta              // xray test_key, requirements, tags
    SchemaVersion int
}

type Resolved struct {            // post-variable-resolution, what the engine actually sends
    Method, URL string
    Headers     http.Header       // fully merged: workspace→collection→folder→request→overrides
    HeaderTrace map[string]Origin // where each header came from (powers UI inheritance indicators)
    Body        []byte
}

type Response struct {
    Status   int
    Headers  http.Header
    Body     ResponseBody         // streamed to temp file above threshold; in-memory below
    Timing   Timing               // DNS, Connect, TLS, TTFB, Download (from httptrace)
    Size     int64
}
```

Key invariant: **resolution is a pure pipeline** `Request → (scopes, env, secrets, computed) → Resolved`, with `HeaderTrace` carrying provenance. The UI's "where did this header come from" indicators and the exporters' "exclude secret headers" rule both read from the same structures — no parallel logic.

---

## 5. Milestones

### Phase 0 — Foundations (Weeks 1–2)

Runs partly in parallel with the spike.

- **W1:** Repo scaffolding, CI (lint/test/build matrix for 3 OSes), Wails v3 walking skeleton spike, decision gate inputs.
- **W1–2:** `docs/dsl-spec.md` — full `.req.toml` schema (requests, folder/collection config files `folder.toml` / `collection.toml`, environment files, header preset files), workspace directory layout, schema versioning + migration rules. Reviewed and frozen as v1.
- **W2:** `internal/dsl` parser/serializer with golden-file round-trip tests (parse → serialize → byte-identical), including the git-merge scenario from PRD §9 (two divergent edits merge cleanly).

**Exit criteria:** skeleton app launches on all 3 OSes in CI; DSL round-trips byte-identically; merge test green.

### Phase 1 — Core client (Weeks 3–10)

| Weeks | Workstream A (Go core) | Workstream B (Frontend) |
|---|---|---|
| 3–4 | `engine`: net/http client, httptrace timing, redirects/proxy/TLS config, custom verbs; `vars`: scope-chain resolver + computed vars | App shell: tree view, tabs, URL bar with live resolved-URL preview, method selector |
| 5–6 | `workspace`: store, fs watcher (external git edits reflect live), CRUD; `secrets`: keyring + env-file `keyring:` references | Body editors (CodeMirror: JSON w/ validation, raw, form-data, urlencoded, binary); send → response wiring |
| 7–8 | Header presets + inheritance merge with provenance; `auth`: bearer/basic/apikey (OAuth2/SigV4 deferred to Phase 3); `history` SQLite store + FTS | Header panel: inheritance indicators, override/disable toggles, bulk paste-parse, secret masking; environments UI |
| 9–10 | `porter/postman` import (v2.x + environments), `porter/curlfmt` export; `.env` import | Response viewer: pretty/raw/preview, JSON tree + JSONPath copy, timing waterfall, streaming for large bodies; history + search UI; saved example responses |

**Exit criteria (= PRD Phase 1 scope):**
- Import a real-world 200+ request Postman collection; browse, edit, send, save.
- Header inheritance demo: workspace preset → collection → request override, provenance visible.
- Secrets never appear in any file under the workspace dir (automated test greps the tree).
- Benchmarks in CI: cold start < 1s, RAM < 150MB on the 500-request synthetic workspace, app-added latency < 5ms (PRD §7).

### Phase 2 — QE bridge (Weeks 11–15)

- **W11–12:** `assert` evaluators (status, jsonpath, header, latency); `runner` sequential engine with per-request assertions, CSV/JSON data-driven iteration, delay controls; runner events stream to UI.
- **W12–13:** `cmd/relay` CLI: `relay run <path> --env <name> --report junit|json`, exit codes for CI; JUnit XML + JSON reporters. Dogfood: Relay's own integration tests run via `relay run` in GitHub Actions.
- **W13–14:** `porter/k6` export (folders → scenarios, assertions → checks, env vars → k6 env, threshold scaffolding); `porter/playwright` export (folders → describe blocks, assertions → expect).
- **W14–15:** `porter/openapi` import + export; response diff view (current vs saved example, cross-environment).

**Exit criteria:**
- PRD §9 metric: Postman import → running k6 script in under 5 minutes, demonstrated on a recorded walkthrough.
- `relay run` produces JUnit consumed by a real GitHub Actions job (our own CI).
- Exported k6/Playwright scripts execute unmodified against a fixture API (httptest server in CI).

### Phase 3 — Integration & polish (Weeks 16–20)

- **W16–17:** `script` runtime — goja sandbox (CPU/memory/time limits, no fs/net by default), `pm.*` shim (variables get/set, `pm.test`, `pm.expect`, `pm.response.json()`, request mutation in pre-request); `docs/pm-shim-compat.md` published; unsupported APIs throw with a documented error, never silently no-op.
- **W17–18:** `adapters/xray` — Xray Cloud GraphQL: auth, push test executions, map `meta.xray.test_key`/`requirements` to traceability; contract tests against recorded Xray sandbox responses; `adapters/tm` interface kept Xray-agnostic for future Zephyr/TestRail.
- **W18–19:** OAuth2 flows (auth-code with loopback redirect capture + client credentials, token cache in keychain); AWS SigV4; mTLS client certs; SOAP: XML body editor with WSDL-aware envelope snippets, XPath assertions.
- **W19–20:** HAR import/export; bulk header paste from HAR; hardening pass — fuzz the DSL parser and importers (`go-fuzz` corpora from `testdata/`), performance re-validation, packaging/signing (MSI/notarized DMG/AppImage+deb), v1.0 release candidate.

**Exit criteria:**
- A regression run pushes results to Xray (sandbox) end-to-end from the UI — PRD §9.
- pm-shim compatibility matrix published; top-20 Postman snippet patterns (from public collections) pass or fail loudly.
- Signed installers for all 3 OSes under 50MB.

---

## 6. Testing & CI Strategy

| Layer | Approach |
|---|---|
| DSL & porters | Golden-file tests: fixture corpus in `testdata/` (real Postman exports, OpenAPI specs, HARs, Bruno/Insomnia samples); round-trip tests where lossless round-trip is promised (Postman v2.1) |
| Engine | `httptest` servers covering redirects, chunked/streaming, HTTP/2, TLS variants; timing assertions against synthetic delays |
| Resolver & headers | Table-driven tests over the full scope chain incl. provenance; property test: secret-flagged values never appear in serialized output or exports |
| Runner & CLI | Relay's own API integration tests written *as a Relay collection*, run by `relay run` in CI (dogfooding, from W13) |
| Scripts | pm-shim conformance suite derived from the published compat matrix |
| Adapters | Contract tests against recorded Xray GraphQL responses (no live dependency in CI); a nightly optional job against a real sandbox |
| Frontend | Vitest unit tests; Playwright E2E against the built Wails app per OS, including screenshot tests (PRD §10 webview-inconsistency mitigation) |
| Performance | `bench.yml`: cold-start, RSS on 500-request synthetic workspace, request overhead — regression gates, fail PR if budget exceeded by >10% |
| Fuzzing | Native Go fuzzing on DSL parser and all importers |

CI matrix: `ubuntu-latest`, `macos-latest`, `windows-latest` on every PR. Release workflow builds, signs, and uploads installers on tag.

---

## 7. Performance Budget Enforcement (PRD §7)

- **Cold start < 1s:** lazy-load the history SQLite index and search; parse workspace files on demand with an in-memory LRU; the tree view reads only file names + minimal metadata at startup.
- **RAM < 150MB:** responses above a threshold (default 5MB) stream to a temp file and the viewer virtualizes; CodeMirror documents are capped with "raw mode beyond threshold" per PRD §10.
- **Request overhead < 5ms:** resolution pipeline benchmarked in isolation; no JSON re-marshaling on the hot path; script runtime only instantiated when a request actually has scripts.

---

## 8. Risk Register (engineering view, extends PRD §10)

| Risk | Trigger/Signal | Mitigation in this plan |
|---|---|---|
| Wails v3 alpha instability | Week-1 spike fails packaging or webview bugs | Explicit fallback gate to Wails v2 at end of W2; nothing in v1 scope requires v3-only features |
| TOML writer breaks git-diffability | Golden tests show reordered keys | Custom ordered serializer built in Phase 0, before features depend on it |
| pm-shim scope creep | Users expect full sandbox (require, fetch) | Compat matrix is the contract; loud errors; shim work is a fixed 1.5-week box in W16–17 |
| OAuth2 loopback capture flakiness across OSes | Auth-code flow fails on Linux distros | Loopback server + manual paste fallback; covered by per-OS E2E |
| Large-response UI lockups | E2E test with 50MB payload janks | Streaming + virtualization built into the viewer from Phase 1, not retrofitted |
| Keychain unavailability (headless Linux/CI) | `relay run` in containers | Secrets layer has an explicit fallback: env-var injection (`RELAY_SECRET_*`) for CLI/CI contexts |

---

## 9. Team Shape & Sequencing Assumptions

Plan assumes **2–3 engineers**: one owning Go core (engine/runner/porters), one owning frontend, one floating (CLI, adapters, packaging/CI). With 2 engineers, stretch each phase ~30% and serialize Workstream A/B inside Phase 1.

Critical path: **DSL spec (W1–2) → workspace store (W5–6) → runner (W11–12) → CLI (W12–13) → Xray adapter (W17–18)**. Porters, response viewer, and auth providers all hang off the critical path and can flex.

---

## 10. Definition of Done for v1.0

All PRD §9 success metrics demonstrated:

1. Postman import → k6 script running: **< 5 min** (recorded walkthrough).
2. Cold start < 1s and RAM < 150MB on the 500-request workspace (CI benchmark, all 3 OSes).
3. Full regression run pushed to Xray from the app, no glue code.
4. Two-contributor git merge of a collection with zero corruption (automated merge test).
5. Signed installers < 50MB for Windows, macOS, Linux.
