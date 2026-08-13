package agentgateway

import (
	"errors"
	"testing"
)

const (
	workerIdentitySchema = `{"additionalProperties":false,"properties":{"account_id":{"type":"string"},"credential_id":{"type":"string"},"credential_revision":{"minimum":1,"type":"integer"},"instance_id":{"type":"string"},"key_pair_id":{"type":"string"},"region":{"type":"string"},"security_group_id":{"type":"string"},"worker_id":{"type":"string"}},"required":["worker_id","instance_id","key_pair_id","security_group_id","credential_id","credential_revision","account_id","region"],"type":"object"}`
	workerDomainSchema   = `{"additionalProperties":false,"properties":{"hostname":{"type":"string"},"mode":{"const":"route53_same_account","type":"string"},"record_status":{"enum":["pending","current","drifted","error"],"type":"string"},"target_ipv4":{"type":"string"},"ttl":{"type":"integer"},"zone_id":{"type":"string"}},"required":["mode","zone_id","hostname","target_ipv4","ttl","record_status"],"type":"object"}`
	workerWorkloadSchema = `{"additionalProperties":false,"properties":{"active_state":{"type":"string"},"domain":` + workerDomainSchema + `,"health":{"type":"string"},"kind":{"enum":["job","service"],"type":"string"},"phase":{"type":"string"},"port":{"type":"integer"},"workload_id":{"type":"string"}},"required":["workload_id","kind","phase","active_state","health"],"type":"object"}`
	workerStatusSchema   = `{"additionalProperties":false,"properties":{"availability":{"enum":["available","unavailable"],"type":"string"},"current_task":{"additionalProperties":false,"properties":{"execution_id":{"type":"string"},"phase":{"type":"string"}},"required":["execution_id","phase"],"type":"object"},"ec2_state":{"type":"string"},"error":{"type":"string"},"hourly_quote":{"additionalProperties":false,"properties":{"currency":{"type":"string"},"expires_at":{"format":"date-time","type":"string"},"micros_per_hour":{"minimum":0,"type":"integer"},"observed_at":{"format":"date-time","type":"string"}},"required":["currency","micros_per_hour","observed_at","expires_at"],"type":"object"},"identity":` + workerIdentitySchema + `,"observed_at":{"format":"date-time","type":"string"},"public_ipv4":{"type":"string"},"server":{"additionalProperties":false,"properties":{"last_seen":{"format":"date-time","type":"string"},"load_1":{"type":"number"},"load_15":{"type":"number"},"load_5":{"type":"number"}},"required":["last_seen","load_1","load_5","load_15"],"type":"object"},"worker_phase":{"type":"string"},"workloads":{"items":` + workerWorkloadSchema + `,"type":"array"}},"required":["identity","availability","ec2_state","worker_phase","observed_at"],"type":"object"}`
)

func workerIdentityFixture() map[string]any {
	return map[string]any{
		"worker_id": "worker-1", "instance_id": "i-123", "key_pair_id": "kp-123", "security_group_id": "sg-123",
		"credential_id": "credential-1", "credential_revision": int64(1), "account_id": "123456789012", "region": "ap-east-1",
	}
}

