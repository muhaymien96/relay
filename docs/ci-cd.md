# Relay CI/CD Integration Guide

Relay's CLI runner (`relay run`) outputs JUnit XML and JSON reports, making it
a drop-in test runner in any CI/CD pipeline. No server, no cloud account, no
configuration beyond environment variables for secrets.

---

## GitHub Actions

The repository ships a workflow at `.github/workflows/ci.yml` that runs the
relay CLI's own integration tests via `relay run`. Use the same pattern for
your collections:

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

      - name: Publish results
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

Relay can push test execution results directly to Xray Cloud after a
collection run.

### Configuration

1. In the Relay UI → **Settings → Xray Cloud**, set:
   - **Project key** (e.g. `AML`)
   - **Test plan key** (optional, e.g. `AML-88`)

2. Set credentials as environment variables (never stored):
   ```bash
   export RELAY_XRAY_CLIENT_ID=your-client-id
   export RELAY_XRAY_CLIENT_SECRET=your-client-secret
   ```
   Get credentials from Xray Cloud → API Keys.

3. Click **Push to Xray** in the runner view after a collection run.

### Request-level traceability

Link a request to an Xray test case by adding a `[scripts]` section with
the test key in the `.req.toml` file, or via the meta table (future):

```toml
# collections/aml/verify-individual.req.toml
name = "Verify Individual"
method = "POST"
url = "{{baseUrl}}/aml/v2/verify"

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

### CI pipeline with Xray push

For fully headless CI pushes, use the relay UI API from a script:

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

Or use the JSON report to drive a custom Xray push via their REST API.

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
[vars]
baseUrl = "https://sit.example.com"

secrets = ["apiKey", "bearerToken"]
```

Maps to environment variables:
```
RELAY_SECRET_APIKEY=your-api-key
RELAY_SECRET_BEARERTOKEN=your-token
```

Export them in your CI pipeline's secret store and inject them as environment
variables — they're never stored in files or the relay database.
