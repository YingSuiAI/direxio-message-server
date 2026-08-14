package agentgateway

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type cloudWorkerPublicFixture struct {
	Schema           string                    `json:"schema"`
	Plan             map[string]any            `json:"plan"`
	Run              map[string]any            `json:"run"`
	RunEvent         map[string]any            `json:"run_event"`
	Confirmation     map[string]any            `json:"confirmation"`
	References       map[string]map[string]any `json:"references"`
	Artifacts        []map[string]any          `json:"artifacts"`
	ArtifactDownload map[string]any            `json:"artifact_download"`
	Completion       map[string]any            `json:"completion"`
}

func loadCloudWorkerPublicFixture(t *testing.T) cloudWorkerPublicFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/cloud_worker_public_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		t.Fatalf("invalid Cloud Worker public fixture: %v", err)
	}
	wantKeys := []string{"artifact_download", "artifacts", "completion", "confirmation", "plan", "references", "run", "run_event", "schema"}
	if got := sortedRawMessageKeys(topLevel); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("Cloud Worker public fixture top-level keys=%v want=%v", got, wantKeys)
	}
	var fixture cloudWorkerPublicFixture
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Schema != "cloud_worker_public_fixture/v1" || fixture.Plan == nil || fixture.Run == nil || fixture.RunEvent == nil || fixture.Confirmation == nil || len(fixture.References) != 3 || len(fixture.Artifacts) != 1 || fixture.ArtifactDownload == nil || fixture.Completion == nil {
		t.Fatalf("invalid Cloud Worker public fixture: %v", err)
	}
	return fixture
}

func sortedRawMessageKeys(value map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestCloudWorkerExecutionV2ResultsMatchPinnedPublicFixture(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	runEventSequence, ok := cloudInteger(fixture.RunEvent["sequence"])
	if !ok || runEventSequence < 1 {
		t.Fatal("fixture run event sequence is invalid")
	}
	runEventsRequest := cloudWorkerRequest("run_id", fixture.Run["run_id"])
	runEventsRequest["after_sequence"] = runEventSequence - 1
	tests := []struct {
		action  string
		request map[string]any
		output  map[string]any
	}{
		{"agent.execution.v2.plans.get", cloudWorkerRequest("plan_id", fixture.Plan["plan_id"]), map[string]any{"plan": fixture.Plan}},
		{"agent.execution.v2.plans.list", cloudWorkerRequest("", nil), map[string]any{"plans": []any{fixture.Plan}, "next_page_token": ""}},
		{"agent.execution.v2.runs.get", cloudWorkerRequest("run_id", fixture.Run["run_id"]), map[string]any{"run": fixture.Run}},
		{"agent.execution.v2.runs.list", cloudWorkerRequest("", nil), map[string]any{"runs": []any{fixture.Run}, "next_page_token": ""}},
		{"agent.execution.v2.runs.cancel", cloudWorkerRequest("run_id", fixture.Run["run_id"]), map[string]any{"run": fixture.Run}},
		{"agent.execution.v2.artifacts.get", cloudWorkerRequest("artifact_id", fixture.Artifacts[0]["artifact_id"]), map[string]any{"artifact": fixture.Artifacts[0]}},
		{"agent.execution.v2.artifacts.download", map[string]any{
			"record_kind": "cloud_worker", "artifact_id": fixture.Artifacts[0]["artifact_id"], "offset_bytes": float64(0), "max_chunk_bytes": float64(512 << 10),
		}, fixture.ArtifactDownload},
		{"agent.execution.v2.runs.events", runEventsRequest, map[string]any{"events": []any{fixture.RunEvent}, "next_sequence": fixture.RunEvent["sequence"], "history_truncated": false}},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			if _, err := adaptActionResultForRequestWithAuthority(test.action, test.request, test.output, authority); err != nil {
				t.Fatalf("fixture rejected: %v", err)
			}
		})
	}
}

