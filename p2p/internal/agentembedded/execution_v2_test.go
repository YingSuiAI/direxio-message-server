package agentembedded

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	action "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

type executionPortStub struct{}

func (executionPortStub) Handle(context.Context, string, string, map[string]any) (any, *action.Error) {
	return map[string]any{}, nil
}

type executionV2InvokeStub struct{}

func (executionV2InvokeStub) Invoke(context.Context, string, ExecutionV2InvokeRequest) (map[string]any, error) {
	return map[string]any{"accepted": true}, nil
}

type executionV2InvokeResultStub struct {
	result map[string]any
	err    error
}

func (s executionV2InvokeResultStub) Invoke(context.Context, string, ExecutionV2InvokeRequest) (map[string]any, error) {
	return s.result, s.err
}

func TestExecutionV2PortFailsClosedWithoutReadiness(t *testing.T) {
	p := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return false }})
	_, err := p.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.runs.get", map[string]any{"run_id": "11111111-1111-4111-8111-111111111111"})
	if err == nil || err.Code != "execution_v2_not_ready" {
		t.Fatalf("error = %#v, want stable execution_v2_not_ready", err)
	}
}

func TestExecutionV2CapabilitySplitDoesNotAdvertiseProviderAliases(t *testing.T) {
	ready := map[string]bool{"execution.v2": true, "execution.v2.plan": true}
	m := New(Config{ExecutionV2: executionPortStub{}, CapabilityReady: func(k string) bool { return ready[k] }})
	v, err := m.Handlers()["agent.backends.get"](context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	embedded := v.(map[string]any)["embedded"].(map[string]any)
	for _, got := range embedded["capabilities"].([]string) {
		if got == "ssh" || got == "http_api" || got == "execution.v2.run" || got == "execution.v2.bindings" {
			t.Fatalf("capability %q unexpectedly advertised", got)
		}
	}
}

func TestExecutionV2BindingReadsDoNotEnableHTTPInvoke(t *testing.T) {
	readOnly := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready:         func() bool { return true },
		BindingsReady: func() bool { return true },
	}).(*executionV2Port)
	if !readOnly.actionReady("artifacts.get") || !readOnly.actionReady("service_bindings.list") || !readOnly.actionReady("service_bindings.get") {
		t.Fatal("durable binding read surface did not become ready")
	}
	if readOnly.actionReady("service_bindings.invoke") {
		t.Fatal("binding read readiness enabled HTTP invocation")
	}

	invokeDisabled := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, BindingsReady: func() bool { return true },
		Invoke: executionV2InvokeStub{}, InvokeReady: func() bool { return false },
	}).(*executionV2Port)
	if invokeDisabled.actionReady("service_bindings.invoke") {
		t.Fatal("configured invoke port ignored its transport readiness gate")
	}

	invokeReady := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, BindingsReady: func() bool { return true },
		Invoke: executionV2InvokeStub{}, InvokeReady: func() bool { return true },
	}).(*executionV2Port)
	if !invokeReady.actionReady("service_bindings.invoke") {
		t.Fatal("fully configured schema-pinned HTTP invoke surface stayed closed")
	}
}

func TestExecutionV2InvokeRejectsUnsafeAdapterOutputAndNeverEchoesAdapterError(t *testing.T) {
	params := map[string]any{
		"binding_id": "11111111-1111-4111-8111-111111111111", "operation": "status",
		"expected_revision": 1, "idempotency_key": "22222222-2222-4222-8222-222222222222", "input": map[string]any{},
	}
	unsafe := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, BindingsReady: func() bool { return true }, InvokeReady: func() bool { return true },
		Invoke: executionV2InvokeResultStub{result: map[string]any{"nested": []any{map[string]any{"AUTHORIZATION": "Bearer never-return"}}}},
	})
	result, actionErr := unsafe.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.service_bindings.invoke", params)
	if result != nil || actionErr == nil || actionErr.Code != "execution_v2_invoke_output_rejected" || actionErr.Error == "Bearer never-return" {
		t.Fatalf("unsafe invoke result=%#v error=%#v", result, actionErr)
	}

	failure := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, BindingsReady: func() bool { return true }, InvokeReady: func() bool { return true },
		Invoke: executionV2InvokeResultStub{err: errors.New("Set-Cookie: session=never-return")},
	})
	_, actionErr = failure.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.service_bindings.invoke", params)
	if actionErr == nil || actionErr.Code != "execution_v2_invoke_failed" || actionErr.Error == "Set-Cookie: session=never-return" {
		t.Fatalf("adapter failure error=%#v", actionErr)
	}

	safePort := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, BindingsReady: func() bool { return true }, InvokeReady: func() bool { return true },
		Invoke: executionV2InvokeResultStub{result: map[string]any{"secret_ref": "credential-service-auth", "purpose": "service_auth", "ok": true}},
	})
	result, actionErr = safePort.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.service_bindings.invoke", params)
	if actionErr != nil || result.(map[string]any)["result"].(map[string]any)["secret_ref"] != "credential-service-auth" {
		t.Fatalf("safe invoke result=%#v error=%#v", result, actionErr)
	}
}

