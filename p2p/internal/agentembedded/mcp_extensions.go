package agentembedded

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/google/uuid"
)

type MCPCatalog interface {
	Search(context.Context, string, string, int, string) ([]extensions.Candidate, string, error)
	Inspect(context.Context, extensions.Candidate) (extensions.Inspection, error)
}

// MCPActionPort is the in-process implementation of agent.core.mcp.*. It
// deliberately accepts no source adapter or local runner: only pinned remote
// HTTPS MCP candidates can cross this boundary.
type MCPActionPort struct {
	Service *extensions.Service
	Client  func(string, extensions.Installation, extensions.Version) *extensions.MCPClient
	Catalog MCPCatalog
}

func NewMCPActionPort(service *extensions.Service, client func(string, extensions.Installation, extensions.Version) *extensions.MCPClient) *MCPActionPort {
	return &MCPActionPort{Service: service, Client: client}
}
func (p *MCPActionPort) Handle(ctx context.Context, owner, action string, params map[string]any) (any, *actionbase.Error) {
	if p == nil || p.Service == nil || strings.TrimSpace(owner) == "" {
		return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	owner = strings.TrimSpace(owner)
	switch action {
	case "agent.core.mcp.discover":
		return p.discover(ctx, params)
	case "agent.core.mcp.inspect":
		return p.inspect(ctx, params)
	case "agent.core.mcp.install":
		return p.mutate(ctx, owner, params, false)
	case "agent.core.mcp.update":
		return p.mutate(ctx, owner, params, true)
	case "agent.core.mcp.remove":
		return p.remove(ctx, owner, params)
	case "agent.core.mcp.get":
		return p.get(ctx, owner, params)
	case "agent.core.mcp.list":
		return p.list(ctx, owner, params)
	case "agent.core.mcp.list_tools":
		return p.tools(ctx, owner, params)
	case "agent.core.mcp.execute":
		return p.execute(ctx, owner, params)
	default:
		return nil, actionbase.CodedError(http.StatusNotFound, "mcp_action_not_found", "unsupported MCP action")
	}
}

func (p *MCPActionPort) discover(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if p.Catalog == nil {
		return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	pageSize := int(int64Value(params["page_size"]))
	if pageSize <= 0 {
		pageSize = 25
	}
	if pageSize > 100 {
		return nil, actionbase.BadRequest("page_size is too large")
	}
	source, actionErr := internalMCPSource(stringValue(params["source"]))
	if actionErr != nil {
		return nil, actionErr
	}
	candidates, next, err := p.Catalog.Search(ctx, source, stringValue(params["query"]), pageSize, stringValue(params["page_token"]))
	if err != nil {
		return nil, mapMCPCatalogErr(err)
	}
	out := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Validate() != nil {
			return nil, actionbase.InternalError(extensions.ErrInvalid)
		}
		out = append(out, candidateDTO(candidate))
	}
	return map[string]any{"candidates": out, "next_page_token": next}, nil
}

func (p *MCPActionPort) inspect(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if p.Catalog == nil {
		return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	c, actionErr := candidateParam(params)
	if actionErr != nil {
		return nil, actionErr
	}
	i, err := p.Catalog.Inspect(ctx, c)
	if err != nil {
		return nil, mapMCPCatalogErr(err)
	}
	if !reflect.DeepEqual(i.Candidate, c) {
		return nil, actionbase.InternalError(extensions.ErrConflict)
	}
	if err := i.Validate(); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return map[string]any{"inspection": inspectionDTO(i)}, nil
}
func (p *MCPActionPort) mutate(ctx context.Context, owner string, params map[string]any, update bool) (any, *actionbase.Error) {
	if p.Catalog == nil {
		return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	c, actionErr := candidateParam(params)
	if actionErr != nil {
		return nil, actionErr
	}
	i, actionErr := inspectionParam(params, c)
	if actionErr != nil {
		return nil, actionErr
	}
	m := extensions.Mutation{OwnerID: owner, IdempotencyKey: stringValue(params["idempotency_key"]), InstallationID: stringValue(params["installation_id"]), ExpectedRevision: int64Value(params["expected_revision"]), Candidate: c, Inspection: i}
	var inputErr *actionbase.Error
	m.SecretInputs, inputErr = secretInputsParam(params["secret_inputs"])
	if inputErr != nil {
		return nil, inputErr
	}
	authoritative, inspectErr := p.Catalog.Inspect(ctx, c)
	if inspectErr != nil {
		return nil, mapMCPCatalogErr(inspectErr)
	}
	if !inspectionMatchesAuthority(i, authoritative) {
		return nil, actionbase.CodedError(http.StatusConflict, "mcp_inspection_changed", "MCP inspection no longer matches the immutable source pin")
	}
	// The catalog result is authoritative for endpoint/grant metadata. The
	// service binds write-only secret values to configured grant fingerprints.
	m.Inspection = authoritative
	if !validUUIDString(m.IdempotencyKey) {
		return nil, actionbase.BadRequest("idempotency_key is required")
	}
	var r extensions.LifecycleResult
	var err error
	if update {
		r, err = p.Service.Update(ctx, m)
	} else {
		r, err = p.Service.Install(ctx, m)
	}
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return lifecycleDTO(r), nil
}
func secretInputsParam(raw any) ([]extensions.SecretInput, *actionbase.Error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		if typed, yes := raw.([]map[string]any); yes {
			items = make([]any, len(typed))
			for i := range typed {
				items[i] = typed[i]
			}
			ok = true
		}
	}
	if !ok {
		return nil, actionbase.BadRequest("secret_inputs must be an array")
	}
	out := make([]extensions.SecretInput, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("secret_inputs must contain objects")
		}
		ref, ok := m["reference_id"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, actionbase.BadRequest("secret_inputs.reference_id is required")
		}
		purpose, ok := m["purpose"].(string)
		if !ok || strings.TrimSpace(purpose) == "" {
			return nil, actionbase.BadRequest("secret_inputs.purpose is required")
		}
		value, ok := m["secret_value"].(string)
		if !ok || value == "" {
			return nil, actionbase.BadRequest("secret_inputs.secret_value is required")
		}
		out = append(out, extensions.SecretInput{ReferenceID: strings.TrimSpace(ref), Purpose: strings.ReplaceAll(strings.TrimSpace(purpose), "-", "_"), Value: value})
	}
	return out, nil
}
func (p *MCPActionPort) remove(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	id := stringValue(params["installation_id"])
	key := stringValue(params["idempotency_key"])
	if id == "" || key == "" {
		return nil, actionbase.BadRequest("installation_id and idempotency_key are required")
	}
	rev := int64Value(params["expected_revision"])
	r, e := p.Service.Uninstall(ctx, owner, id, key, rev)
	if e != nil {
		return nil, mapMCPErr(e)
	}
	return lifecycleDTO(r), nil
}
func (p *MCPActionPort) get(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	id := stringValue(params["installation_id"])
	if id == "" {
		return nil, actionbase.BadRequest("installation_id is required")
	}
	i, e := p.Service.Store.Get(ctx, owner, id)
	if e != nil {
		return nil, mapMCPErr(e)
	}
	return map[string]any{"installation": installationDTO(i)}, nil
}
func (p *MCPActionPort) list(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	n := int(int64Value(params["page_size"]))
	if n <= 0 {
		n = 25
	}
	if n > 100 {
		return nil, actionbase.BadRequest("page_size is too large")
	}
	source, actionErr := internalMCPSource(stringValue(params["source"]))
	if actionErr != nil {
		return nil, actionErr
	}
	state, actionErr := internalMCPState(stringValue(params["state"]))
	if actionErr != nil {
		return nil, actionErr
	}
	var (
		items []extensions.Installation
		next  string
		e     error
	)
	if source == "" && state == "" {
		items, next, e = p.Service.Store.List(ctx, owner, n, stringValue(params["page_token"]))
	} else {
		store, ok := p.Service.Store.(extensions.FilteredStore)
		if !ok {
			return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
		}
		items, next, e = store.ListFiltered(ctx, owner, n, stringValue(params["page_token"]), source, state)
	}
	if e != nil {
		return nil, mapMCPErr(e)
	}
	out := make([]any, 0, len(items))
	for _, i := range items {
		out = append(out, installationDTO(i))
	}
	return map[string]any{"installations": out, "next_page_token": next}, nil
}
func (p *MCPActionPort) tools(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	id := stringValue(params["installation_id"])
	key := stringValue(params["idempotency_key"])
	if !validUUIDString(id) || !validUUIDString(key) {
		return nil, actionbase.BadRequest("installation_id and idempotency_key are required")
	}
	_, _, tools, actionErr := p.ensurePinnedTools(ctx, owner, id, int64Value(params["expected_revision"]), true)
	if actionErr != nil {
		return nil, actionErr
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, toolDTO(tool))
	}
	return map[string]any{"tools": out}, nil
}