func TestCloudWorkerRunEventsPinBoundedSecretFreeProgressAndCursor(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	request := cloudWorkerRequest("run_id", fixture.Run["run_id"])
	runEventSequence, ok := cloudInteger(fixture.RunEvent["sequence"])
	if !ok || runEventSequence < 1 {
		t.Fatal("fixture run event sequence is invalid")
	}
	request["after_sequence"] = runEventSequence - 1
	validProgress := func() map[string]any {
		return map[string]any{
			"phase": "running", "elapsed_ms": float64(1200), "last_activity_at": fixture.RunEvent["at"],
			"cpu_time_ms": float64(800), "memory_high_water_bytes": float64(32 << 20),
			"invocation_count": float64(2), "uploaded_bytes": float64(0), "output_truncated": false,
		}
	}
	validEvent := func() map[string]any {
		event := cloneParams(fixture.RunEvent)
		event["type"] = "worker_progress"
		event["progress"] = validProgress()
		return event
	}
	validate := func(output map[string]any) error {
		_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.runs.events", request, output, authority)
		return err
	}
	validOutput := func(event map[string]any) map[string]any {
		return map[string]any{"events": []any{event}, "next_sequence": event["sequence"], "history_truncated": false}
	}
	if err := validate(validOutput(validEvent())); err != nil {
		t.Fatalf("valid worker progress rejected: %v", err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"missing truncation flag": func(output map[string]any) { delete(output, "history_truncated") },
		"non-boolean truncation flag": func(output map[string]any) {
			output["history_truncated"] = "false"
		},
		"unknown envelope field": func(output map[string]any) { output["s3_url"] = "s3://private/internal" },
	} {
		t.Run(name, func(t *testing.T) {
			output := validOutput(validEvent())
			mutate(output)
			err := validate(output)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid events envelope accepted: %v", err)
			}
			if strings.Contains(err.Error(), "s3://private/internal") {
				t.Fatal("events validation reflected a private storage address")
			}
		})
	}

	for _, field := range []string{"phase", "elapsed_ms", "last_activity_at", "cpu_time_ms", "memory_high_water_bytes", "invocation_count", "uploaded_bytes", "output_truncated"} {
		t.Run("missing progress "+field, func(t *testing.T) {
			event := validEvent()
			delete(event["progress"].(map[string]any), field)
			if err := validate(validOutput(event)); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("incomplete progress accepted: %v", err)
			}
		})
	}
	for _, field := range []string{"text", "raw", "model_text", "secret", "stderr", "env", "s3_url", "bucket", "key"} {
		t.Run("forbidden progress "+field, func(t *testing.T) {
			event := validEvent()
			event["progress"].(map[string]any)[field] = "protected-value"
			err := validate(validOutput(event))
			if !errors.Is(err, ErrInvalidActionResult) || strings.Contains(err.Error(), "protected-value") {
				t.Fatalf("private progress field handling err=%v", err)
			}
		})
	}

	for name, mutate := range map[string]func(map[string]any){
		"unknown phase":         func(progress map[string]any) { progress["phase"] = "shell_output" },
		"padded phase":          func(progress map[string]any) { progress["phase"] = " running" },
		"negative elapsed":      func(progress map[string]any) { progress["elapsed_ms"] = float64(-1) },
		"elapsed over max":      func(progress map[string]any) { progress["elapsed_ms"] = float64(86400001) },
		"cpu over max":          func(progress map[string]any) { progress["cpu_time_ms"] = float64(604800001) },
		"memory over max":       func(progress map[string]any) { progress["memory_high_water_bytes"] = float64(68719476737) },
		"invocations over max":  func(progress map[string]any) { progress["invocation_count"] = float64(1000001) },
		"upload over max":       func(progress map[string]any) { progress["uploaded_bytes"] = float64(9437185) },
		"invalid activity time": func(progress map[string]any) { progress["last_activity_at"] = "not-a-time" },
		"activity after event": func(progress map[string]any) {
			at, err := time.Parse(time.RFC3339Nano, fixture.RunEvent["at"].(string))
			if err != nil {
				t.Fatal(err)
			}
			progress["last_activity_at"] = at.Add(time.Second).Format(time.RFC3339Nano)
		},
		"non-boolean truncation": func(progress map[string]any) { progress["output_truncated"] = "false" },
	} {
		t.Run(name, func(t *testing.T) {
			event := validEvent()
			mutate(event["progress"].(map[string]any))
			if err := validate(validOutput(event)); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid progress accepted: %v", err)
			}
		})
	}

	t.Run("worker progress requires progress", func(t *testing.T) {
		event := validEvent()
		delete(event, "progress")
		if err := validate(validOutput(event)); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("worker_progress without progress accepted: %v", err)
		}
	})
	t.Run("lifecycle event forbids progress", func(t *testing.T) {
		event := validEvent()
		event["type"] = "execution_running"
		if err := validate(validOutput(event)); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("lifecycle event progress accepted: %v", err)
		}
	})
	t.Run("truncated cursor may resume at retained history", func(t *testing.T) {
		event := validEvent()
		event["sequence"] = float64(7)
		cursorRequest := cloneParams(request)
		cursorRequest["after_sequence"] = float64(2)
		_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.runs.events", cursorRequest, map[string]any{
			"events": []any{event}, "next_sequence": float64(7), "history_truncated": true,
		}, authority)
		if err != nil {
			t.Fatalf("truncated retained cursor rejected: %v", err)
		}
	})
	t.Run("truncated cursor requires a retained history gap", func(t *testing.T) {
		event := validEvent()
		event["sequence"] = float64(3)
		cursorRequest := cloneParams(request)
		cursorRequest["after_sequence"] = float64(2)
		_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.runs.events", cursorRequest, map[string]any{
			"events": []any{event}, "next_sequence": float64(3), "history_truncated": true,
		}, authority)
		if !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("truncated cursor without a retained history gap accepted: %v", err)
		}
	})
	t.Run("non-truncated cursor rejects a leading gap", func(t *testing.T) {
		event := validEvent()
		event["sequence"] = float64(runEventSequence + 1)
		if err := validate(validOutput(event)); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("non-truncated leading gap accepted: %v", err)
		}
	})
	t.Run("truncated retained suffix rejects an internal gap", func(t *testing.T) {
		first := validEvent()
		first["sequence"] = float64(7)
		second := validEvent()
		second["event_id"] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		second["sequence"] = float64(9)
		cursorRequest := cloneParams(request)
		cursorRequest["after_sequence"] = float64(2)
		_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.runs.events", cursorRequest, map[string]any{
			"events": []any{first, second}, "next_sequence": float64(9), "history_truncated": true,
		}, authority)
		if !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("truncated internal gap accepted: %v", err)
		}
	})
	t.Run("empty cursor pages pin next sequence", func(t *testing.T) {
		cursorRequest := cloneParams(request)
		cursorRequest["after_sequence"] = float64(2)
		for name, output := range map[string]map[string]any{
			"current window":   {"events": []any{}, "next_sequence": float64(2), "history_truncated": false},
			"truncated window": {"events": []any{}, "next_sequence": float64(7), "history_truncated": true},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.runs.events", cursorRequest, output, authority); err != nil {
					t.Fatalf("valid empty cursor page rejected: %v", err)
				}
			})
		}
	})
}

