package main

// The driver is deliberately a small, fail-closed acceptance client.  It is
// not a second ProductCore adapter: all mutations go through the public owner
// action boundary and every secret is read from a caller-owned file only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

type Driver struct {
	base    *url.URL
	client  *http.Client
	token   string
	timeout time.Duration
}

type Summary struct {
	Flow       string `json:"flow"`
	CoreReady  bool   `json:"core_ready"`
	AnswerHash string `json:"answer_sha256,omitempty"`
	AnswerLen  int    `json:"answer_length,omitempty"`
	Extensions int    `json:"extensions,omitempty"`
	Workload   bool   `json:"workload,omitempty"`
}

func NewDriver(raw string, timeout time.Duration) (*Driver, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("base URL must be an absolute http(s) URL")
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Driver{base: u, client: &http.Client{Timeout: timeout}, timeout: timeout}, nil
}

func readSecretFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", errors.New("secret file must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file is not a regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("secret file cannot be read")
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("secret file must contain one non-empty line")
	}
	return secret, nil
}

func (d *Driver) action(ctx context.Context, name string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"action": name, "params": params})
	if err != nil {
		return nil, errors.New("cannot encode action request")
	}
	u := *d.base
	u.Path = strings.TrimRight(u.Path, "/") + "/_p2p/command"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, errors.New("cannot create action request")
	}
	req.Header.Set("Content-Type", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New("action request failed")
	}
	defer resp.Body.Close()
	var value map[string]any
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("action returned invalid JSON")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, _ := value["code"].(string)
		if code == "" {
			code = "http_error"
		}
		return nil, fmt.Errorf("action %s failed (%d:%s)", name, resp.StatusCode, code)
	}
	return value, nil
}

func (d *Driver) authenticate(ctx context.Context, ownerSecret string) error {
	if strings.TrimSpace(ownerSecret) == "" {
		return errors.New("owner secret is empty")
	}
	bootstrap, err := d.action(ctx, "portal.bootstrap", map[string]any{"password": ownerSecret})
	if err != nil {
		return err
	}
	if _, ok := bootstrap["access_token"].(string); !ok {
		return errors.New("portal bootstrap omitted access token")
	}
	authToken := ""
	// Bootstrap and auth intentionally use the supplied secret; the bootstrap
	// response is discarded immediately and is never included in summaries.
	d.token = ""
	auth, err := d.action(ctx, "portal.auth", map[string]any{"password": ownerSecret})
	if err != nil {
		return err
	}
	authToken, _ = auth["access_token"].(string)
	if authToken == "" {
		return errors.New("portal auth omitted access token")
	}
	d.token = authToken
	return nil
}

func requireCore(ctx context.Context, d *Driver, caps ...string) (map[string]any, error) {
	result, err := d.action(ctx, "agent.backends.get", nil)
	if err != nil {
		return nil, err
	}
	core, ok := result["core"].(map[string]any)
	if !ok || core["configured"] != true || core["status"] != "ready" || core["api_version"] != "v1" {
		return nil, errors.New("Agent Core is not configured and ready")
	}
	if instanceID, ok := core["instance_id"].(string); !ok || strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("Agent Core readiness omitted instance identity")
	}
	available := map[string]bool{}
	if values, ok := core["capabilities"].([]any); ok {
		for _, value := range values {
			if text, ok := value.(string); ok {
				available[text] = true
			}
		}
	}
	for _, cap := range caps {
		if !available[cap] {
			return nil, fmt.Errorf("required Core capability unavailable: %s", cap)
		}
	}
	return core, nil
}

func newID() string { return uuid.NewString() }

func canonicalDigest(params map[string]any) string {
	encoded, _ := json.Marshal(params) // map encoding is lexicographically ordered
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func mapValue(m map[string]any, key string) (map[string]any, error) {
	v, ok := m[key].(map[string]any)
	if !ok || v == nil {
		return nil, fmt.Errorf("response omitted %s", key)
	}
	return v, nil
}

func stringValue(m map[string]any, key string) (string, error) {
	v, ok := m[key].(string)
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("response omitted %s", key)
	}
	return v, nil
}

func intValue(m map[string]any, key string) (int64, error) {
	v, ok := m[key].(float64)
	if !ok || v != float64(int64(v)) {
		return 0, fmt.Errorf("response omitted integer %s", key)
	}
	return int64(v), nil
}

