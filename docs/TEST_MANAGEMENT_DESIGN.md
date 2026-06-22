# Relay Test Management Design

Relay's test management section should make API testing feel like a first-class QA workflow: author granular checks in the app, keep the test source in the Relay workspace, run the same tests from the UI or CLI, publish JUnit to Azure DevOps, and push traceable executions to Xray Cloud without glue scripts.

## Current Implementation State

This document is the target design and roadmap. The current implementation already includes a substantial UI workflow, but a few storage and CLI details differ from the target design below.

Implemented now:

- Left-rail **Test Management** view in the browser workbench and desktop app.
- Default UI test cases seeded from existing requests.
- Multiple test cases per request, stored in the local SQLite workspace database.
- Test folders and test sets.
- Test identity fields: name, enabled flag, tags, owner, priority, Xray key, requirements, and test plan key.
- Assertion and script-test execution for UI-managed tests.
- Last-run history and assertion/script step details.
- Xray Cloud settings, credential status, connection test, test validation, test creation, requirement linking, test set creation, and execution push through the UI/local API.
- CLI `relay run` for request-file assertions and scripts with JUnit or JSON output.

Current request-file metadata shape:

```toml
name = "Verify Individual"
method = "POST"
url = "{{baseUrl}}/aml/v2/verify"
tags = ["regression", "contract"]
owner = "qa"
priority = "high"
xray_key = "AML-T142"
requirements = ["AML-88"]
```

Not implemented yet:

- Nested `[meta.test]` / `[meta.xray]` request metadata.
- CLI selection flags such as `--select tag=regression`.
- CLI `--json-out` combined with JUnit in one run.
- Headless CLI Xray push flags such as `--xray-push`, `--xray-project`, and `--xray-auto-create`.
- Automatic write-back of UI-managed test case changes to `.req.toml` files as the canonical source of truth.

Until those items land, use `.req.toml` assertions/scripts for CI with `relay run`, and use the workbench for UI Test Management and Xray push.

## Goals

- Create granular API tests from saved Relay requests.
- Assert status code, response headers, response body values, response text, timing, and script-based `pm.test` checks.
- Store tests locally in Relay request files so git remains the source of truth.
- Link tests to Jira/Xray test issues and requirement/story issues.
- Create missing Xray tests from Relay when needed, then write the key back to the request metadata.
- Run a collection, folder, selected tests, or tagged subset from the UI and the CLI.
- Publish results to Azure DevOps using JUnit and optionally push the same run to Xray Cloud.

## Primary User Flow

1. The user opens **Test Management** from the left rail.
2. Relay shows test suites grouped by collection and folder.
3. The user selects a request and adds checks in a structured test builder:
   - Status: `equals 200`, `is 2xx`, `is one of [200, 201]`.
   - Header: `Content-Type contains application/json`, `X-Trace-Id exists`.
   - JSON body: `$.result.status equals VERIFIED`, `$.items length > 0`.
   - Text body: `contains success` or `does not contain error`.
   - Timing: `total < 2000ms`.
   - Script: advanced `pm.test` blocks for custom logic.
4. The user links the test to Xray:
   - Existing test key, for example `AML-T142`.
   - Requirement/story links, for example `AML-88`, `AML-91`.
   - Optional labels, component, priority, test plan.
5. Relay saves the metadata and assertions in the `.req.toml` file.
6. The user runs selected tests from the Test Management view or runs the whole collection.
7. Relay shows pass/fail details and offers **Push to Xray**.
8. In CI, `relay run` runs the same stored tests, publishes JUnit to Azure DevOps, and optionally pushes the execution to Xray.

## UI Section Design

Add a new left-rail section:

```text
Test Management
```

### Test Management Layout

Use a dense workbench layout, not a marketing page.

- Left pane: suite tree.
  - Collections
  - Folders
  - Requests/tests
  - Filter chips: `Failed last run`, `No Xray link`, `Has assertions`, `Tagged`, `Requirement`
- Main pane: selected test editor.
  - Test identity panel
  - Assertion builder
  - Xray/Jira traceability panel
  - Last run result panel
- Right pane: run controls and metadata.
  - Environment selector
  - Run selected
  - Run folder
  - Run collection
  - Push to Xray
  - Export JUnit/JSON

### Test Identity Panel

Fields:

- Test name: defaults to request name.
- Stable test id: generated Relay id for local tracking.
- Tags: smoke, regression, contract, negative, etc.
- Owner/component.
- Priority.
- Enabled/disabled.

### Assertion Builder

Keep the current `[[assertions]]` model, but make the UI granular and explicit.

Assertion rows:

| Type | UI fields | Stored shape |
|---|---|---|
| Status | operator, expected code/range/list | `type = "status"`, `operator`, `equals` or `values` |
| Header | header name, operator, expected value | `type = "header"`, `name`, `operator`, `equals` / `contains` |
| JSON body | JSONPath, operator, expected value | `type = "jsonpath"`, `path`, `operator`, `equals` |
| Text body | operator, expected text | `type = "contains"` / `not_contains` |
| Timing | metric, operator, threshold ms | `type = "max_ms"` |
| Script | Code editor | `[scripts].tests` |