func TestCloudWorkerChatReferencesConsumePinnedPublicFixture(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	references := []any{
		fixture.References["plan"],
		fixture.References["run"],
		fixture.References["confirmation"],
	}
	response := canonicalChatResponseForTest(
		"Cloud task completed",
		references,
		[]any{fixture.Plan["task_id"]},
		[]any{fixture.Plan["plan_id"]},
	)
	got, err := adaptActionResultForRequestWithAuthority(
		"agent.chat", nil, response, cloudWorkerFixtureAuthority(t, fixture),
	)
	if err != nil {
		t.Fatalf("golden Cloud Worker references rejected: %v", err)
	}
	if !reflect.DeepEqual(got["references"], references) ||
		!reflect.DeepEqual(got["related_task_ids"], []any{fixture.Plan["task_id"]}) ||
		!reflect.DeepEqual(got["related_plan_ids"], []any{fixture.Plan["plan_id"]}) {
		t.Fatalf("golden Cloud Worker linkage projection=%#v", got)
	}
}

func TestCloudWorkerArtifactDownloadResultFailsClosed(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	request := map[string]any{
		"record_kind": "cloud_worker", "artifact_id": fixture.Artifacts[0]["artifact_id"],
		"offset_bytes": float64(0), "max_chunk_bytes": float64(512 << 10),
	}
	finalChunk := cloneParams(fixture.ArtifactDownload)
	finalChunk["size_bytes"] = float64(5)
	finalChunk["eof"] = true
	if _, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.artifacts.download", request, finalChunk, authority); err != nil {
		t.Fatalf("non-empty final artifact chunk rejected: %v", err)
	}

	for name, mutation := range map[string]func(map[string]any){
		"unknown storage field":  func(result map[string]any) { result["s3_url"] = "s3://private/internal" },
		"foreign owner":          func(result map[string]any) { result["owner_id"] = "@foreign:example.test" },
		"foreign generation":     func(result map[string]any) { result["account_generation"] = float64(8) },
		"wrong artifact":         func(result map[string]any) { result["artifact_id"] = "99999999-9999-4999-8999-999999999999" },
		"bad base64":             func(result map[string]any) { result["data_base64"] = "aGVsbG8" },
		"bad chunk digest":       func(result map[string]any) { result["chunk_sha256"] = result["artifact_sha256"] },
		"padded artifact digest": func(result map[string]any) { result["artifact_sha256"] = " " + result["artifact_sha256"].(string) },
		"padded execution id":    func(result map[string]any) { result["execution_id"] = " " + result["execution_id"].(string) },
		"range gap":              func(result map[string]any) { result["next_offset_bytes"] = float64(6) },
		"premature eof":          func(result map[string]any) { result["eof"] = true },
		"empty chunk": func(result map[string]any) {
			result["data_base64"] = ""
			result["chunk_sha256"] = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
			result["next_offset_bytes"] = result["offset_bytes"]
		},
		"oversize response": func(result map[string]any) {
			result["data_base64"] = string(make([]byte, 700000))
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := cloneParams(fixture.ArtifactDownload)
			mutation(result)
			_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.artifacts.download", request, result, authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid artifact download result accepted: %v", err)
			}
			if strings.Contains(err.Error(), "s3://private/internal") {
				t.Fatal("result validation reflected a private storage address")
			}
		})
	}
	tooSmallRequest := cloneParams(request)
	tooSmallRequest["max_chunk_bytes"] = float64(4)
	if _, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.artifacts.download", tooSmallRequest, fixture.ArtifactDownload, authority); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("chunk larger than requested maximum accepted: %v", err)
	}
}