func (p *MCPActionPort) ensurePinnedTools(ctx context.Context, owner, id string, expectedRevision int64, refresh bool) (extensions.Installation, extensions.Version, []extensions.Tool, *actionbase.Error) {
	i, e := p.Service.Store.Get(ctx, owner, id)
	if e != nil {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(e)
	}
	if expectedRevision > 0 && i.Revision != expectedRevision {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(extensions.ErrRevisionConflict)
	}
	if i.State != "installed" || i.ActiveVersionID == "" {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(extensions.ErrRevisionConflict)
	}
	var v extensions.Version
	for _, x := range i.Versions {
		if x.VersionID == i.ActiveVersionID {
			v = x
		}
	}
	if v.VersionID == "" {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(extensions.ErrRevisionConflict)
	}
	if !refresh && len(v.Tools) > 0 {
		return i, v, append([]extensions.Tool(nil), v.Tools...), nil
	}
	if p.Client == nil {
		return extensions.Installation{}, extensions.Version{}, nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	client := p.Client(owner, i, v)
	if client == nil {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(extensions.ErrUnavailable)
	}
	tools, e := client.ListTools(ctx)
	if e != nil {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(e)
	}
	pinner, ok := p.Service.Store.(extensions.ToolPinner)
	if !ok {
		return extensions.Installation{}, extensions.Version{}, nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	tools, e = pinner.PinTools(ctx, owner, id, v.VersionID, i.Revision, tools)
	if e != nil {
		return extensions.Installation{}, extensions.Version{}, nil, mapMCPErr(e)
	}
	v.Tools = append([]extensions.Tool(nil), tools...)
	return i, v, tools, nil
}
func (p *MCPActionPort) execute(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	id := stringValue(params["installation_id"])
	name := stringValue(params["tool_name"])
	key := stringValue(params["idempotency_key"])
	expectedRevision := int64Value(params["expected_revision"])
	if !validUUIDString(id) || name == "" || !validUUIDString(key) || expectedRevision < 1 {
		return nil, actionbase.BadRequest("installation_id, tool_name and idempotency_key are required")
	}
	if _, _, _, actionErr := p.ensurePinnedTools(ctx, owner, id, expectedRevision, false); actionErr != nil {
		return nil, actionErr
	}
	r, e := p.Service.RequestExecute(ctx, extensions.ExecuteRequest{OwnerID: owner, InstallationID: id, IdempotencyKey: key, ToolName: name, ExpectedRevision: expectedRevision, Input: mustJSON(params["input"])})
	if e != nil {
		return nil, mapMCPErr(e)
	}
	return executeDTO(r), nil
}

func candidateParam(p map[string]any) (extensions.Candidate, *actionbase.Error) {
	return candidateValue(p["candidate"])
}
func inspectionParam(p map[string]any, c extensions.Candidate) (extensions.Inspection, *actionbase.Error) {
	raw, ok := objectValue(p["inspection"])
	if !ok || !exactKeys(raw, "candidate", "content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "network_grants", "secret_grants") {
		return extensions.Inspection{}, actionbase.BadRequest("inspection is invalid")
	}
	inspectedCandidate, candidateErr := candidateValue(raw["candidate"])
	if candidateErr != nil || !reflect.DeepEqual(inspectedCandidate, c) {
		return extensions.Inspection{}, actionbase.BadRequest("inspection candidate is invalid")
	}
	execution, ok := objectValue(raw["execution"])
	if !ok || !exactKeys(execution, "remote") {
		return extensions.Inspection{}, actionbase.BadRequest("only remote MCP execution is supported")
	}
	remote, ok := objectValue(execution["remote"])
	if !ok || !exactKeys(remote, "url", "credential_reference_id") {
		return extensions.Inspection{}, actionbase.BadRequest("remote MCP execution is invalid")
	}
	networkItems, ok := arrayValue(raw["network_grants"])
	if !ok {
		return extensions.Inspection{}, actionbase.BadRequest("network_grants must be an array")
	}
	network := make([]extensions.NetworkGrant, 0, len(networkItems))
	for _, item := range networkItems {
		grant, ok := objectValue(item)
		if !ok || !exactKeys(grant, "scheme", "host", "port", "path_prefix", "digest") {
			return extensions.Inspection{}, actionbase.BadRequest("network grant is invalid")
		}
		port := int64Value(grant["port"])
		if port < 1 || port > 65535 {
			return extensions.Inspection{}, actionbase.BadRequest("network grant is invalid")
		}
		network = append(network, extensions.NetworkGrant{
			Scheme:     stringValue(grant["scheme"]),
			Host:       stringValue(grant["host"]),
			Port:       uint32(port),
			PathPrefix: stringValue(grant["path_prefix"]),
			Digest:     stringValue(grant["digest"]),
		})
	}
	secretItems, ok := arrayValue(raw["secret_grants"])
	if !ok {
		return extensions.Inspection{}, actionbase.BadRequest("secret_grants must be an array")
	}
	secrets := make([]extensions.SecretGrant, 0, len(secretItems))
	for _, item := range secretItems {
		grant, ok := objectValue(item)
		configured, configuredOK := grant["configured"].(bool)
		if !ok || !configuredOK || !exactKeys(grant, "reference_id", "purpose", "binding_digest", "configured") {
			return extensions.Inspection{}, actionbase.BadRequest("secret grant is invalid")
		}
		purpose := strings.ReplaceAll(stringValue(grant["purpose"]), "-", "_")
		secrets = append(secrets, extensions.SecretGrant{
			ReferenceID:   stringValue(grant["reference_id"]),
			Purpose:       purpose,
			BindingDigest: stringValue(grant["binding_digest"]),
			Configured:    configured,
		})
	}
	i := extensions.Inspection{
		Candidate:       inspectedCandidate,
		ContentDigest:   stringValue(raw["content_digest"]),
		ManifestDigest:  stringValue(raw["manifest_digest"]),
		ExecutionDigest: stringValue(raw["execution_digest"]),
		NetworkDigest:   stringValue(raw["network_schema_digest"]),
		SecretDigest:    stringValue(raw["secret_schema_digest"]),
		Execution: extensions.Execution{Remote: &extensions.Endpoint{
			URL:                   stringValue(remote["url"]),
			CredentialReferenceID: stringValue(remote["credential_reference_id"]),
		}},
		NetworkGrants: network,
		SecretGrants:  secrets,
	}
	if i.Validate() != nil {
		return extensions.Inspection{}, actionbase.BadRequest("inspection is invalid")
	}
	return i, nil
}
func installationDTO(i extensions.Installation) map[string]any {
	proposed := ""
	if (i.State == "installing" || i.State == "updating") && len(i.Versions) > 0 {
		proposed = i.Versions[len(i.Versions)-1].VersionID
	}
	return map[string]any{
		"installation_id":     i.ID,
		"kind":                wireEnum(i.Candidate.Kind),
		"source":              wireEnum(i.Candidate.Source),
		"name":                i.Candidate.Name,
		"description":         i.Candidate.Description,
		"revision":            i.Revision,
		"state":               wireEnum(i.State),
		"active_version_id":   i.ActiveVersionID,
		"proposed_version_id": proposed,
		"candidate_id":        i.Candidate.ID,
		"transport":           wireTransport(i.Candidate.Transport),
		"versions":            versionsDTO(i.Versions),
		"created_at":          i.CreatedAt,
		"updated_at":          i.UpdatedAt,
	}
}
func versionsDTO(vs []extensions.Version) []any {
	out := make([]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, map[string]any{"version_id": v.VersionID, "content_digest": v.ContentDigest, "manifest_digest": v.ManifestDigest, "execution_digest": v.ExecutionDigest, "network_schema_digest": v.NetworkDigest, "secret_schema_digest": v.SecretDigest, "created_at": v.CreatedAt})
	}
	return out
}
func inspectionDTO(i extensions.Inspection) map[string]any {
	network := make([]any, 0, len(i.NetworkGrants))
	for _, grant := range i.NetworkGrants {
		network = append(network, map[string]any{"scheme": grant.Scheme, "host": grant.Host, "port": grant.Port, "path_prefix": grant.PathPrefix, "digest": grant.Digest})
	}
	secrets := make([]any, 0, len(i.SecretGrants))
	for _, grant := range i.SecretGrants {
		secrets = append(secrets, map[string]any{"reference_id": grant.ReferenceID, "purpose": wireEnum(grant.Purpose), "binding_digest": grant.BindingDigest, "configured": grant.Configured})
	}
	execution := map[string]any{}
	if i.Execution.Remote != nil {
		execution["remote"] = map[string]any{"url": i.Execution.Remote.URL, "credential_reference_id": i.Execution.Remote.CredentialReferenceID}
	}
	return map[string]any{"candidate": candidateDTO(i.Candidate), "content_digest": i.ContentDigest, "manifest_digest": i.ManifestDigest, "execution_digest": i.ExecutionDigest, "network_schema_digest": i.NetworkDigest, "secret_schema_digest": i.SecretDigest, "execution": execution, "network_grants": network, "secret_grants": secrets}
}

func candidateValue(value any) (extensions.Candidate, *actionbase.Error) {
	raw, ok := objectValue(value)
	if !ok || !exactKeys(raw, "id", "kind", "source", "name", "description", "pin", "transport") {
		return extensions.Candidate{}, actionbase.BadRequest("candidate is invalid")
	}
	pin, ok := objectValue(raw["pin"])
	if !ok || !exactKeys(pin, "registry_version", "registry_sha256", "git_commit", "git_sha256") {
		return extensions.Candidate{}, actionbase.BadRequest("candidate pin is invalid")
	}
	source, actionErr := internalMCPSource(stringValue(raw["source"]))
	if actionErr != nil || source == "" {
		return extensions.Candidate{}, actionbase.BadRequest("candidate source is invalid")
	}
	if stringValue(raw["kind"]) != extensions.KindMCP || stringValue(raw["transport"]) != "streamable-http" {
		return extensions.Candidate{}, actionbase.BadRequest("only remote HTTPS MCP candidates are supported")
	}
	candidate := extensions.Candidate{
		ID:          stringValue(raw["id"]),
		Kind:        extensions.KindMCP,
		Source:      source,
		Name:        stringValue(raw["name"]),
		Description: stringValue(raw["description"]),
		Transport:   extensions.TransportRemote,
		Pin: extensions.SourcePin{
			RegistryVersion: stringValue(pin["registry_version"]),
			RegistrySHA256:  stringValue(pin["registry_sha256"]),
			GitCommit:       stringValue(pin["git_commit"]),
			GitSHA256:       stringValue(pin["git_sha256"]),
		},
	}
	if candidate.Validate() != nil {
		return extensions.Candidate{}, actionbase.BadRequest("candidate is invalid")
	}
	return candidate, nil
}

func candidateDTO(candidate extensions.Candidate) map[string]any {
	return map[string]any{
		"id":          candidate.ID,
		"kind":        wireEnum(candidate.Kind),
		"source":      wireEnum(candidate.Source),
		"name":        candidate.Name,
		"description": candidate.Description,
		"transport":   wireTransport(candidate.Transport),
		"pin": map[string]any{
			"registry_version": candidate.Pin.RegistryVersion,
			"registry_sha256":  candidate.Pin.RegistrySHA256,
			"git_commit":       candidate.Pin.GitCommit,
			"git_sha256":       candidate.Pin.GitSHA256,
		},
	}
}

func toolDTO(tool extensions.Tool) map[string]any {
	return map[string]any{
		"name":                tool.Name,
		"description":         tool.Description,
		"input_schema_digest": tool.InputSchemaDigest,
	}
}

func executeDTO(result extensions.ExecuteResult) map[string]any {
	return map[string]any{
		"task_id":         result.TaskID,
		"confirmation_id": result.ConfirmationID,
	}
}

func inspectionMatchesAuthority(submitted, authoritative extensions.Inspection) bool {
	left := submitted
	right := authoritative
	// Tool schemas are not part of the public inspection DTO. They are fixed
	// independently by the first successful tools/list CAS.
	left.Tools = nil
	right.Tools = nil
	return authoritative.Validate() == nil && reflect.DeepEqual(left, right)
}

func internalMCPSource(source string) (string, *actionbase.Error) {
	source = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(source)), "-", "_")
	switch source {
	case "":
		return "", nil
	case "official_registry", "smithery", "glama", "github":
		return source, nil
	default:
		return "", actionbase.BadRequest("source is invalid")
	}
}

