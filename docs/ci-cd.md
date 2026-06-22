# Relay CI/CD Integration Guide

Relay's CLI runner (`relay run`) outputs JUnit XML or JSON reports, making it a drop-in API test runner in CI/CD pipelines. It runs directly from `.req.toml` files, so CI does not need the browser workbench, desktop app, SQLite database, account, or cloud sync.

Current CLI scope:

- Runs every `*.req.toml` file under a directory in lexical order.
- Resolves `environments/<name>.toml` files and `RELAY_SECRET_*` secrets.
- Supports CSV/JSON data-driven runs with `--data`.
- Supports `--delay`, `--bail`, `--timeout`, `--insecure`, and `--no-redirect`.
- Writes one report format per run with `--report junit|json --out FILE`.
- Exits with code `1` when any request, assertion, or script test fails.
- Does not yet implement `--select`, `--json-out`, or Xray push flags.

---

## GitHub Actions

Use this pattern for API collections:

```yaml
# .github/workflows/api-tests.yml
name: API Tests

on:
  push:
    branches: [main]
  pull_request:

jobs:
  relay:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'

      - name: Install relay
        run: go install github.com/muhaymien96/relay/cmd/relay@latest

      - name: Run collection
        run: |
          relay run collections/regression \
            --env sit \
            --report junit \
            --out results.xml \
            --timeout 30s
        env:
          RELAY_SECRET_APIKEY: ${{ secrets.RELAY_SECRET_APIKEY }}

      - name: Publish JUnit results
        uses: mikepenz/action-junit-report@v4
        if: always()
        with:
          report_paths: results.xml
```

**Secrets:** any variable listed under `secrets` in an environment file is
read from `RELAY_SECRET_<NAME>`. Map them in your GitHub repo secrets
(Settings → Secrets and variables → Actions).

---

## Azure DevOps

A reusable pipeline template lives at `docs/azure-devops-pipeline.yml`. The
minimum inline setup is:

```yaml
# azure-pipelines.yml
trigger:
  - main

pool:
  vmImage: ubuntu-latest

steps:
  - task: GoTool@0
    inputs:
      version: '1.25'

  - script: go install github.com/muhaymien96/relay/cmd/relay@latest
    displayName: Install relay

  - script: |
      relay run collections/regression \
        --env sit \
        --report junit \
        --out $(Build.ArtifactStagingDirectory)/relay-results.xml
    displayName: Run Relay collection
    env:
      RELAY_SECRET_APIKEY: $(RELAY_SECRET_APIKEY)  # set in Pipeline Library

  - task: PublishTestResults@2
    displayName: Publish to Azure Test Plans
    inputs:
      testResultsFormat: JUnit
      testResultsFiles: '$(Build.ArtifactStagingDirectory)/relay-results.xml'
      failTaskOnFailedTests: true
      testRunTitle: 'Relay regression'
    condition: always()
```

**Pipeline library secrets:** add `RELAY_SECRET_*` variables in
Pipelines → Library → Variable Groups, then link the group to your pipeline.

---

## Jira / Xray Cloud

Relay can push Test Management runs to Xray Cloud from the UI/local API. The headless `relay run` CLI does not yet have Xray push flags.

### Configuration

1. In the Relay UI Settings view, set the project key, optional test plan key, optional Cloud GraphQL/auth URL overrides, and optional default labels/component.

2. Prefer credentials from environment variables: `RELAY_XRAY_CLIENT_ID` and `RELAY_XRAY_CLIENT_SECRET`. Get credentials from Xray Cloud -> API Keys. The UI can also save credentials into the local `relay.db` for a workstation-only setup.

3. In Test Management, run selected tests and click **Push to Xray**.

### Request-level traceability

The current request file metadata fields are top-level fields:

```toml
# collections/aml/verify-individual.req.toml
name = "Verify Individual"
method = "POST"
url = "{{baseUrl}}/aml/v2/verify"
tags = ["regression"]
xray_key = "AML-T142"
requirements = ["AML-88"]

[[assertions]]
type = "status"
equals = 200

[scripts]
tests = '''
pm.test("result is VERIFIED", function() {
  var j = pm.response.json();
  pm.expect(j.result.status).to.equal("VERIFIED");
});
'''
```

When the UI seeds default Test Management cases from request files, it copies `tags`, `owner`, `priority`, `xray_key`, `requirements`, assertions, and test scripts into the SQLite test-case records.

### CI pipeline with Xray push

The target CI path is a future headless CLI push from `relay run`, so pipelines should not depend on starting the browser UI server long-term. The design is documented in [TEST_MANAGEMENT_DESIGN.md](TEST_MANAGEMENT_DESIGN.md).

Roadmap CLI shape:

```bash
relay run collections/regression \
  --env sit \
  --report junit \
  --out relay-results.xml \
  --json-out relay-results.json \
  --xray-push \
  --xray-project AML \
  --xray-test-plan AML-88 \
  --xray-summary "Relay SIT regression"
```

Until those Xray CLI flags are implemented, use the Relay UI API from a
short-lived local sidecar only as a temporary workaround. This requires a seeded `relay.db` and Xray settings/credentials available to the UI server:

```bash
#!/usr/bin/env bash
# xray-push.sh — run after relay ui is started on a sidecar port

COLLECTION_ID=$1
ENV_NAME=${2:-sit}
RELAY_URL=${RELAY_URL:-http://127.0.0.1:7717}

curl -sf -X POST "$RELAY_URL/api/xray/push" \
  -H "Content-Type: application/json" \
  -d "{\"collectionId\":$COLLECTION_ID,\"env\":\"$ENV_NAME\"}" \
  | jq -r '.executionKey'
```

Or use the JSON report to drive a custom Xray push via Xray's API.

---

## GitLab CI

```yaml
# .gitlab-ci.yml
relay-tests:
  image: golang:1.25
  stage: test
  before_script:
    - go install github.com/muhaymien96/relay/cmd/relay@latest
  script:
    - relay run collections/regression --env sit --report junit --out relay-results.xml
  artifacts:
    when: always
    reports:
      junit: relay-results.xml
  variables:
    RELAY_SECRET_APIKEY: $RELAY_SECRET_APIKEY
```

---

## MCP Server (AI agent integration)

Relay exposes its workspace via an HTTP API (`relay ui`) that can be accessed
by MCP-compatible AI agents (Claude Code, Cursor, etc.) for:

- **Authoring requests** — POST to `/api/requests` to create requests
  programmatically from an OpenAPI spec or natural-language description
- **Running collections** — POST to `/api/run` and inspect results
- **Pushing to Xray** — POST to `/api/xray/push` with collection + env

Example Claude Code usage with relay running on port 7717:

```
Use the Relay API at http://localhost:7717 to:
1. Import the OpenAPI spec at ./api-spec.json
2. Run the imported collection against the sit environment
3. Show me which tests failed
```

The relay UI API is fully local — no authentication, no cloud, no telemetry.

---

## Environment secrets in CI

All secrets use the `RELAY_SECRET_<NAME>` convention. The name corresponds
to the secret variable name in your environment TOML file:

```toml
# environments/sit.toml
secrets = ["apiKey", "bearerToken"]

[vars]
baseUrl = "https://sit.example.com"
```

Maps to environment variables:
```
RELAY_SECRET_APIKEY=your-api-key
RELAY_SECRET_BEARERTOKEN=your-token
```

Export them in your CI pipeline's secret store and inject them as environment
variables. Environment secret values are not stored in request files.