func TestCloudWorkerArtifactDownloadKeepsDirectPublicEnvelope(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	request := map[string]any{
		"record_kind": "cloud_worker", "artifact_id": fixture.Artifacts[0]["artifact_id"],
		"offset_bytes": float64(0), "max_chunk_bytes": float64(512 << 10),
	}
	got, err := adaptActionResultForRequestWithAuthority(
		"agent.execution.v2.artifacts.download", request, fixture.ArtifactDownload,
		cloudWorkerFixtureAuthority(t, fixture),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fixture.ArtifactDownload) {
		t.Fatalf("artifact download projection = %#v, want direct response %#v", got, fixture.ArtifactDownload)
	}
	if _, wrapped := got["chunk"]; wrapped {
		t.Fatal("artifact download unexpectedly added a chunk envelope")
	}
}

func TestCloudWorkerExecutionV2ResultsFailClosedOnShapeDrift(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	request := cloudWorkerRequest("plan_id", fixture.Plan["plan_id"])
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing reuse preference", func(plan map[string]any) { delete(plan, "persistent_worker_reuse") }},
		{"unknown field", func(plan map[string]any) { plan["s3_url"] = "s3://private/internal" }},
		{"retired digest", func(plan map[string]any) { plan["digest"] = strings.Repeat("a", 64) }},
		{"retired recipe", func(plan map[string]any) { plan["recipe_id"] = "ephemeral-pi-task" }},
		{"producer status drift", func(plan map[string]any) { plan["status"] = "queued" }},
		{"invalid generation", func(plan map[string]any) { plan["account_generation"] = float64(0) }},
	}

	t.Run("persistent terminal run does not require destruction", func(t *testing.T) {
		run := cloneParams(fixture.Run)
		if _, err := adaptActionResultForRequestWithAuthority(
			"agent.execution.v2.runs.get",
			cloudWorkerRequest("run_id", run["run_id"]),
			map[string]any{"run": run}, authority,
		); err != nil {
			t.Fatalf("persistent terminal run rejected: %v", err)
		}
	})

	for _, field := range []string{"plan_digest", "digest", "workspace_mode", "quote_digest", "execution_digest"} {
		t.Run("retired run field "+field, func(t *testing.T) {
			run := cloneParams(fixture.Run)
			run[field] = strings.Repeat("b", 64)
			_, err := adaptActionResultForRequestWithAuthority(
				"agent.execution.v2.runs.get",
				cloudWorkerRequest("run_id", run["run_id"]),
				map[string]any{"run": run}, authority,
			)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("retired run field %s was accepted: %v", field, err)
			}
		})
	}

	t.Run("run and execution identities are independent", func(t *testing.T) {
		run := cloneParams(fixture.Run)
		if run["run_id"] == run["execution_id"] {
			t.Fatal("fixture does not exercise independent run and execution identities")
		}
		if _, err := adaptActionResultForRequestWithAuthority(
			"agent.execution.v2.runs.get",
			cloudWorkerRequest("run_id", run["run_id"]),
			map[string]any{"run": run}, authority,
		); err != nil {
			t.Fatalf("independent run and execution identities rejected: %v", err)
		}
	})
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			plan := cloneParams(fixture.Plan)
			mutation.mutate(plan)
			_, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.plans.get", request, map[string]any{"plan": plan}, authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("shape drift err=%v", err)
			}
		})
	}
	// The same generic action remains on its existing projection when the
	// discriminator is omitted; Cloud Worker validation is never guessed from
	// an ID or response shape.
	if _, err := adaptActionResultForRequest("agent.execution.v2.plans.get", map[string]any{}, map[string]any{"plan": map[string]any{"legacy": true}}); err != nil {
		t.Fatalf("generic Execution V2 result was routed through Cloud Worker validation: %v", err)
	}
}