func TestExecutionV2ActionCapabilitiesSplitBindingReadsFromHTTPInvoke(t *testing.T) {
	for action, want := range map[string]string{
		"agent.execution.v2.artifacts.get":           "execution.v2.bindings",
		"agent.execution.v2.service_bindings.list":   "execution.v2.bindings",
		"agent.execution.v2.service_bindings.get":    "execution.v2.bindings",
		"agent.execution.v2.service_bindings.invoke": "execution.v2.transport.http_api",
	} {
		if got := executionV2Capability(action); got != want {
			t.Fatalf("%s capability = %q, want %q", action, got, want)
		}
	}
}

func TestExecutionV2HTTPAPICapabilityIsPublishedOnlyWhenReady(t *testing.T) {
	capabilities := func(httpReady bool) []string {
		ready := map[string]bool{
			"execution.v2":                    true,
			"execution.v2.bindings":           true,
			"execution.v2.transport.http_api": httpReady,
		}
		m := New(Config{ExecutionV2: executionPortStub{}, CapabilityReady: func(k string) bool { return ready[k] }})
		v, actionErr := m.Handlers()["agent.backends.get"](context.Background(), nil)
		if actionErr != nil {
			t.Fatal(actionErr)
		}
		return v.(map[string]any)["embedded"].(map[string]any)["capabilities"].([]string)
	}
	contains := func(values []string, want string) bool {
		for _, value := range values {
			if value == want {
				return true
			}
		}
		return false
	}
	closed := capabilities(false)
	if !contains(closed, "execution.v2.bindings") || contains(closed, "execution.v2.transport.http_api") {
		t.Fatalf("closed capabilities = %v", closed)
	}
	ready := capabilities(true)
	if !contains(ready, "execution.v2.bindings") || !contains(ready, "execution.v2.transport.http_api") {
		t.Fatalf("ready capabilities = %v", ready)
	}
}

func TestExecutionV2PublishesExactAWSSSMCapabilityOnlyWhenReady(t *testing.T) {
	ready := map[string]bool{
		"execution.v2":                   true,
		"execution.v2.run":               true,
		"execution.v2.transport.aws_ssm": true,
	}
	m := New(Config{ExecutionV2: executionPortStub{}, CapabilityReady: func(k string) bool { return ready[k] }})
	v, actionErr := m.Handlers()["agent.backends.get"](context.Background(), nil)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	capabilities := v.(map[string]any)["embedded"].(map[string]any)["capabilities"].([]string)
	found := false
	for _, got := range capabilities {
		if got == "transport.aws_ssm" {
			t.Fatal("legacy transport capability alias advertised")
		}
		if got == "execution.v2.transport.aws_ssm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("capabilities = %v", capabilities)
	}
}

func TestExecutionV2PortRejectsUnknownAndOwnerlessRequests(t *testing.T) {
	p := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return true }})
	_, err := p.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.runs.get", map[string]any{"run_id": "11111111-1111-4111-8111-111111111111", "alias": "run_id"})
	if err == nil || err.Code != "unknown_field" || err.Status != 400 {
		t.Fatalf("unknown field error = %#v", err)
	}
	_, err = p.Handle(context.Background(), "", "agent.execution.v2.runs.get", map[string]any{"run_id": "11111111-1111-4111-8111-111111111111"})
	if err == nil || err.Code != "owner_required" || err.Status != 401 {
		t.Fatalf("owner error = %#v", err)
	}
}

