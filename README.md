# Relay

A fast, lightweight, local-first API client. Collections are plain TOML files
in your repo — branch, diff, review, and merge them like any other code. The
same engine drives interactive use and headless CI runs.

Single static binary (~6MB), no Electron, no account, no cloud, no telemetry.

```
relay send  <file.req.toml>  [--env NAME] [-v]
relay run   <dir>            [--env NAME] [--report junit|json] [--out FILE] [--bail] [--delay 200ms]
relay import postman <collection.json> [--out DIR]
relay export curl <file.req.toml> [--env NAME]
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

## Import & export

```sh
relay import postman collection.json --out my-collection   # Postman v2.x
relay export curl my-collection/02-verify.req.toml --env sit
```

Postman import maps folders to directories, requests to `.req.toml` files
(deterministic output — clean diffs), collection variables to
`collection.toml`, and bearer/basic/apikey auth.

## Project docs

- [Product Requirements (PRD)](docs/PRD.md)
- [Implementation Plan](docs/IMPLEMENTATION_PLAN.md)
- Example workspace: [`examples/aml-demo`](examples/aml-demo)

The desktop app (Wails), k6/Playwright export, OpenAPI, scripting, and the
Xray adapter are tracked in the implementation plan; the CLI and engine here
are the foundation they share.