func (d *Driver) stream(ctx context.Context, message string, profileID string) (string, error) {
	ticketResponse, err := d.action(ctx, "realtime.ws_ticket.create", nil)
	if err != nil {
		return "", err
	}
	ticket, err := stringValue(ticketResponse, "ticket")
	if err != nil {
		return "", err
	}
	u := *d.base
	u.Scheme = map[string]string{"http": "ws", "https": "wss"}[u.Scheme]
	u.Path = strings.TrimRight(u.Path, "/") + "/_p2p/ws"
	query := u.Query()
	query.Set("ticket", ticket)
	u.RawQuery = query.Encode()
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return "", errors.New("realtime websocket connection failed")
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "client.hello"}); err != nil {
		return "", errors.New("realtime hello failed")
	}
	var ready map[string]any
	if err := wsjson.Read(ctx, conn, &ready); err != nil {
		return "", errors.New("realtime ready failed")
	}
	if ready["type"] != "server.ready" {
		return "", errors.New("realtime session is not ready")
	}
	conversationID, profileUUID := newID(), profileID
	params := map[string]any{"conversation_id": conversationID, "expected_revision": int64(0), "client_model_profile_id": profileUUID, "message": message, "extensions": []any{}}
	turnID := newID()
	frame := map[string]any{"type": "client.agent_core_stream", "turn_id": turnID, "request_digest": canonicalDigest(params), "params": params}
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		return "", errors.New("realtime stream request failed")
	}
	answer := strings.Builder{}
	accepted := false
	done := false
	lastSequence := int64(0)
	for !done {
		var event map[string]any
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			return "", errors.New("realtime stream ended before done")
		}
		if event["turn_id"] != turnID {
			return "", errors.New("realtime turn linkage drift")
		}
		switch event["type"] {
		case "server.agent_core_stream.accepted":
			accepted = true
			if _, err := stringValue(event, "core_turn_id"); err != nil {
				return "", err
			}
			if got, _ := event["conversation_id"].(string); got != conversationID {
				return "", errors.New("realtime conversation linkage drift")
			}
		case "server.agent_core_stream.error":
			code, _ := event["code"].(string)
			if code == "" {
				code = "agent_core_stream_error"
			}
			return "", fmt.Errorf("Core stream failed: %s", code)
		case "server.agent_core_stream.cancelled":
			return "", errors.New("Core stream was cancelled")
		case "server.agent_core_stream.event":
			seq, err := intValue(event, "seq")
			if err != nil || seq <= lastSequence {
				return "", errors.New("realtime event sequence drift")
			}
			lastSequence = seq
			if text := extractAnswer(event["data"]); text != "" {
				answer.WriteString(text)
			}
			if event["event"] == "done" || event["event"] == "completed" {
				done = true
			}
		default:
			return "", errors.New("unknown Core stream frame")
		}
	}
	if !accepted || strings.TrimSpace(answer.String()) == "" {
		return "", errors.New("Core stream did not produce a non-empty answer")
	}
	return answer.String(), nil
}