func TestExecutionV2PortRejectsBareAliases(t *testing.T) {
	p := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return true }})
	_, err := p.Handle(context.Background(), "@owner:example.test", "runs.get", map[string]any{})
	if err == nil || err.Code != "execution_v2_action_not_found" {
		t.Fatalf("bare alias error = %#v", err)
	}
}

func TestExecutionV2ActionNamesAreExact(t *testing.T) {
	if len(executionV2Actions()) != 33 {
		t.Fatalf("action count = %d", len(executionV2Actions()))
	}
	for _, name := range executionV2Actions() {
		if containsExecutionV2Action(name) == false {
			t.Errorf("missing action %q", name)
		}
	}
}

func TestExecutionV2RuntimeFieldsMatchPublishedSchemas(t *testing.T) {
	for bare, allowed := range executionV2AllowedFields {
		spec, ok := serviceapi.ActionSpecFor("agent.execution.v2." + bare)
		if !ok || spec.Schema == nil {
			t.Fatalf("missing schema for %s", bare)
		}
		runtimeKeys := make([]string, 0, len(allowed))
		for key := range allowed {
			runtimeKeys = append(runtimeKeys, key)
		}
		schemaKeys := make([]string, 0, len(spec.Schema.Request))
		for key := range spec.Schema.Request {
			schemaKeys = append(schemaKeys, key)
		}
		sort.Strings(runtimeKeys)
		sort.Strings(schemaKeys)
		if !reflect.DeepEqual(runtimeKeys, schemaKeys) {
			t.Fatalf("%s runtime keys %v != schema keys %v", bare, runtimeKeys, schemaKeys)
		}
	}
}

func TestExecutionV2RejectsRawPlanAndInvalidNumbersBeforePorts(t *testing.T) {
	port := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready:     func() bool { return true },
		PlanReady: func() bool { return true },
	})
	_, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.plans.create", map[string]any{
		"project_id": "11111111-1111-4111-8111-111111111111", "analysis_id": "22222222-2222-4222-8222-222222222222",
		"target_id": "33333333-3333-4333-8333-333333333333", "target_revision": 1,
		"purpose": "service", "intent": "container-service", "recipe_id": "generic-container-service",
		"idempotency_key": "44444444-4444-4444-8444-444444444444", "plan": map[string]any{"digest": "client-owned"},
	})
	if actionErr == nil || actionErr.Code != "unknown_field" {
		t.Fatalf("raw plan error = %#v", actionErr)
	}
	_, actionErr = port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.plans.list", map[string]any{"page_size": 1.5})
	if actionErr == nil || actionErr.Status != 400 {
		t.Fatalf("fractional page error = %#v", actionErr)
	}
}

func TestExecutionV2GenericContainerPlanSelectionIsInitialDeployOnly(t *testing.T) {
	port := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return true }, PlanReady: func() bool { return true }})
	base := map[string]any{
		"project_id": "11111111-1111-4111-8111-111111111111", "analysis_id": "22222222-2222-4222-8222-222222222222",
		"target_id": "33333333-3333-4333-8333-333333333333", "target_revision": 1,
		"purpose": "service", "intent": "deploy", "recipe_id": coreexecution.RecipeGenericContainerService,
		"idempotency_key": "44444444-4444-4444-8444-444444444444",
	}
	for _, intent := range []string{"upgrade", "repair"} {
		params := make(map[string]any, len(base))
		for key, value := range base {
			params[key] = value
		}
		params["intent"] = intent
		if _, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.plans.create", params); actionErr == nil || actionErr.Status != 400 {
			t.Fatalf("generic container %s selection was accepted: %#v", intent, actionErr)
		}
	}
	job := make(map[string]any, len(base))
	for key, value := range base {
		job[key] = value
	}
	job["purpose"] = "job"
	if _, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.plans.create", job); actionErr == nil || actionErr.Status != 400 {
		t.Fatalf("generic container job selection was accepted: %#v", actionErr)
	}
}

type executionV2AnalyzeStub struct {
	got ExecutionV2AnalyzeRequest
	err error
}

type executionV2TargetImportStub struct {
	got ExecutionV2TargetImportRequest
}