func internalMCPState(state string) (string, *actionbase.Error) {
	state = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(state)), "-", "_")
	switch state {
	case "", "draft", "installing", "installed", "updating", "uninstalling", "removed", "failed":
		return state, nil
	default:
		return "", actionbase.BadRequest("state is invalid")
	}
}

func wireEnum(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
}

func wireTransport(transport string) string {
	if transport == extensions.TransportRemote {
		return "streamable-http"
	}
	return wireEnum(transport)
}

func objectValue(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func arrayValue(value any) ([]any, bool) {
	if values, ok := value.([]any); ok {
		return values, true
	}
	if values, ok := value.([]map[string]any); ok {
		out := make([]any, len(values))
		for index := range values {
			out[index] = values[index]
		}
		return out, true
	}
	return nil, false
}

func exactKeys(value map[string]any, keys ...string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func lifecycleDTO(r extensions.LifecycleResult) map[string]any {
	return map[string]any{"installation": installationDTO(r.Installation), "task_id": r.TaskID, "confirmation_id": r.ConfirmationID}
}
func mcpConfirmationDTO(c extensions.Confirmation) map[string]any {
	return map[string]any{"confirmation_id": c.ID, "task_id": c.TaskID, "state": c.State, "revision": c.Revision, "binding": c.Binding, "expires_at": c.ExpiresAt}
}
func mapMCPErr(err error) *actionbase.Error {
	switch {
	case errors.Is(err, extensions.ErrInvalid):
		return actionbase.BadRequest("invalid MCP extension request")
	case errors.Is(err, extensions.ErrNotFound):
		return actionbase.CodedError(http.StatusNotFound, "mcp_not_found", "MCP installation was not found")
	case errors.Is(err, extensions.ErrRevisionConflict), errors.Is(err, extensions.ErrConflict), errors.Is(err, extensions.ErrIdempotencyConflict):
		return actionbase.CodedError(http.StatusConflict, "mcp_conflict", "MCP extension state conflict")
	case errors.Is(err, extensions.ErrLocalExecutionDisabled):
		return actionbase.CodedError(http.StatusPreconditionFailed, "local_execution_disabled", "local skill execution is disabled")
	case errors.Is(err, extensions.ErrUncertain):
		return actionbase.CodedError(http.StatusConflict, "extension_execution_uncertain", "execution outcome is uncertain; retry is forbidden")
	case errors.Is(err, extensions.ErrUnavailable), errors.Is(err, extensions.ErrMCPUnavailable):
		return actionbase.CodedError(http.StatusServiceUnavailable, "mcp_unavailable", "remote MCP capability is unavailable")
	case errors.Is(err, extensions.ErrMCPProtocol):
		return actionbase.CodedError(http.StatusBadGateway, "mcp_protocol_error", "remote MCP returned an invalid response")
	default:
		return actionbase.InternalError(err)
	}
}

func mapMCPCatalogErr(err error) *actionbase.Error {
	switch {
	case errors.Is(err, extensions.ErrInvalid):
		return actionbase.BadRequest("invalid MCP catalog request")
	case errors.Is(err, extensions.ErrNotFound):
		return actionbase.CodedError(http.StatusNotFound, "mcp_candidate_not_found", "MCP candidate was not found")
	case errors.Is(err, extensions.ErrUnavailable):
		return actionbase.CodedError(http.StatusServiceUnavailable, "mcp_catalog_unavailable", "MCP catalog is unavailable")
	default:
		return actionbase.CodedError(http.StatusBadGateway, "mcp_catalog_failed", "MCP catalog request failed")
	}
}
func validUUIDString(v string) bool {
	parsed, err := uuid.Parse(strings.TrimSpace(v))
	return err == nil && parsed.String() == strings.ToLower(strings.TrimSpace(v))
}
func int64Value(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case int32:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		if x <= uint64(^uint64(0)>>1) {
			return int64(x)
		}
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