func extractAnswer(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		for _, key := range []string{"text", "delta", "content", "answer", "message"} {
			if text, ok := v[key].(string); ok && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

func (d *Driver) deepseek(ctx context.Context, apiKey string) (string, error) {
	if _, err := requireCore(ctx, d, "agent.info", "model.profile", "conversation"); err != nil {
		return "", err
	}
	clientProfileID := newID()
	syncResponse, err := d.action(ctx, "agent.core.model_profiles.sync", map[string]any{
		"idempotency_key": newID(), "default_client_profile_id": clientProfileID,
		"entries": []any{map[string]any{"client_profile_id": clientProfileID, "display_name": "DeepSeek E2E", "provider": "openai", "base_url": "https://api.deepseek.com", "model": "deepseek-chat", "api_key": apiKey}},
	})
	if err != nil {
		return "", err
	}
	if got, _ := syncResponse["default_client_profile_id"].(string); got != clientProfileID {
		return "", errors.New("model sync default profile linkage drift")
	}
	profiles, ok := syncResponse["profiles"].([]any)
	if !ok || len(profiles) == 0 {
		return "", errors.New("model sync omitted profiles")
	}
	var profileID string
	for _, raw := range profiles {
		profile, ok := raw.(map[string]any)
		if !ok || profile["client_profile_id"] != clientProfileID {
			continue
		}
		if profile["provider"] != "openai_compatible" || profile["base_url"] != "https://api.deepseek.com" || profile["model"] != "deepseek-chat" || profile["api_key_configured"] != true {
			return "", errors.New("model sync profile projection drift")
		}
		if revision, revErr := intValue(profile, "revision"); revErr != nil || revision < 1 {
			return "", errors.New("model sync profile revision is invalid")
		}
		profileID, _ = profile["profile_id"].(string)
	}
	if profileID == "" {
		return "", errors.New("model sync omitted selected profile")
	}
	readback, err := d.action(ctx, "agent.core.model_profiles.get", map[string]any{"profile_id": profileID})
	if err != nil {
		return "", err
	}
	profile, err := mapValue(readback, "profile")
	if err != nil || profile["client_profile_id"] != clientProfileID || profile["provider"] != "openai_compatible" || profile["base_url"] != "https://api.deepseek.com" || profile["model"] != "deepseek-chat" || profile["api_key_configured"] != true {
		return "", errors.New("model profile readback drift")
	}
	return d.stream(ctx, "Reply with one short sentence proving the real Core model is connected.", clientProfileID)
}

func (d *Driver) extensions(ctx context.Context, secretFile string) (int, error) {
	if _, err := requireCore(ctx, d, "agent.info", "mcp", "skill", "task", "confirmation"); err != nil {
		return 0, err
	}
	secretInputs, err := readExtensionSecrets(secretFile)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, kind := range []string{"mcp", "skill"} {
		prefix := "agent.core." + kind
		if kind == "skill" {
			prefix = "agent.core.skills"
		}
		listing, err := d.action(ctx, prefix+".discover", map[string]any{"page_size": int64(32)})
		if err != nil {
			return count, err
		}
		candidates, ok := listing["candidates"].([]any)
		if !ok {
			return count, errors.New("extension discovery omitted candidates")
		}
		var candidate, inspection map[string]any
		var inputs []map[string]any
		for _, item := range candidates {
			m, ok := item.(map[string]any)
			if ok && m["kind"] == kind {
				inspectionResponse, inspectErr := d.action(ctx, prefix+".inspect", map[string]any{"candidate": m})
				if inspectErr != nil {
					return count, inspectErr
				}
				inspected, inspectMapErr := mapValue(inspectionResponse, "inspection")
				if inspectMapErr != nil {
					return count, inspectMapErr
				}
				inspectedCandidate, candidateErr := mapValue(inspected, "candidate")
				if candidateErr != nil || !sameJSON(m, inspectedCandidate) {
					return count, errors.New("extension inspection candidate drift")
				}
				if !validInspectionDigests(inspected) {
					return count, errors.New("extension inspection digest projection drift")
				}
				selectedInputs, selectErr := selectExtensionSecrets(inspected, secretInputs)
				if selectErr != nil {
					if canSkipExtensionCandidate(selectErr) {
						continue
					}
					return count, selectErr
				}
				candidate, inspection, inputs = m, inspected, selectedInputs
				break
			}
		}
		if candidate == nil {
			continue
		}
		params := map[string]any{"idempotency_key": newID(), "candidate": candidate, "inspection": inspection, "secret_inputs": inputs}
		installResponse, err := d.action(ctx, prefix+".install", params)
		if err != nil {
			return count, err
		}
		installation, err := mapValue(installResponse, "installation")
		if err != nil {
			return count, err
		}
		installationID, err := stringValue(installation, "installation_id")
		if err != nil {
			return count, err
		}
		if state, _ := installation["state"].(string); state == "installed" || state == "active" {
			return count, errors.New("extension was executable before confirmation")
		}
		installRevision, revErr := intValue(installation, "revision")
		if revErr != nil || installRevision < 1 {
			return count, errors.New("extension installation revision is invalid")
		}
		before, beforeErr := d.action(ctx, prefix+".get", map[string]any{"installation_id": installationID})
		if beforeErr != nil {
			return count, beforeErr
		}
		beforeInstallation, beforeErr := mapValue(before, "installation")
		if beforeErr != nil || beforeInstallation["installation_id"] != installationID {
			return count, errors.New("extension pre-confirm readback drift")
		}
		if beforeInstallation["kind"] != kind || beforeInstallation["candidate_id"] != candidate["id"] {
			return count, errors.New("extension pre-confirm identity drift")
		}
		if beforeRevision, revisionErr := intValue(beforeInstallation, "revision"); revisionErr != nil || beforeRevision != installRevision {
			return count, errors.New("extension pre-confirm revision drift")
		}
		if state, _ := beforeInstallation["state"].(string); state != "installing" && state != "draft" {
			return count, errors.New("extension pre-confirm state is executable")
		}
		if active, _ := beforeInstallation["active_version_id"].(string); active != "" {
			return count, errors.New("extension has an active version before confirmation")
		}
		proposed, _ := beforeInstallation["proposed_version_id"].(string)
		if proposed == "" || !installationHasVersion(beforeInstallation, proposed) {
			return count, errors.New("extension pre-confirm proposed version linkage drift")
		}
		if kind == "mcp" {
			preTools, preToolsErr := d.action(ctx, "agent.core.mcp.list_tools", map[string]any{"installation_id": installationID, "expected_revision": installRevision})
			if preToolsErr == nil {
				if values, ok := preTools["tools"].([]any); !ok || len(values) != 0 {
					return count, errors.New("MCP tools became available before confirmation")
				}
			} else if !strings.Contains(preToolsErr.Error(), "not found") && !strings.Contains(preToolsErr.Error(), "precondition") {
				return count, preToolsErr
			}
		}
		confirmationID, err := stringValue(installResponse, "confirmation_id")
		if err != nil {
			return count, err
		}
		taskID, err := stringValue(installResponse, "task_id")
		if err != nil {
			return count, err
		}
		contentDigest, _ := inspection["content_digest"].(string)
		networkGrants, _ := inspection["network_grants"].([]any)
		secretGrants, _ := inspection["secret_grants"].([]any)
		if err := d.confirmTaskChecked(ctx, confirmationID, taskID, map[string]string{"operation_domain": "extension", "target_id": installationID, "target_revision": fmt.Sprintf("%d", installRevision), "content_digest": contentDigest, "parameter_digest": "*", "network_digest": "*", "secret_grant_digest": "*", "network_grants_count": fmt.Sprintf("%d", len(networkGrants)), "secret_grants_count": fmt.Sprintf("%d", len(secretGrants))}, inspection, nil); err != nil {
			return count, err
		}
		installationReadback, err := d.action(ctx, prefix+".get", map[string]any{"installation_id": installationID})
		if err != nil {
			return count, err
		}
		readbackInstallation, err := mapValue(installationReadback, "installation")
		if err != nil {
			return count, err
		}
		if got, _ := readbackInstallation["installation_id"].(string); got != installationID {
			return count, errors.New("extension installation readback drift")
		}
		if state, _ := readbackInstallation["state"].(string); state != "installed" && state != "active" {
			return count, errors.New("extension installation did not reach active state")
		}
		if readbackInstallation["kind"] != kind || readbackInstallation["candidate_id"] != candidate["id"] {
			return count, errors.New("extension installation identity drift")
		}
		if revision, revErr := intValue(readbackInstallation, "revision"); revErr != nil || revision < 1 {
			return count, errors.New("extension installation revision is invalid")
		}
		versions, ok := readbackInstallation["versions"].([]any)
		if !ok || !installationHasDigests(versions, inspection) {
			return count, errors.New("extension installation version digest drift")
		}
		if kind == "mcp" {
			readbackRevision, _ := intValue(readbackInstallation, "revision")
			toolsResponse, err := d.action(ctx, "agent.core.mcp.list_tools", map[string]any{"installation_id": installationID, "expected_revision": readbackRevision})
			if err != nil {
				return count, err
			}
			tools, ok := toolsResponse["tools"].([]any)
			if !ok || len(tools) == 0 {
				return count, errors.New("MCP installation returned no tools")
			}
			tool, ok := tools[0].(map[string]any)
			if !ok {
				return count, errors.New("MCP tool projection is invalid")
			}
			toolName, err := stringValue(tool, "name")
			if err != nil {
				return count, err
			}
			exec, err := d.action(ctx, "agent.core.mcp.execute", map[string]any{"idempotency_key": newID(), "installation_id": installationID, "tool_name": toolName, "input": map[string]any{}})
			if err != nil {
				return count, err
			}
			execTask, err := stringValue(exec, "task_id")
			if err != nil {
				return count, err
			}
			if err := d.pollTask(ctx, execTask); err != nil {
				return count, err
			}
		} else {
			exec, err := d.action(ctx, "agent.core.skills.execute", map[string]any{"idempotency_key": newID(), "installation_id": installationID, "input": map[string]any{}})
			if err != nil {
				return count, err
			}
			execTask, err := stringValue(exec, "task_id")
			if err != nil {
				return count, err
			}
			if err := d.pollTask(ctx, execTask); err != nil {
				return count, err
			}
		}
		count++
	}
	if count != 2 {
		return count, errors.New("both MCP and Skill installations are required")
	}
	return count, nil
}

func (d *Driver) confirmTask(ctx context.Context, confirmationID, taskID string, expected ...map[string]string) error {
	var fields map[string]string
	if len(expected) > 0 {
		fields = expected[0]
	}
	return d.confirmTaskChecked(ctx, confirmationID, taskID, fields, nil, nil)
}

func (d *Driver) confirmTaskChecked(ctx context.Context, confirmationID, taskID string, expected map[string]string, inspection map[string]any, initialBinding map[string]any) error {
	first, err := d.action(ctx, "agent.core.confirmations.get", map[string]any{"confirmation_id": confirmationID})
	if err != nil {
		return err
	}
	confirmation, err := mapValue(first, "confirmation")
	if err != nil {
		return err
	}
	if got, _ := confirmation["confirmation_id"].(string); got != confirmationID {
		return errors.New("confirmation linkage drift")
	}
	if state, _ := confirmation["state"].(string); state != "pending" {
		return errors.New("confirmation is not pending")
	}
	if got, _ := confirmation["task_id"].(string); got != taskID {
		return errors.New("confirmation task linkage drift")
	}
	if len(expected) > 0 {
		binding, bindingErr := mapValue(confirmation, "binding")
		if bindingErr != nil {
			return bindingErr
		}
		if initialBinding != nil && !sameJSON(binding, initialBinding) {
			return errors.New("confirmation binding changed before confirm")
		}
		for key, want := range expected {
			if strings.HasSuffix(key, "_digest") && want == "*" {
				value, _ := binding[key].(string)
				if !validHexDigest(value) {
					return errors.New("confirmation digest binding drift")
				}
				continue
			}
			if key == "network_grants_count" || key == "secret_grants_count" {
				values, ok := binding[strings.TrimSuffix(key, "_count")].([]any)
				if !ok || len(values) != atoiSafe(want) {
					return errors.New("confirmation grant binding drift")
				}
				continue
			}
			got := ""
			if text, ok := binding[key].(string); ok {
				got = text
			} else if number, ok := binding[key].(float64); ok && number == float64(int64(number)) {
				got = fmt.Sprintf("%d", int64(number))
			}
			if got != want {
				return errors.New("confirmation binding drift")
			}
		}
		if inspection != nil {
			if err := validateBindingDescriptors(binding, inspection); err != nil {
				return err
			}
		}
	}
	revision, err := intValue(confirmation, "revision")
	if err != nil {
		return err
	}
	confirmed, err := d.action(ctx, "agent.core.confirmations.confirm", map[string]any{"confirmation_id": confirmationID, "idempotency_key": newID(), "expected_revision": revision})
	if err != nil {
		return err
	}
	confirmedObject, err := mapValue(confirmed, "confirmation")
	if err != nil {
		return err
	}
	if got, _ := confirmedObject["confirmation_id"].(string); got != confirmationID {
		return errors.New("confirmation response linkage drift")
	}
	if state, _ := confirmedObject["state"].(string); state != "confirmed" && state != "consumed" {
		return errors.New("confirmation response is not terminally confirmed")
	}
	if confirmedRevision, revErr := intValue(confirmedObject, "revision"); revErr != nil || confirmedRevision <= revision {
		return errors.New("confirmation response revision drift")
	}
	return d.pollTask(ctx, taskID)
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateBindingDescriptors(binding map[string]any, inspection map[string]any) error {
	network, _ := inspection["network_grants"].([]any)
	boundNetwork, ok := binding["network_grants"].([]any)
	if !ok || len(boundNetwork) != len(network) {
		return errors.New("confirmation network grant descriptor drift")
	}
	expectedNetwork := make([]string, 0, len(network))
	for _, raw := range network {
		grant, ok := raw.(map[string]any)
		if !ok {
			return errors.New("inspection network grant descriptor invalid")
		}
		scheme, schemeOK := grant["scheme"].(string)
		host, hostOK := grant["host"].(string)
		pathPrefix, _ := grant["path_prefix"].(string)
		digest, digestOK := grant["digest"].(string)
		port, portErr := intValue(grant, "port")
		if !schemeOK || !hostOK || !digestOK || portErr != nil || scheme == "" || host == "" || !validHexDigest(digest) {
			return errors.New("inspection network grant descriptor invalid")
		}
		expectedNetwork = append(expectedNetwork, fmt.Sprintf("%s://%s:%d%s:%s", scheme, host, port, pathPrefix, digest))
	}
	actualNetwork := make([]string, 0, len(boundNetwork))
	for _, raw := range boundNetwork {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return errors.New("confirmation network grant descriptor invalid")
		}
		actualNetwork = append(actualNetwork, strings.TrimSpace(value))
	}
	sort.Strings(expectedNetwork)
	sort.Strings(actualNetwork)
	if !sameJSON(expectedNetwork, actualNetwork) {
		return errors.New("confirmation network grant descriptor drift")
	}
	secrets, _ := inspection["secret_grants"].([]any)
	boundSecrets, ok := binding["secret_grants"].([]any)
	if !ok || len(boundSecrets) != len(secrets) {
		return errors.New("confirmation secret grant descriptor drift")
	}
	type secretGrant struct{ ref, purpose, digest string }
	expectedSecrets := make([]secretGrant, 0, len(secrets))
	for _, raw := range secrets {
		grant, ok := raw.(map[string]any)
		if !ok {
			return errors.New("inspection secret grant descriptor invalid")
		}
		expected := secretGrant{ref: strings.TrimSpace(fmt.Sprint(grant["reference_id"])), purpose: strings.TrimSpace(fmt.Sprint(grant["purpose"])), digest: strings.TrimSpace(fmt.Sprint(grant["binding_digest"]))}
		if expected.ref == "" || expected.purpose == "" || !validHexDigest(expected.digest) {
			return errors.New("inspection secret grant descriptor invalid")
		}
		expectedSecrets = append(expectedSecrets, expected)
	}
	actualSecrets := make([]secretGrant, 0, len(boundSecrets))
	for _, raw := range boundSecrets {
		grant, ok := raw.(map[string]any)
		actual := secretGrant{ref: strings.TrimSpace(fmt.Sprint(grant["reference_id"])), purpose: strings.TrimSpace(fmt.Sprint(grant["purpose"])), digest: strings.TrimSpace(fmt.Sprint(grant["binding_digest"]))}
		if !ok || !validHexDigest(actual.digest) || actual.ref == "" || actual.purpose == "" {
			return errors.New("confirmation secret grant descriptor invalid")
		}
		actualSecrets = append(actualSecrets, actual)
	}
	sort.Slice(expectedSecrets, func(i, j int) bool { return fmt.Sprint(expectedSecrets[i]) < fmt.Sprint(expectedSecrets[j]) })
	sort.Slice(actualSecrets, func(i, j int) bool { return fmt.Sprint(actualSecrets[i]) < fmt.Sprint(actualSecrets[j]) })
	for i := range expectedSecrets {
		if expectedSecrets[i] != actualSecrets[i] {
			return errors.New("confirmation secret grant descriptor drift")
		}
	}
	return nil
}

func atoiSafe(value string) int {
	var result int
	for _, char := range value {
		if char < '0' || char > '9' {
			return -1
		}
		result = result*10 + int(char-'0')
	}
	return result
}

func (d *Driver) pollTask(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(d.timeout)
	lastSequence := int64(0)
	for time.Now().Before(deadline) {
		result, err := d.action(ctx, "agent.core.tasks.get", map[string]any{"task_id": taskID})
		if err != nil {
			return err
		}
		task, err := mapValue(result, "task")
		if err != nil {
			return err
		}
		if got, _ := task["task_id"].(string); got != taskID {
			return errors.New("task linkage drift")
		}
		events, eventErr := d.action(ctx, "agent.core.tasks.events", map[string]any{"task_id": taskID, "after_sequence": lastSequence, "limit": int64(128)})
		if eventErr != nil {
			return eventErr
		}
		values, ok := events["events"].([]any)
		if !ok {
			return errors.New("task events response omitted events")
		}
		for _, raw := range values {
			event, ok := raw.(map[string]any)
			if !ok {
				return errors.New("task event projection is invalid")
			}
			if got, _ := event["task_id"].(string); got != taskID {
				return errors.New("task event linkage drift")
			}
			seq, seqErr := intValue(event, "sequence")
			if seqErr != nil || seq <= lastSequence {
				return errors.New("task event sequence drift")
			}
			lastSequence = seq
		}
		state, _ := task["status"].(string)
		switch state {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return fmt.Errorf("task terminal state: %s", state)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("task polling timed out")
}

func readExtensionSecrets(path string) ([]map[string]any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("extension secret input file must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("extension secret input file is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("extension secret input file cannot be read")
	}
	var values []map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, errors.New("extension secret input file is invalid JSON")
	}
	for _, value := range values {
		if len(value) != 3 {
			return nil, errors.New("extension secret input contains unknown fields")
		}
		if _, err := stringValue(value, "reference_id"); err != nil {
			return nil, err
		}
		if _, err := stringValue(value, "purpose"); err != nil {
			return nil, err
		}
		if _, err := stringValue(value, "secret_value"); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func canSkipExtensionCandidate(err error) bool {
	text := err.Error()
	return strings.Contains(text, "not fully supplied") || strings.Contains(text, "without grants") || strings.Contains(text, "grant linkage drift")
}

func installationHasDigests(versions []any, inspection map[string]any) bool {
	content, _ := inspection["content_digest"].(string)
	manifest, _ := inspection["manifest_digest"].(string)
	execution, _ := inspection["execution_digest"].(string)
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if ok && version["content_digest"] == content && version["manifest_digest"] == manifest && version["execution_digest"] == execution && version["network_schema_digest"] == inspection["network_schema_digest"] && version["secret_schema_digest"] == inspection["secret_schema_digest"] {
			return true
		}
	}
	return false
}

func installationHasVersion(installation map[string]any, versionID string) bool {
	versions, ok := installation["versions"].([]any)
	if !ok {
		return false
	}
	for _, raw := range versions {
		if version, ok := raw.(map[string]any); ok && version["version_id"] == versionID {
			return true
		}
	}
	return false
}

func validInspectionDigests(inspection map[string]any) bool {
	for _, key := range []string{"content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest"} {
		value, ok := inspection[key].(string)
		if !ok || len(value) != 64 {
			return false
		}
		for _, char := range value {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return false
			}
		}
	}
	return true
}

func selectExtensionSecrets(inspection map[string]any, provided []map[string]any) ([]map[string]any, error) {
	grants, ok := inspection["secret_grants"].([]any)
	if !ok {
		return nil, errors.New("inspection omitted secret grants")
	}
	if len(grants) == 0 {
		if len(provided) != 0 {
			return nil, errors.New("secret inputs supplied for an extension without grants")
		}
		return []map[string]any{}, nil
	}
	if len(grants) != len(provided) {
		return nil, errors.New("extension secret grants are not fully supplied")
	}
	byRef := map[string]map[string]any{}
	for _, item := range provided {
		ref, _ := item["reference_id"].(string)
		if _, exists := byRef[ref]; exists || ref == "" {
			return nil, errors.New("duplicate extension secret reference")
		}
		byRef[ref] = item
	}
	for _, raw := range grants {
		grant, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("inspection secret grant is invalid")
		}
		ref, err := stringValue(grant, "reference_id")
		if err != nil {
			return nil, err
		}
		input, ok := byRef[ref]
		if !ok || input["purpose"] != grant["purpose"] {
			return nil, errors.New("extension secret grant linkage drift")
		}
	}
	return provided, nil
}

func sameJSON(a, b any) bool {
	left, err1 := json.Marshal(a)
	right, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(left) == string(right)
}

func (d *Driver) workload(ctx context.Context, path string) error {
	if _, err := requireCore(ctx, d, "agent.info", "task", "confirmation", "workload.core_runner"); err != nil {
		return err
	}
	planParams, err := readWorkloadPlan(path)
	if err != nil {
		return err
	}
	planResponse, err := d.action(ctx, "agent.core.workloads.plan", planParams)
	if err != nil {
		return err
	}
	plan, err := mapValue(planResponse, "plan")
	if err != nil {
		return err
	}
	planID, err := stringValue(plan, "plan_id")
	if err != nil {
		return err
	}
	if _, err := intValue(plan, "revision"); err != nil {
		return err
	}
	planDigest, err := stringValue(plan, "digest")
	if err != nil {
		return err
	}
	getResponse, err := d.action(ctx, "agent.core.workloads.get", map[string]any{"plan_id": planID})
	if err != nil {
		return err
	}
	gotPlan, err := mapValue(getResponse, "plan")
	if err != nil {
		return err
	}
	if gotPlan["plan_id"] != planID || gotPlan["digest"] != planDigest {
		return errors.New("workload plan readback drift")
	}
	quoteResponse, err := d.action(ctx, "agent.core.workloads.quote", map[string]any{"plan_id": planID})
	if err != nil {
		return err
	}
	quote, err := mapValue(quoteResponse, "quote")
	if err != nil {
		return err
	}
	if quote["plan_id"] != planID || quote["plan_digest"] != planDigest {
		return errors.New("workload quote linkage drift")
	}
	applyParams := map[string]any{"idempotency_key": newID(), "plan_id": planID}
	applyResponse, err := d.action(ctx, "agent.core.workloads.apply", applyParams)
	if err != nil {
		return err
	}
	operation, err := mapValue(applyResponse, "operation")
	if err != nil {
		return err
	}
	if operation["plan_id"] != planID || operation["plan_digest"] != planDigest || operation["target_kind"] != plan["target_kind"] || operation["plan_revision"] != plan["revision"] {
		return errors.New("workload operation linkage drift")
	}
	confirmation, err := mapValue(applyResponse, "confirmation")
	if err != nil {
		return err
	}
	confirmationID, err := stringValue(confirmation, "confirmation_id")
	if err != nil {
		return err
	}
	taskID, err := stringValue(applyResponse, "task_id")
	if err != nil {
		return err
	}
	if operation["task_id"] != taskID || operation["confirmation_id"] != confirmationID {
		return errors.New("workload operation task linkage drift")
	}
	workloadID, _ := operation["workload_id"].(string)
	if workloadID == "" {
		return errors.New("workload apply omitted workload id")
	}
	initialBinding, bindingErr := mapValue(confirmation, "binding")
	if bindingErr != nil {
		return bindingErr
	}
	if err := d.confirmTaskChecked(ctx, confirmationID, taskID, map[string]string{"operation_domain": "workload:apply", "target_id": workloadID, "target_revision": fmt.Sprint(operation["plan_revision"]), "content_digest": planDigest, "parameter_digest": "*", "network_digest": "*", "secret_grant_digest": "*"}, nil, initialBinding); err != nil {
		return err
	}
	if _, err := d.pollWorkloadOperation(ctx, operation, false); err != nil {
		return err
	}
	applyActualResponse, err := d.action(ctx, "agent.core.workloads.actual.get", map[string]any{"workload_id": workloadID})
	if err != nil {
		return err
	}
	applyActual, err := mapValue(applyActualResponse, "workload")
	if err != nil || applyActual["workload_id"] != workloadID || applyActual["applied_plan_id"] != planID || applyActual["applied_plan_digest"] != planDigest || applyActual["readback_digest"] == "" || applyActual["provider_version"] == "" || applyActual["identity"] == nil {
		return errors.New("workload apply actual readback drift")
	}
	applyState, _ := applyActual["state"].(string)
	if applyState != "ready" && applyState != "applied" {
		return errors.New("workload apply actual state is not ready")
	}
	readback, err := d.action(ctx, "agent.core.workloads.get", map[string]any{"plan_id": planID})
	if err != nil {
		return err
	}
	readbackPlan, err := mapValue(readback, "plan")
	if err != nil || readbackPlan["digest"] != planDigest {
		return errors.New("workload post-apply readback drift")
	}
	destroyParams := map[string]any{"idempotency_key": newID(), "plan_id": planID, "workload_id": workloadID}
	destroyResponse, err := d.action(ctx, "agent.core.workloads.destroy", destroyParams)
	if err != nil {
		return err
	}
	destroyOperation, err := mapValue(destroyResponse, "operation")
	if err != nil {
		return err
	}
	if destroyOperation["plan_id"] != planID || destroyOperation["plan_digest"] != planDigest || destroyOperation["target_kind"] != plan["target_kind"] || destroyOperation["plan_revision"] != plan["revision"] {
		return errors.New("workload destroy operation linkage drift")
	}
	destroyConfirmationObject, err := mapValue(destroyResponse, "confirmation")
	if err != nil {
		return err
	}
	destroyConfirmation, err := stringValue(destroyConfirmationObject, "confirmation_id")
	if err != nil {
		return err
	}
	destroyTask, err := stringValue(destroyResponse, "task_id")
	if err != nil {
		return err
	}
	destroyInitialBinding, bindingErr := mapValue(destroyConfirmationObject, "binding")
	if bindingErr != nil {
		return bindingErr
	}
	if err := d.confirmTaskChecked(ctx, destroyConfirmation, destroyTask, map[string]string{"operation_domain": "workload:destroy", "target_id": workloadID, "target_revision": fmt.Sprint(destroyOperation["plan_revision"]), "content_digest": planDigest, "parameter_digest": "*", "network_digest": "*", "secret_grant_digest": "*"}, nil, destroyInitialBinding); err != nil {
		return err
	}
	if _, err := d.pollWorkloadOperation(ctx, destroyOperation, true); err != nil {
		return err
	}
	actualResponse, err := d.action(ctx, "agent.core.workloads.actual.get", map[string]any{"workload_id": workloadID})
	if err != nil {
		return err
	}
	actual, err := mapValue(actualResponse, "workload")
	if err != nil || actual["workload_id"] != workloadID || actual["applied_plan_id"] != planID || actual["applied_plan_digest"] != planDigest || actual["readback_digest"] == "" || actual["provider_version"] == "" || actual["identity"] == nil || actual["state"] != "destroyed" {
		return errors.New("workload destroy actual readback drift")
	}
	return nil
}

func (d *Driver) pollWorkloadOperation(ctx context.Context, initial map[string]any, destroy bool) (map[string]any, error) {
	operationID, err := stringValue(initial, "operation_id")
	if err != nil {
		return nil, err
	}
	planID, err := stringValue(initial, "plan_id")
	if err != nil {
		return nil, err
	}
	planDigest, err := stringValue(initial, "plan_digest")
	if err != nil {
		return nil, err
	}
	workloadID, err := stringValue(initial, "workload_id")
	if err != nil {
		return nil, err
	}
	taskID, err := stringValue(initial, "task_id")
	if err != nil {
		return nil, err
	}
	confirmationID, err := stringValue(initial, "confirmation_id")
	if err != nil {
		return nil, err
	}
	initialRevision, err := intValue(initial, "revision")
	if err != nil || initialRevision < 1 {
		return nil, errors.New("workload operation initial revision is invalid")
	}
	lastSequence := int64(0)
	lastRevision := initialRevision
	wantKind := "apply"
	if destroy {
		wantKind = "destroy"
	}
	deadline := time.Now().Add(d.timeout)
	for time.Now().Before(deadline) {
		response, err := d.action(ctx, "agent.core.workloads.operations.get", map[string]any{"operation_id": operationID})
		if err != nil {
			return nil, err
		}
		operation, err := mapValue(response, "operation")
		if err != nil {
			return nil, err
		}
		for key, want := range map[string]any{"operation_id": operationID, "workload_id": workloadID, "plan_id": planID, "plan_digest": planDigest, "task_id": taskID, "confirmation_id": confirmationID, "kind": wantKind, "target_kind": initial["target_kind"], "plan_revision": initial["plan_revision"]} {
			if operation[key] != want {
				return nil, errors.New("workload operation readback linkage drift")
			}
		}
		revision, revisionErr := intValue(operation, "revision")
		if revisionErr != nil || revision < lastRevision {
			return nil, errors.New("workload operation revision drift")
		}
		lastRevision = revision
		eventsResponse, err := d.action(ctx, "agent.core.workloads.operations.events", map[string]any{"operation_id": operationID, "after_sequence": lastSequence})
		if err != nil {
			return nil, err
		}
		events, ok := eventsResponse["events"].([]any)
		if !ok {
			return nil, errors.New("workload operation events omitted events")
		}
		for _, raw := range events {
			event, ok := raw.(map[string]any)
			if !ok || event["operation_id"] != operationID {
				return nil, errors.New("workload operation event linkage drift")
			}
			sequence, sequenceErr := intValue(event, "sequence")
			if sequenceErr != nil || sequence <= lastSequence {
				return nil, errors.New("workload operation event sequence drift")
			}
			lastSequence = sequence
			if sparse, present := event["actual"]; present && sparse != nil {
				if sparseMap, ok := sparse.(map[string]any); !ok || sparseMap["workload_id"] != workloadID {
					return nil, errors.New("workload sparse readback linkage drift")
				}
			}
		}
		status, _ := operation["status"].(string)
		if status == "succeeded" {
			if revision <= initialRevision {
				return nil, errors.New("workload operation terminal revision did not advance")
			}
			actual, _ := operation["actual"].(map[string]any)
			if actual == nil || actual["workload_id"] != workloadID || actual["applied_plan_id"] != planID || actual["applied_plan_digest"] != planDigest || actual["readback_digest"] == "" || actual["provider_version"] == "" || actual["identity"] == nil {
				return nil, errors.New("workload operation actual readback drift")
			}
			state, _ := actual["state"].(string)
			if destroy && state != "destroyed" || !destroy && state != "ready" && state != "applied" {
				return nil, errors.New("workload operation terminal state drift")
			}
			return operation, nil
		}
		if status == "failed" || status == "uncertain" || status == "rejected" || status == "expired" || status == "canceled" {
			return nil, fmt.Errorf("workload operation terminal state: %s", status)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, errors.New("workload operation polling timed out")
}

func readWorkloadPlan(path string) (map[string]any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("workload plan file is required")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("workload plan file must be an absolute path")
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workload plan file is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("workload plan file cannot be read")
	}
	var params map[string]any
	if err := json.Unmarshal(data, &params); err != nil || params == nil {
		return nil, errors.New("workload plan file is invalid JSON")
	}
	allowed := map[string]bool{"idempotency_key": true, "summary": true, "artifact": true, "source": true, "command_steps": true, "image_digest": true, "image_uri": true, "target_kind": true, "expires_at": true, "typed_target": true, "typed_resource_limits": true, "typed_secret_grants": true}
	for key := range params {
		if !allowed[key] {
			return nil, fmt.Errorf("workload plan contains unsupported field: %s", key)
		}
	}
	if err := rejectSecretValues(params); err != nil {
		return nil, err
	}
	if _, ok := params["idempotency_key"].(string); !ok || params["idempotency_key"] == "" {
		params["idempotency_key"] = newID()
	}
	return params, nil
}

func rejectSecretValues(value any) error {
	var walk func(any, string) error
	walk = func(item any, key string) error {
		lower := strings.ToLower(key)
		if key != "typed_secret_grants" && key != "secret_grants" && key != "reference_id" && key != "binding_digest" && key != "purpose" {
			for _, forbidden := range []string{"secret_value", "access_key", "password", "private_key", "api_key", "token"} {
				if strings.Contains(lower, forbidden) {
					return errors.New("workload plan must not contain secret values")
				}
			}
		}
		switch v := item.(type) {
		case map[string]any:
			for child, value := range v {
				if err := walk(value, child); err != nil {
					return err
				}
			}
		case []any:
			for _, value := range v {
				if err := walk(value, key); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value, "")
}