func TestWorkerActionBindingsAndSchemaPins(t *testing.T) {
	listInput := `{"additionalProperties":false,"properties":{},"type":"object"}`
	for _, test := range []struct{ action, operation, input, result string }{
		{"agent.workers.list", "list_workers", listInput, `{"additionalProperties":false,"properties":{"workers":{"items":` + workerStatusSchema + `,"maxItems":5,"type":"array"}},"required":["workers"],"type":"object"}`},
		{"agent.workers.get", "get_worker", `{"additionalProperties":false,"properties":{"identity":` + workerIdentitySchema + `},"required":["identity"],"type":"object"}`, `{"additionalProperties":false,"properties":{"worker":` + workerStatusSchema + `},"required":["worker"],"type":"object"}`},
		{"agent.workers.destroy", "destroy_worker", `{"additionalProperties":false,"properties":{"confirmation":{"const":"destroy_worker","type":"string"},"identity":` + workerIdentitySchema + `},"required":["identity","confirmation"],"type":"object"}`, `{"additionalProperties":false,"properties":{"destroyed":{"const":true,"type":"boolean"},"identity":` + workerIdentitySchema + `},"required":["identity","destroyed"],"type":"object"}`},
		{"agent.workers.bind_domain", "bind_domain", `{"additionalProperties":false,"properties":{"confirmation":{"const":"bind_domain","type":"string"},"hostname":{"type":"string"},"ttl":{"type":"integer"},"worker_identity":` + workerIdentitySchema + `,"workload_id":{"type":"string"},"zone_id":{"type":"string"}},"required":["worker_identity","workload_id","zone_id","hostname","ttl","confirmation"],"type":"object"}`, `{"additionalProperties":false,"properties":{"domain":` + workerDomainSchema + `,"worker_identity":` + workerIdentitySchema + `,"workload_id":{"type":"string"}},"required":["worker_identity","workload_id","domain"],"type":"object"}`},
		{"agent.workers.unbind_domain", "unbind_domain", `{"additionalProperties":false,"properties":{"confirmation":{"const":"unbind_domain","type":"string"},"hostname":{"type":"string"},"ttl":{"type":"integer"},"worker_identity":` + workerIdentitySchema + `,"workload_id":{"type":"string"},"zone_id":{"type":"string"}},"required":["worker_identity","workload_id","zone_id","hostname","ttl","confirmation"],"type":"object"}`, `{"additionalProperties":false,"properties":{"domain":` + workerDomainSchema + `,"unbound":{"const":true,"type":"boolean"},"worker_identity":` + workerIdentitySchema + `,"workload_id":{"type":"string"}},"required":["worker_identity","workload_id","domain","unbound"],"type":"object"}`},
	} {
		binding, ok := actionBindingFor(test.action)
		if !ok || binding.capabilityID != "agent.worker.v1" || binding.operation != test.operation {
			t.Fatalf("%s binding = %#v", test.action, binding)
		}
		descriptor := catalogTestDescriptor("agent.worker.v1", test.operation, test.input, test.result)
		if err := ValidateCatalog(catalogTestWithDigest(t, descriptor), []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s schema rejected: %v", test.action, err)
		}
	}
}

func TestWorkerRequestsRequireExactIdentityAndConfirmation(t *testing.T) {
	identity := workerIdentityFixture()
	valid := []struct {
		action string
		params map[string]any
	}{
		{"agent.workers.list", map[string]any{}},
		{"agent.workers.get", map[string]any{"identity": identity}},
		{"agent.workers.destroy", map[string]any{"identity": identity, "confirmation": "destroy_worker"}},
		{"agent.workers.bind_domain", map[string]any{"worker_identity": identity, "workload_id": "web", "zone_id": "Z123", "hostname": "app.example.com", "ttl": int64(300), "confirmation": "bind_domain"}},
		{"agent.workers.unbind_domain", map[string]any{"worker_identity": identity, "workload_id": "web", "zone_id": "Z123", "hostname": "app.example.com", "ttl": int64(300), "confirmation": "unbind_domain"}},
	}
	for _, test := range valid {
		if err := ValidateActionRequest(test.action, test.params); err != nil {
			t.Errorf("%s valid request rejected: %v", test.action, err)
		}
	}

	invalid := workerIdentityFixture()
	delete(invalid, "instance_id")
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.workers.get", map[string]any{"identity": invalid}},
		{"agent.workers.destroy", map[string]any{"identity": workerIdentityFixture(), "confirmation": "yes"}},
		{"agent.workers.bind_domain", map[string]any{"worker_identity": workerIdentityFixture(), "workload_id": "web", "zone_id": "Z123", "hostname": "app.example.com", "ttl": int64(300), "confirmation": "unbind_domain"}},
	} {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("%s invalid request error = %v", test.action, err)
		}
	}
}

func TestWorkerResultPassesThroughWithoutDuplicateDeepValidation(t *testing.T) {
	output := map[string]any{"workers": []any{}, "future_field": "preserved"}
	result, err := adaptActionResultForRequest("agent.workers.list", map[string]any{}, output)
	if err != nil || result["future_field"] != "preserved" {
		t.Fatalf("thin Worker projection = %#v, err=%v", result, err)
	}
}
