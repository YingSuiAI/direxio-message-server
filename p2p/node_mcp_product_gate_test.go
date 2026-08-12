package p2p_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	nodeMCPGatePackage = "mcp-mx-calculator"
	nodeMCPGateVersion = "1.0.1"
	nodeMCPGateTool    = "add"
)

// TestManagedNodeMCPProductGate is an opt-in, black-box release gate. It must
// point at a real split stack: the request enters through Message Server's
// owner-authenticated ProductCore HTTP route, crosses its Agent gateway, and
// is executed by the external Agent, PostgreSQL, scheduler, and offline
// extension runner. This test never constructs an in-process Agent or a mock.
//
// Required environment:
//
//	DIREXTALK_TEST_NODE_MCP_PRODUCT_GATE=1
//	DIREXTALK_TEST_PRODUCT_BASE_URL=https://message-server.example
//	DIREXTALK_TEST_OWNER_TOKEN_FILE=/run/secrets/owner-access-token
//
// DIREXTALK_TEST_PRODUCT_CA_FILE may name a private CA. Plain HTTP is accepted
// only for a loopback address, so an owner token cannot be sent to a remote
// clear-text endpoint accidentally. Secret values are never placed in argv or
// test output.
func TestManagedNodeMCPProductGate(t *testing.T) {
	if os.Getenv("DIREXTALK_TEST_NODE_MCP_PRODUCT_GATE") != "1" {
		t.Skip("DIREXTALK_TEST_NODE_MCP_PRODUCT_GATE=1 not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client := newNodeMCPProductClient(t)

	discovered := client.action(ctx, false, "agent.core.mcp.discover", map[string]any{
		"source": "npm", "query": nodeMCPGatePackage, "page_size": 10,
	})
	candidate := exactCandidate(t, discovered, nodeMCPGatePackage, nodeMCPGateVersion)
	if candidate["source"] != "npm" || candidate["transport"] != "stdio_node" {
		t.Fatalf("discovered candidate source/transport = %v/%v, want npm/stdio_node", candidate["source"], candidate["transport"])
	}
	pin := objectField(t, candidate, "pin")
	if pin["registry_version"] != nodeMCPGateVersion || !lowerSHA256(stringField(pin, "registry_sha256")) || stringField(pin, "git_commit") != "" || stringField(pin, "git_sha256") != "" {
		t.Fatal("discovered npm candidate did not contain only its exact immutable registry pin")
	}

	inspected := client.action(ctx, false, "agent.core.mcp.inspect", map[string]any{"candidate": candidate})
	inspection := objectField(t, inspected, "inspection")
	assertNodeInspection(t, inspection, candidate)

	installRequest := map[string]any{
		"idempotency_key": uuid.NewString(),
		"candidate":       candidate,
		"inspection":      inspection,
		"secret_inputs":   []any{},
	}
	install := client.action(ctx, true, "agent.core.mcp.install", installRequest)
	installationID := requiredString(t, objectField(t, install, "installation"), "id")
	confirmOperation(t, ctx, client, requiredString(t, install, "confirmation_id"))
	waitTaskSucceeded(t, ctx, client, requiredString(t, install, "task_id"))

	installed := objectField(t, client.action(ctx, false, "agent.core.mcp.get", map[string]any{"installation_id": installationID}), "installation")
	revision := assertInstalledNodeReceipt(t, installed)

	toolsResult := client.action(ctx, false, "agent.core.mcp.list_tools", map[string]any{
		"installation_id": installationID, "expected_revision": revision,
	})
	tool := exactTool(t, toolsResult, nodeMCPGateTool)
	assertCalculatorToolSchema(t, tool)

	execute := client.action(ctx, true, "agent.core.mcp.execute", map[string]any{
		"idempotency_key": uuid.NewString(), "installation_id": installationID,
		"expected_revision": revision, "tool_name": nodeMCPGateTool,
		"input": map[string]any{"a": 2, "b": 3},
	})
	confirmOperation(t, ctx, client, requiredString(t, execute, "confirmation_id"))
	executionTask := waitTaskSucceeded(t, ctx, client, requiredString(t, execute, "task_id"))
	assertSuccessfulMCPResult(t, executionTask, "5")

	latest := objectField(t, client.action(ctx, false, "agent.core.mcp.get", map[string]any{"installation_id": installationID}), "installation")
	removeRequest := map[string]any{
		"idempotency_key": uuid.NewString(), "installation_id": installationID,
		"expected_revision": requiredPositiveInteger(t, latest, "revision"),
	}
	removedStart := client.action(ctx, true, "agent.core.mcp.remove", removeRequest)
	replayedStart := client.action(ctx, true, "agent.core.mcp.remove", removeRequest)
	if !reflect.DeepEqual(removedStart, replayedStart) {
		t.Fatal("exact uninstall replay did not return the original public operation result")
	}
	confirmOperation(t, ctx, client, requiredString(t, removedStart, "confirmation_id"))
	waitTaskSucceeded(t, ctx, client, requiredString(t, removedStart, "task_id"))

	removed := objectField(t, client.action(ctx, false, "agent.core.mcp.get", map[string]any{"installation_id": installationID}), "installation")
	if removed["state"] != "removed" || stringField(removed, "active_version_id") != "" {
		t.Fatalf("final installation state = %v with active version present=%t, want removed/absent", removed["state"], stringField(removed, "active_version_id") != "")
	}
	finalReplay := client.action(ctx, true, "agent.core.mcp.remove", removeRequest)
	if !reflect.DeepEqual(removedStart, finalReplay) {
		t.Fatal("exact uninstall replay drifted after the durable removal completed")
	}

	t.Logf("managed Node MCP ProductCore gate passed: package=%s version=%s receipt_fields=8 tool=%s result=valid uninstall=replayed", nodeMCPGatePackage, nodeMCPGateVersion, nodeMCPGateTool)
}

type nodeMCPProductClient struct {
	t       *testing.T
	baseURL *url.URL
	token   string
	http    *http.Client
}

func newNodeMCPProductClient(t *testing.T) *nodeMCPProductClient {
	t.Helper()
	rawURL := strings.TrimSpace(os.Getenv("DIREXTALK_TEST_PRODUCT_BASE_URL"))
	if rawURL == "" {
		t.Fatal("DIREXTALK_TEST_PRODUCT_BASE_URL is required")
	}
	baseURL, err := url.Parse(rawURL)
	if err != nil || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		t.Fatal("DIREXTALK_TEST_PRODUCT_BASE_URL must be an absolute URL without user info, query, or fragment")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	if baseURL.Scheme != "https" && !(baseURL.Scheme == "http" && loopbackHost(baseURL.Hostname())) {
		t.Fatal("ProductCore gate requires HTTPS, except for an explicit loopback HTTP endpoint")
	}

	tokenFile := strings.TrimSpace(os.Getenv("DIREXTALK_TEST_OWNER_TOKEN_FILE"))
	if !filepath.IsAbs(tokenFile) {
		t.Fatal("DIREXTALK_TEST_OWNER_TOKEN_FILE must be an absolute path")
	}
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read owner token file: %v", err)
	}
	rawToken := string(tokenBytes)
	token := strings.TrimSuffix(rawToken, "\r\n")
	token = strings.TrimSuffix(token, "\n")
	if token == "" || strings.TrimSpace(token) != token || (rawToken != token && rawToken != token+"\n" && rawToken != token+"\r\n") {
		t.Fatal("owner token file is empty or malformed")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile := strings.TrimSpace(os.Getenv("DIREXTALK_TEST_PRODUCT_CA_FILE")); caFile != "" {
		if !filepath.IsAbs(caFile) {
			t.Fatal("DIREXTALK_TEST_PRODUCT_CA_FILE must be an absolute path")
		}
		pem, readErr := os.ReadFile(caFile)
		if readErr != nil {
			t.Fatalf("read ProductCore CA file: %v", readErr)
		}
		roots, rootErr := x509.SystemCertPool()
		if rootErr != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("ProductCore CA file contains no valid certificates")
		}
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = new(tls.Config)
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &nodeMCPProductClient{
		t: t, baseURL: baseURL, token: token,
		http: &http.Client{Transport: transport, Timeout: 3 * time.Minute},
	}
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	return net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (c *nodeMCPProductClient) action(ctx context.Context, mutation bool, action string, params map[string]any) map[string]any {
	c.t.Helper()
	path := "/_p2p/query"
	if mutation {
		path = "/_p2p/command"
	}
	body, err := json.Marshal(map[string]any{"action": action, "params": params})
	if err != nil {
		c.t.Fatalf("encode ProductCore action %s: %v", action, err)
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("build ProductCore action %s: %v", action, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("ProductCore action %s transport failed: %v", action, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		c.t.Fatalf("ProductCore action %s response read failed: %v", action, err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		c.t.Fatalf("ProductCore action %s returned invalid JSON (status %d)", action, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		c.t.Fatalf("ProductCore action %s failed: status=%d code=%q error=%q", action, response.StatusCode, stringField(result, "code"), stringField(result, "error"))
	}
	return result
}

func exactCandidate(t *testing.T, result map[string]any, packageName, version string) map[string]any {
	t.Helper()
	for _, raw := range arrayField(t, result, "candidates") {
		candidate, ok := raw.(map[string]any)
		if !ok || candidate["id"] != packageName {
			continue
		}
		pin, ok := candidate["pin"].(map[string]any)
		if ok && pin["registry_version"] == version {
			return candidate
		}
	}
	t.Fatalf("npm discovery did not return the exact pinned package %s@%s", packageName, version)
	return nil
}

func assertNodeInspection(t *testing.T, inspection, candidate map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(inspection["candidate"], candidate) {
		t.Fatal("inspection candidate differs from the discovered immutable candidate")
	}
	execution := objectField(t, inspection, "execution")
	stdio := objectField(t, execution, "stdio")
	if stdio["runtime"] != "node" || requiredString(t, stdio, "relative_path") == "" {
		t.Fatalf("inspection stdio runtime/path = %v/%v, want node/non-empty", stdio["runtime"], stdio["relative_path"])
	}
	if len(arrayField(t, inspection, "network_grants")) != 0 || len(arrayField(t, inspection, "secret_grants")) != 0 {
		t.Fatal("managed Node acceptance package unexpectedly requested network or secret grants")
	}
	if _, submittedReceipt := inspection["node_artifact"]; submittedReceipt {
		t.Fatal("inspection accepted a client-authored Node receipt")
	}
}

func confirmOperation(t *testing.T, ctx context.Context, client *nodeMCPProductClient, confirmationID string) {
	t.Helper()
	confirmation := objectField(t, client.action(ctx, false, "agent.core.confirmations.get", map[string]any{"confirmation_id": confirmationID}), "confirmation")
	if confirmation["state"] != "pending" {
		t.Fatalf("confirmation %s state = %v, want pending", confirmationID, confirmation["state"])
	}
	client.action(ctx, true, "agent.core.confirmations.confirm", map[string]any{
		"idempotency_key": uuid.NewString(), "confirmation_id": confirmationID,
		"expected_revision": requiredPositiveInteger(t, confirmation, "revision"),
	})
}

func waitTaskSucceeded(t *testing.T, ctx context.Context, client *nodeMCPProductClient, taskID string) map[string]any {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		result := client.action(ctx, false, "agent.core.tasks.get", map[string]any{"task_id": taskID})
		task := objectField(t, result, "task")
		switch task["status"] {
		case "succeeded":
			return task
		case "failed", "canceled":
			t.Fatalf("task %s ended in %v (code=%q)", taskID, task["status"], stringField(task, "failure_code"))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("task %s did not finish before the product gate deadline: %v", taskID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertInstalledNodeReceipt(t *testing.T, installation map[string]any) int64 {
	t.Helper()
	if installation["state"] != "installed" || installation["enabled"] != true {
		t.Fatalf("installation state/enabled = %v/%v, want installed/true", installation["state"], installation["enabled"])
	}
	activeVersionID := requiredString(t, installation, "active_version_id")
	versions := arrayField(t, installation, "versions")
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok || stringField(version, "version_id") != activeVersionID {
			continue
		}
		receipt := objectField(t, version, "node_artifact")
		wantKeys := []string{"artifact_bytes", "file_count", "lifecycle_scripts_disabled", "native_addons_absent", "node_version", "npm_version", "package_name", "package_version"}
		gotKeys := make([]string, 0, len(receipt))
		for key := range receipt {
			gotKeys = append(gotKeys, key)
		}
		sort.Strings(gotKeys)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("public Node receipt fields = %v, want exact 8-field contract %v", gotKeys, wantKeys)
		}
		if receipt["package_name"] != nodeMCPGatePackage || receipt["package_version"] != nodeMCPGateVersion ||
			receipt["node_version"] != "v24.18.1" || receipt["npm_version"] != "11.16.0" ||
			receipt["lifecycle_scripts_disabled"] != true || receipt["native_addons_absent"] != true ||
			requiredPositiveInteger(t, receipt, "artifact_bytes") <= 0 || requiredPositiveInteger(t, receipt, "file_count") <= 0 {
			t.Fatal("public Node receipt did not prove the exact pinned runtime, package, and build restrictions")
		}
		return requiredPositiveInteger(t, installation, "revision")
	}
	t.Fatal("active installation version has no public Node artifact receipt")
	return 0
}

func exactTool(t *testing.T, result map[string]any, name string) map[string]any {
	t.Helper()
	for _, raw := range arrayField(t, result, "tools") {
		tool, ok := raw.(map[string]any)
		if ok && tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("installed MCP did not publish expected tool %q", name)
	return nil
}

func assertCalculatorToolSchema(t *testing.T, tool map[string]any) {
	t.Helper()
	schema := objectField(t, tool, "input_schema")
	properties := objectField(t, schema, "properties")
	a := objectField(t, properties, "a")
	b := objectField(t, properties, "b")
	required := arrayField(t, schema, "required")
	if schema["type"] != "object" || a["type"] != "number" || b["type"] != "number" || !containsString(required, "a") || !containsString(required, "b") {
		t.Fatal("calculator add input schema does not require numeric a and b fields")
	}
	if !lowerSHA256(requiredString(t, tool, "input_schema_digest")) {
		t.Fatal("calculator add input schema digest is not a lowercase SHA-256 value")
	}
}

func assertSuccessfulMCPResult(t *testing.T, task map[string]any, expectedText string) {
	t.Helper()
	result := objectField(t, task, "result")
	envelope := objectField(t, result, "json")
	if envelope["isError"] == true || envelope["is_error"] == true {
		t.Fatal("MCP execution completed with a business-error result")
	}
	content := arrayField(t, envelope, "content")
	if len(content) == 0 {
		t.Fatal("MCP execution returned no content")
	}
	first, ok := content[0].(map[string]any)
	if !ok || first["type"] != "text" || stringField(first, "text") != expectedText {
		t.Fatalf("MCP execution did not return the exact expected local result %q", expectedText)
	}
}

func objectField(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	result, ok := value[key].(map[string]any)
	if !ok || result == nil {
		t.Fatalf("field %q is not an object", key)
	}
	return result
}

func arrayField(t *testing.T, value map[string]any, key string) []any {
	t.Helper()
	result, ok := value[key].([]any)
	if !ok {
		t.Fatalf("field %q is not an array", key)
	}
	return result
}

func requiredString(t *testing.T, value map[string]any, key string) string {
	t.Helper()
	result := stringField(value, key)
	if strings.TrimSpace(result) == "" {
		t.Fatalf("field %q is not a non-empty string", key)
	}
	return result
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func requiredPositiveInteger(t *testing.T, value map[string]any, key string) int64 {
	t.Helper()
	number, ok := value[key].(float64)
	if !ok || number <= 0 || number != float64(int64(number)) {
		t.Fatalf("field %q is not a positive integer", key)
	}
	return int64(number)
}

func containsString(values []any, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func lowerSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