type executionV2TargetReserveStub struct {
	got ExecutionV2TargetReserveRequest
}

func (s *executionV2TargetReserveStub) ReserveTarget(_ context.Context, _ string, in ExecutionV2TargetReserveRequest) (coreexecution.ExecutionTarget, error) {
	s.got = in
	return coreexecution.ExecutionTarget{ID: "77777777-7777-4777-8777-777777777777", Revision: 1}, nil
}

func (s *executionV2TargetImportStub) ImportTarget(_ context.Context, _ string, in ExecutionV2TargetImportRequest) (ExecutionV2TargetImportResult, error) {
	s.got = in
	return ExecutionV2TargetImportResult{
		Target:        coreexecution.ExecutionTarget{ID: "55555555-5555-4555-8555-555555555555", Revision: 1},
		ObservationID: "66666666-6666-4666-8666-666666666666",
		Observation:   coreexecution.TargetObservation{TargetID: "55555555-5555-4555-8555-555555555555", TargetRevision: 1},
	}, nil
}

func TestExecutionV2TargetImportAcceptsOnlyBootstrapIdentityAndIsTransportFenced(t *testing.T) {
	stub := &executionV2TargetImportStub{}
	port := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, TargetImport: stub,
		TargetImportReady: func() bool { return true }, TransportAWSReady: func() bool { return true },
	})
	params := map[string]any{
		"credential_id": "11111111-1111-4111-8111-111111111111", "credential_revision": 7,
		"instance_id": "i-0123456789abcdef0", "idempotency_key": "22222222-2222-4222-8222-222222222222",
	}
	result, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.import", params)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	if stub.got.CredentialRevision != 7 || stub.got.InstanceID != params["instance_id"] {
		t.Fatalf("typed import request=%+v", stub.got)
	}
	response := result.(map[string]any)
	if response["observation_id"] == "" || response["target"] == nil || response["observation"] == nil {
		t.Fatalf("import response=%+v", response)
	}
	if response["target"].(map[string]any)["owner_id"] != "@owner:example.test" || response["observation"].(map[string]any)["owner_id"] != "@owner:example.test" {
		t.Fatalf("import response omitted authenticated owner projection: %+v", response)
	}

	params["account_id"] = "123456789012"
	if _, actionErr = port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.import", params); actionErr == nil || actionErr.Code != "unknown_field" {
		t.Fatalf("caller authority field error=%+v", actionErr)
	}
	delete(params, "account_id")
	closed := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, TargetImport: stub,
		TargetImportReady: func() bool { return true }, TransportAWSReady: func() bool { return false },
	})
	if _, actionErr = closed.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.import", params); actionErr == nil || actionErr.Code != "execution_v2_not_ready" {
		t.Fatalf("transport readiness error=%+v", actionErr)
	}
}

func TestExecutionV2TargetReserveIsTypedAndFailsClosedWithoutProvisionReadiness(t *testing.T) {
	stub := &executionV2TargetReserveStub{}
	params := map[string]any{
		"credential_id": "11111111-1111-4111-8111-111111111111", "credential_revision": 7,
		"instance_type": "t3.small", "volume_gib": 20,
		"idempotency_key": "22222222-2222-4222-8222-222222222222",
	}
	port := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return true }, TargetReserve: stub, TargetReserveReady: func() bool { return true }})
	result, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.reserve", params)
	if actionErr != nil || result.(map[string]any)["target"] == nil || stub.got.InstanceType != "t3.small" || stub.got.VolumeGiB != 20 {
		t.Fatalf("result=%+v request=%+v err=%+v", result, stub.got, actionErr)
	}
	if result.(map[string]any)["target"].(map[string]any)["owner_id"] != "@owner:example.test" {
		t.Fatalf("reserve response omitted authenticated owner projection: %+v", result)
	}
	params["region"] = "us-west-2"
	if _, actionErr = port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.reserve", params); actionErr == nil || actionErr.Code != "unknown_field" {
		t.Fatalf("caller region error=%+v", actionErr)
	}
	delete(params, "region")
	closed := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return true }, TargetReserve: stub, TargetReserveReady: func() bool { return false }})
	if _, actionErr = closed.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.targets.reserve", params); actionErr == nil || actionErr.Code != "execution_v2_not_ready" {
		t.Fatalf("readiness error=%+v", actionErr)
	}
}