func TestCloudWorkerPublicProjectionHidesSecretLocatorsAndPinsCancelIntent(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)

	plan := cloneParams(fixture.Plan)
	plan["secret_grants"] = []any{map[string]any{
		"purpose": "model_api",
	}}
	if _, err := adaptActionResultForRequestWithAuthority(
		"agent.execution.v2.plans.get",
		cloudWorkerRequest("plan_id", plan["plan_id"]),
		map[string]any{"plan": plan}, authority,
	); err != nil {
		t.Fatalf("purpose-only secret grant rejected: %v", err)
	}
	for _, forbidden := range []string{"configured", "count", "reference_id", "binding_digest", "secret_ref", "credential_id"} {
		t.Run("secret grant "+forbidden, func(t *testing.T) {
			drifted := cloneParams(plan)
			drifted["secret_grants"] = []any{map[string]any{
				"purpose": "model_api", forbidden: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			}}
			_, err := adaptActionResultForRequestWithAuthority(
				"agent.execution.v2.plans.get",
				cloudWorkerRequest("plan_id", drifted["plan_id"]),
				map[string]any{"plan": drifted}, authority,
			)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("private secret locator %s was accepted: %v", forbidden, err)
			}
		})
	}
	quote, ok := fixture.Plan["quote"].(map[string]any)
	if !ok {
		t.Fatal("fixture quote is malformed")
	}
	if _, exposed := quote["basis_digest"]; exposed {
		t.Fatal("public quote exposes the private authorization basis digest")
	}

	for _, mutation := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing", func(run map[string]any) { delete(run, "cancellation_requested") }},
		{"not boolean", func(run map[string]any) { run["cancellation_requested"] = "false" }},
	} {
		t.Run("cancellation_requested "+mutation.name, func(t *testing.T) {
			run := cloneParams(fixture.Run)
			mutation.mutate(run)
			_, err := adaptActionResultForRequestWithAuthority(
				"agent.execution.v2.runs.get",
				cloudWorkerRequest("run_id", run["run_id"]),
				map[string]any{"run": run}, authority,
			)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid cancellation_requested was accepted: %v", err)
			}
		})
	}
}