The current assertion types already support the MVP: `status`, `jsonpath`, `header`, `contains`, and `max_ms`. The design should extend them with `operator` later rather than replacing the model.

### Xray/Jira Traceability Panel

Fields:

- Xray project key: inherited from workspace settings.
- Xray test key: existing issue key, for example `AML-T142`.
- Requirement/story links: `AML-88`, `AML-91`.
- Test plan key: inherited default, overridable per run.
- Create/link actions:
  - **Link existing test** validates that the key exists in Xray.
  - **Create Xray test** creates a Jira/Xray test issue from the Relay test name and assertions.
  - **Link requirements** adds requirement/story links in Jira/Xray.
  - **Sync metadata** refreshes summary/status/links from Xray.

## Relay Storage Model

Relay tests continue to live inside request files. SQLite can cache history and last-run state, but the source of truth remains the workspace.

Recommended `.req.toml` extension:

```toml
name = "Verify Individual"
method = "POST"
url = "{{baseUrl}}/aml/v2/verify"

[meta.test]
id = "relay-verify-individual"
tags = ["regression", "contract"]
priority = "high"
enabled = true

[meta.xray]
test_key = "AML-T142"
requirements = ["AML-88"]
labels = ["relay", "api"]
component = "AML"

[[assertions]]
type = "status"
equals = 200

[[assertions]]
type = "header"
name = "Content-Type"
contains = "application/json"

[[assertions]]
type = "jsonpath"
path = "$.result.status"
equals = "VERIFIED"
```

Implementation note: add `Meta *Meta` to `dsl.Request`, with `Meta.Test` and `Meta.Xray` structs. Preserve backwards compatibility by treating missing metadata as an enabled unlinked test.

## Runner Model

The existing runner already executes requests and evaluates assertions. Extend it from request-level results to test-management-aware results.

### Selection Inputs

Support these run scopes:

- Collection id/path.
- Folder id/path.
- Explicit request ids/files.
- Tag expression, for example `tag=smoke` or `tag=regression,!flaky`.
- Xray test key list.
- Requirement key list.

### Result Shape

Every result should include local and external identity:

```json
{
  "testId": "relay-verify-individual",
  "xrayTestKey": "AML-T142",
  "requirementKeys": ["AML-88"],
  "name": "Verify Individual",
  "file": "collections/aml/verify-individual.req.toml",
  "method": "POST",
  "url": "https://sit.example.com/aml/v2/verify",
  "status": 200,
  "durationMs": 184,
  "passed": true,
  "assertions": [
    { "type": "status", "passed": true, "message": "status is 200" }
  ],
  "scriptTests": []
}
```

JUnit output should emit each assertion/script test as a separate testcase when present, which the current reporter already does. Add Xray keys and Relay IDs as testcase properties so downstream tools can correlate results.

## Xray Integration Design

Use the existing `internal/adapters/tm` interface and extend the Xray adapter.

### Xray Settings

Keep credentials in environment variables only:

- `RELAY_XRAY_CLIENT_ID`
- `RELAY_XRAY_CLIENT_SECRET`

Store non-secret settings in Relay:

- Project key.
- Default test plan key.
- Optional Cloud URL override.
- Default issue type for created tests.
- Optional labels/component defaults.

### Adapter Capabilities

Current adapter:

- Authenticate with Xray Cloud.
- Push a test execution from normalized results.

Required adapter additions:

- `GetTest(key)` to validate linked tests.
- `CreateTest(input)` to create Xray test issues from Relay tests.
- `UpdateTest(key, input)` to sync summary/labels if requested.
- `LinkRequirements(testKey, requirementKeys)` to connect tests to Jira issues.
- `ImportExecution(exec)` or extend `PushExecution` to include evidence/comments/assertion detail.

### Push Execution Behavior

When pushing a run:

1. For every result, read `meta.xray.test_key`.
2. If a test key exists, push the result against that Xray test.
3. If no key exists and auto-create is enabled:
   - Create the Xray test.
   - Link requirements.
   - Save the new key back to the Relay request file.
   - Push the result against the new key.
4. If no key exists and auto-create is disabled:
   - Push by summary only if Xray auto-provision is enabled.
   - Otherwise mark the result as skipped from Xray push and show it in the UI.
5. Create a Test Execution with summary, start/finish time, environment, test plan, and all mapped results.
6. Return the execution key and per-test push status.

## CLI Design

Keep `relay run` as the single runner for local, UI, and CI usage.

Recommended flags:

```bash
relay run collections/aml \
  --env sit \
  --select tag=regression \
  --report junit \
  --out relay-results.xml \
  --json-out relay-results.json \
  --xray-push \
  --xray-project AML \
  --xray-test-plan AML-88 \
  --xray-summary "SIT regression $(Build.BuildNumber)" \
  --xray-auto-create
```

Flags:

- `--select`: filter by folder, request name, tag, xray key, requirement key.
- `--report junit|json`: existing report output.
- `--out`: existing report file.
- `--json-out`: write a machine-readable result file in addition to JUnit.
- `--xray-push`: push results after the run.
- `--xray-project`: project key override.
- `--xray-test-plan`: test plan key override.
- `--xray-summary`: execution summary.
- `--xray-auto-create`: create missing Xray tests and write keys back to Relay files.
- `--xray-dry-run`: validate mapping without pushing.

Exit codes:

- `0`: run completed and all tests passed.
- `1`: run completed with failed tests.
- `2`: invalid CLI usage/configuration.
- `3`: tests ran, but Xray push failed.

## Azure DevOps Integration

Azure DevOps should consume the same Relay tests in two ways:

1. Publish JUnit to Azure Test Plans using `PublishTestResults@2`.
2. Push the execution to Xray Cloud directly from `relay run --xray-push`.

Recommended pipeline:

```yaml
trigger:
  - main

pool:
  vmImage: ubuntu-latest

variables:
  RELAY_RESULTS_XML: '$(Build.ArtifactStagingDirectory)/relay-results.xml'
  RELAY_RESULTS_JSON: '$(Build.ArtifactStagingDirectory)/relay-results.json'

steps:
  - checkout: self

  - task: GoTool@0
    inputs:
      version: '1.25'

  - script: go install github.com/muhaymien96/relay/cmd/relay@latest
    displayName: Install relay

  - script: |
      relay run collections/aml \
        --env sit \
        --select tag=regression \
        --report junit \
        --out "$(RELAY_RESULTS_XML)" \
        --json-out "$(RELAY_RESULTS_JSON)" \
        --xray-push \
        --xray-project AML \
        --xray-test-plan AML-88 \
        --xray-summary "Relay SIT regression $(Build.BuildNumber)"
    displayName: Run Relay tests and push Xray execution
    env:
      RELAY_SECRET_APIKEY: $(RELAY_SECRET_APIKEY)
      RELAY_SECRET_BEARER_TOKEN: $(RELAY_SECRET_BEARER_TOKEN)
      RELAY_XRAY_CLIENT_ID: $(RELAY_XRAY_CLIENT_ID)
      RELAY_XRAY_CLIENT_SECRET: $(RELAY_XRAY_CLIENT_SECRET)

  - task: PublishTestResults@2
    displayName: Publish Relay JUnit results
    inputs:
      testResultsFormat: JUnit
      testResultsFiles: '$(RELAY_RESULTS_XML)'
      failTaskOnFailedTests: true
      testRunTitle: 'Relay SIT regression'
    condition: always()

  - publish: '$(Build.ArtifactStagingDirectory)'
    artifact: relay-results
    condition: always()
```

Until the CLI Xray push flags exist, the fallback is:

- Run `relay run --report junit --out relay-results.xml`.
- Run `relay run --report json --out relay-results.json`.
- Use a small script or future `relay xray push relay-results.json` command to import the JSON report to Xray.

The UI API sidecar approach should not be the long-term CI path because headless pipelines should not need to start the browser UI server.

## Implementation Slices

### Slice 1: Metadata and Selection

- Add `Meta` to `dsl.Request`.
- Add parser/marshal tests for `[meta.test]` and `[meta.xray]`.
- Add runner filters for tags, requirements, Xray keys, and enabled/disabled.

### Slice 2: Test Management UI

- Add `Test Management` nav item.
- Add suite tree with filters.
- Add assertion builder with status/header/jsonpath/body/timing rows.
- Add Xray traceability panel.
- Reuse existing request save/send/run APIs where possible.

### Slice 3: Xray Adapter Expansion

- Add Xray test lookup/create/link methods.
- Add dry-run mapping endpoint.
- Add push result response with per-test status.
- Persist newly created Xray keys back to request files/store.

### Slice 4: CLI and CI

- Add `--select`, `--json-out`, and Xray flags to `relay run`.
- Add JUnit testcase properties.
- Update `docs/azure-devops-pipeline.yml` to use CLI Xray push.
- Add CI examples using Azure DevOps variable groups.

### Slice 5: Hardening

- Contract tests with recorded Xray GraphQL responses.
- Unit tests for metadata round-trip and selection filters.
- Pipeline smoke fixture proving JUnit and Xray JSON mapping.
- UI tests for assertion editing and Xray link validation.

## Acceptance Criteria

- A user can create a request test with status/header/body assertions from the UI.
- The test is stored in the Relay workspace and can be reviewed in git.
- A user can link the test to an existing Xray test key and Jira requirement keys.
- A user can create a missing Xray test from Relay and save the returned key locally.
- A user can run selected tests, folders, or collections from the UI.
- `relay run` can execute the same tests in Azure DevOps and publish JUnit.
- The CLI can push run results to Xray without starting the UI server.
- Failed assertions show enough detail in Relay, Azure DevOps, and Xray to diagnose the failure.
- Secrets are never written to request files, reports, Xray comments, or logs.