func TestExecutionV2ProvisionCapabilityIsAbsentUntilLifecycleReadiness(t *testing.T) {
	ready := false
	m := New(Config{ExecutionV2: executionPortStub{}, CapabilityReady: func(token string) bool {
		if token == "execution.v2" {
			return ready
		}
		return ready && token == "execution.v2.provision"
	}})
	capabilities := func() []string {
		value, actionErr := m.Handlers()["agent.backends.get"](context.Background(), nil)
		if actionErr != nil {
			t.Fatal(actionErr)
		}
		return value.(map[string]any)["embedded"].(map[string]any)["capabilities"].([]string)
	}
	contains := func(values []string, want string) bool {
		for _, value := range values {
			if value == want {
				return true
			}
		}
		return false
	}
	if got := capabilities(); contains(got, "execution.v2.provision") {
		t.Fatalf("provision advertised before lifecycle readiness: %v", got)
	}
	ready = true
	if got := capabilities(); !contains(got, "execution.v2.provision") {
		t.Fatalf("provision missing after lifecycle readiness: %v", got)
	}
}

func (s *executionV2AnalyzeStub) Analyze(_ context.Context, _ string, in ExecutionV2AnalyzeRequest) (coreexecution.ProjectAnalysis, error) {
	s.got = in
	return coreexecution.ProjectAnalysis{ProjectID: in.ProjectID}, s.err
}

func TestExecutionV2AnalyzerReceivesTypedPinnedSourceAndErrorsAreRedacted(t *testing.T) {
	stub := &executionV2AnalyzeStub{}
	port := NewExecutionV2ActionPort(ExecutionV2Config{
		Ready: func() bool { return true }, PlanReady: func() bool { return true }, Analyze: stub,
	})
	params := map[string]any{
		"project_id":      "11111111-1111-4111-8111-111111111111",
		"idempotency_key": "22222222-2222-4222-8222-222222222222",
		"source": map[string]any{
			"kind": "git_https", "location": "https://example.test/repo.git",
			"commit": "0123456789abcdef0123456789abcdef01234567", "immutable": true,
		},
	}
	if _, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.projects.analyze", params); actionErr != nil {
		t.Fatal(actionErr)
	}
	if stub.got.ProjectID != params["project_id"] || stub.got.Source.Commit == "" || !stub.got.Source.Immutable {
		t.Fatalf("typed request = %#v", stub.got)
	}
	stub.err = errors.New("postgres password=do-not-leak")
	_, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.projects.analyze", params)
	if actionErr == nil || actionErr.Code != "execution_v2_internal" || actionErr.Error == stub.err.Error() {
		t.Fatalf("redacted error = %#v", actionErr)
	}
}

func TestExecutionV2PublicAnalysisAndPlanChildrenCarryAuthenticatedOwner(t *testing.T) {
	const owner = "@owner:example.test"
	analysis := executionV2AnalysisMap(owner, coreexecution.ProjectAnalysis{
		AnalysisID: "11111111-1111-4111-8111-111111111111",
		ProjectID:  "22222222-2222-4222-8222-222222222222",
	})
	if analysis["owner_id"] != owner {
		t.Fatalf("analysis owner_id = %#v", analysis["owner_id"])
	}

	plan := executionV2PlanMap(owner, coreexecution.ExecutionPlan{
		OwnerID: "untrusted-internal-owner",
		Targets: []coreexecution.ExecutionTarget{{
			ID:       "33333333-3333-4333-8333-333333333333",
			Revision: 2,
		}},
		Stages: []coreexecution.ExecutionStage{{
			StageKey: "deploy",
			Revision: 1,
		}},
	})
	if plan["owner_id"] != owner {
		t.Fatalf("plan owner_id = %#v", plan["owner_id"])
	}
	targets, ok := plan["targets"].([]any)
	if !ok || len(targets) != 1 || targets[0].(map[string]any)["owner_id"] != owner {
		t.Fatalf("plan targets owner projection = %#v", plan["targets"])
	}
	stages, ok := plan["stages"].([]any)
	if !ok || len(stages) != 1 || stages[0].(map[string]any)["owner_id"] != owner {
		t.Fatalf("plan stages owner projection = %#v", plan["stages"])
	}
}