func TestCloudWorkerExecutionV2ResultsRejectForeignPreparedAuthority(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)

	foreignPlan := cloneParams(fixture.Plan)
	foreignPlan["owner_id"] = "@foreign:example.test"
	foreignRun := cloneParams(fixture.Run)
	foreignRun["account_generation"] = float64(authority.accountGeneration + 1)
	foreignArtifact := cloneParams(fixture.Artifacts[0])
	foreignArtifact["owner_id"] = "@foreign:example.test"
	foreignGenerationArtifact := cloneParams(fixture.Artifacts[0])
	foreignGenerationArtifact["account_generation"] = float64(authority.accountGeneration + 1)
	foreignEvent := map[string]any{
		"event_id": "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "run_id": fixture.Run["run_id"],
		"owner_id": authority.ownerID, "account_generation": float64(authority.accountGeneration + 1),
		"revision": float64(9), "sequence": float64(1), "type": "execution_succeeded",
		"at": fixture.Run["updated_at"], "payload_digest": fixture.RunEvent["payload_digest"], "status": "succeeded",
	}
	tests := []struct {
		name    string
		action  string
		request map[string]any
		output  map[string]any
	}{
		{"plan owner", "agent.execution.v2.plans.get", cloudWorkerRequest("plan_id", fixture.Plan["plan_id"]), map[string]any{"plan": foreignPlan}},
		{"plan page owner", "agent.execution.v2.plans.list", cloudWorkerRequest("", nil), map[string]any{"plans": []any{fixture.Plan, foreignPlan}, "next_page_token": ""}},
		{"run generation", "agent.execution.v2.runs.get", cloudWorkerRequest("run_id", fixture.Run["run_id"]), map[string]any{"run": foreignRun}},
		{"run page generation", "agent.execution.v2.runs.list", cloudWorkerRequest("", nil), map[string]any{"runs": []any{fixture.Run, foreignRun}, "next_page_token": ""}},
		{"event generation", "agent.execution.v2.runs.events", cloudWorkerRequest("run_id", fixture.Run["run_id"]), map[string]any{"events": []any{foreignEvent}, "next_sequence": float64(1), "history_truncated": false}},
		{"artifact owner", "agent.execution.v2.artifacts.get", cloudWorkerRequest("artifact_id", fixture.Artifacts[0]["artifact_id"]), map[string]any{"artifact": foreignArtifact}},
		{"artifact generation", "agent.execution.v2.artifacts.get", cloudWorkerRequest("artifact_id", fixture.Artifacts[0]["artifact_id"]), map[string]any{"artifact": foreignGenerationArtifact}},
		{"plan request identity", "agent.execution.v2.plans.get", cloudWorkerRequest("plan_id", "99999999-9999-4999-8999-999999999999"), map[string]any{"plan": fixture.Plan}},
		{"plan request revision", "agent.execution.v2.plans.get", map[string]any{"record_kind": "cloud_worker", "plan_id": fixture.Plan["plan_id"], "revision": float64(2)}, map[string]any{"plan": fixture.Plan}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := adaptActionResultForRequestWithAuthority(test.action, test.request, test.output, authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("foreign authority err=%v", err)
			}
		})
	}

	if _, err := adaptActionResultForRequestWithAuthority("agent.execution.v2.plans.get", cloudWorkerRequest("plan_id", fixture.Plan["plan_id"]), map[string]any{"plan": fixture.Plan}, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("missing prepared authority err=%v", err)
	}
}

func cloudWorkerRequest(idField string, value any) map[string]any {
	request := map[string]any{"record_kind": "cloud_worker"}
	if idField != "" {
		request[idField] = value
	}
	return request
}

func cloudWorkerFixtureAuthority(t *testing.T, fixture cloudWorkerPublicFixture) actionResultAuthority {
	t.Helper()
	owner, ok := fixture.Plan["owner_id"].(string)
	generation, generationOK := cloudInteger(fixture.Plan["account_generation"])
	if !ok || !generationOK || owner == "" || generation <= 0 {
		t.Fatal("fixture authority is invalid")
	}
	return actionResultAuthority{ownerID: owner, accountGeneration: generation}
}
