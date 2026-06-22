# Relay API Client Guide

This guide explains daily usage of Relay's API client in both desktop and browser UI.

## 1. Create and Organize Requests

1. Open the Collections view.
2. Create a collection with the + button.
3. Add requests in two ways:
- Top New Request button (adds to current collection).
- Right-click a folder and choose New request in folder.
4. Move existing requests with right-click on a request: Move to folder.

## 2. Environment Variables and Secrets

Relay supports placeholders in this format:

{{variableName}}

You can use placeholders in:
- Request URL
- Request headers
- Request body
- Auth fields (for example bearer token)
- Assertions and scripts

### 2.1 Where variables come from

Variable resolution order (highest to lowest priority):
1. Request vars
2. Folder vars
3. Collection vars
4. Selected environment vars/secrets

### 2.2 Environment setup

1. Go to Environments view.
2. Create an environment (for example local or sit).
3. Add vars like:
- baseUrl = api.example.com
- customerId = 12345
4. Add secret names (for example apiToken).
5. Set process environment variables before starting Relay:
- RELAY_SECRET_APITOKEN=your-token

## 3. Using Variables in URL, Headers, and Body

### 3.1 URL examples

https://{{baseUrl}}/v1/customers/{{customerId}}

### 3.2 Header examples

Authorization: Bearer {{apiToken}}
X-Correlation-Id: {{$uuid}}

### 3.3 Body examples (JSON)

{
  "customerId": "{{customerId}}",
  "channel": "web"
}

## 4. Assertions (Validation Rules)

Open the Assertions tab and add rules such as:
- status equals 200
- jsonpath $.result.status equals VERIFIED
- header Content-Type contains application/json
- contains success
- max_ms 2000

Tips:
- Keep status assertion on every request.
- Add at least one body/content assertion for business validation.

## 5. Scripts (Pre-request and Tests)

Open the Scripts tab and use either:
- Pre-request (runs before request send)
- Tests (runs after response)

### 5.1 Pre-request example

pm.environment.set("requestId", "req-" + Date.now());

### 5.2 Tests examples

pm.test("Status is 200", function () {
  pm.expect(pm.response.code).to.equal(200);
});

pm.test("Body includes success", function () {
  pm.expect(pm.response.text()).to.include("success");
});

pm.test("Capture token", function () {
  var j = pm.response.json();
  pm.environment.set("token", j.token);
});

## 6. Save and Send Workflow

1. Edit request fields.
2. Click Save (or Ctrl+S / Cmd+S).
3. Click Send.
4. Review:
- Response body/headers/timing
- Assertion chips
- Script test results

## 7. Running Folders and Collections

### 7.1 Folder run

Right-click a folder and choose Run folder.
- Runs all requests in that folder.
- Uses currently selected environment.
- Shows pass/fail summary.

### 7.2 Collection run

Right-click a collection and choose Run collection.
- Runs all requests in the collection.
- Includes assertion and script outcomes.

## 8. Test Management and Xray

Relay tests are saved as part of each request, so the same checks run in the UI, CLI, and CI pipelines.

Use assertions for granular validation:
- Status code checks, for example status equals 200.
- Response header checks, for example Content-Type contains application/json.
- JSON response checks, for example $.result.status equals VERIFIED.
- Body text checks, for example response contains success.
- Timing checks, for example total response time is under 2000ms.

For advanced checks, use Scripts -> Tests with pm.test blocks.

Xray/Jira traceability is configured from Settings -> Xray Cloud. Credentials stay in environment variables:
- RELAY_XRAY_CLIENT_ID
- RELAY_XRAY_CLIENT_SECRET

The detailed Test Management design, including Jira issue links, Xray test creation, selected test runs, and Azure DevOps pipeline behavior, lives in [TEST_MANAGEMENT_DESIGN.md](TEST_MANAGEMENT_DESIGN.md).

## 9. Troubleshooting

### Variables not resolving
- Confirm environment is selected in dropdown.
- Confirm variable exists in request/folder/collection/environment.
- For secrets, confirm RELAY_SECRET_* variable is set before app launch.

### Script tests not showing
- Ensure script is in Scripts -> Tests (not Pre-request).
- Send request again after saving.

### Folder appears empty
- Right-click folder and choose New request in folder.
- Or move existing requests into that folder.
