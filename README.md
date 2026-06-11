# Relay

A fast, lightweight, local-first API client. Collections are plain TOML files
in your repo — branch, diff, review, and merge them like any other code. The
same engine drives interactive use and headless CI runs.

Single static binary (~6MB), no Electron, no account, no cloud, no telemetry.

```
relay send  <file.req.toml>  [--env NAME] [-v]
relay run   <dir>            [--env NAME] [--report junit|json] [--out FILE]
                             [--data rows.csv|rows.json] [--bail] [--delay 200ms]
relay import postman <collection.json> [--out DIR]
relay export curl <file.req.toml> [--env NAME]
relay export k6 <dir> [--env NAME] [--out script.js]
relay export playwright <dir> [--env NAME] [--out api.spec.ts]
relay ui    [dir]            [--port 7717]
```

## Install

```sh
go install github.com/muhaymien96/relay/cmd/relay@latest
```

Or build from source: `go build -o relay ./cmd/relay`.

## Quick start

A request is one file:

```toml
# 02-verify-individual.req.toml
name = "Verify Individual"
method = "POST"
url = "{{baseUrl}}/aml/v2/verify"

[headers]
Content-Type = "application/json"

[auth]
type = "bearer"            # bearer | basic | apikey
token = "{{apiToken}}"

[body]
type = "json"              # json | xml | raw | urlencoded (or file = "payload.bin")
content = '''
{ "idNumber": "{{testIdNumber}}", "channel": "API" }
'''

[[assertions]]
type = "status"
equals = 200

[[assertions]]
type = "jsonpath"          # subset: $.field.nested[0]["key"]
path = "$.result.status"
equals = "VERIFIED"

[[assertions]]
type = "max_ms"
max_ms = 2000
```

Send it:

```sh
relay send 02-verify-individual.req.toml --env local -v
```

```
POST http://127.0.0.1:18080/aml/v2/verify  200 OK  1ms
< timing: dns=0s connect=368µs tls=0s ttfb=308µs download=127µs total=1.054ms
{ "result": { "status": "VERIFIED" } }
```

Run a whole collection (every `*.req.toml` under the directory, in lexical
order) and emit JUnit for CI:

```sh
relay run examples/aml-demo --env local --report junit --out report.xml
```

Exit code is non-zero when any request fails, so it slots straight into
GitHub Actions / Azure DevOps.

## Environments & secrets

`environments/<name>.toml`, found by walking up from the request file:

```toml
# environments/sit.toml
secrets = ["apiToken"]      # must appear before any [table]

[vars]
baseUrl = "https://sit.example.com"
testIdNumber = "8001015009087"
```

Secret values never touch the repo: each name in `secrets` is read from the
process environment as `RELAY_SECRET_<NAME>` (e.g. `apiToken` →
`RELAY_SECRET_APITOKEN`). A missing secret fails the run with the exact
variable name to export.

Variable precedence: request `[vars]` → folder/collection `[vars]` →
environment. Computed variables are always available: `{{$uuid}}`,
`{{$timestamp}}`, `{{$isoTimestamp}}`, `{{$randomInt}}`.

## Header inheritance

`collection.toml` at the collection root and `folder.toml` in subfolders
contribute headers and vars to every request beneath them; deeper files win,
and a request header overrides an inherited one. Setting a header to `""`
disables an inherited header for that request.

```toml
# collection.toml
name = "AML Demo"

[headers]
X-Correlation-Id = "{{$uuid}}"
```

## Assertions

| type       | fields                  | checks                          |
|------------|-------------------------|---------------------------------|
| `status`   | `equals`                | HTTP status code                |
| `jsonpath` | `path`, `equals`        | JSON value at path              |
| `header`   | `name`, `equals` / `contains` | response header           |
| `contains` | `contains`              | substring in body               |
| `max_ms`   | `max_ms`                | total request duration          |

## Data-driven runs

```sh
relay run my-collection --env sit --data ids.csv
```

The collection executes once per row; row values are the
highest-precedence variables and may appear in assertion expectations too
(`equals = "{{testIdNumber}}"`). CSV needs a header row; JSON is an array
of objects.

## Import & export

```sh
relay import postman collection.json --out my-collection   # Postman v2.x
relay export curl my-collection/02-verify.req.toml --env sit
relay export k6 my-collection --env sit --out load.js
relay export playwright my-collection --env sit --out api.spec.ts
```

Postman import maps folders to directories, requests to `.req.toml` files
(deterministic output — clean diffs), collection variables to
`collection.toml`, and bearer/basic/apikey auth.

The k6/Playwright exporters resolve plain variables inline but keep two
things dynamic in the generated script: secrets become `__ENV.RELAY_SECRET_*`
/ `process.env.RELAY_SECRET_*` references (no secret value ever lands in a
script), and computed variables become live expressions so every load-test
iteration gets a fresh `{{$uuid}}`. Folders map to k6 `group()`s and
Playwright `describe` blocks; assertions map to `check`s and `expect`s.

## Workbench UI: browser or native desktop

```sh
relay ui my-collection        # browser UI at http://127.0.0.1:7717
```

The same binary serves the full Relay workbench, backed by a local SQLite
database (`relay.db`): collections / folders / requests with structured
editors (params, auth, headers with inheritance origins, body, assertions,
vars), reusable **header presets** attachable to collections and folders
(secret-flagged values are stored locally, masked in the UI, and excluded
from exports), environments with `RELAY_SECRET_*` secrets, an in-app
**collection runner** with pass/fail summary cards and P95 latency, send
**history** with stored responses, per-request performance metrics, and
Postman import / k6 / Playwright export — all local-first, localhost-only.

On first launch in a directory of `.req.toml` files the database is seeded
from them automatically; `/api/export` (and the CLI porters) write the same
file format back out, so git and CI keep working on plain files while the
app works on SQLite.

The native desktop app (`relay-app`) wraps the identical UI in a system
webview via Wails v2 — WebView2 on Windows, WKWebView on macOS, WebKitGTK
on Linux. No Electron, no bundled browser, ~10MB binary.

```sh
# Linux: needs libgtk-3-dev libwebkit2gtk-4.1-dev to build
go build -tags desktop,production,webkit2_41 -o relay-app ./cmd/relay-app
# macOS / Windows
go build -tags desktop,production -o relay-app ./cmd/relay-app

relay-app --workspace my-collection   # or RELAY_WORKSPACE=... relay-app
```

## Project docs

- [Product Requirements (PRD)](docs/PRD.md)
- [Implementation Plan](docs/IMPLEMENTATION_PLAN.md)
- Example workspace: [`examples/aml-demo`](examples/aml-demo)

OpenAPI import/export, scripting (goja pm-shim), and the Xray adapter are
tracked in the implementation plan; the engine, CLI, exporters, and shared
UI here are the foundation they build on.
