package agent

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"testing"
)

type runtimeProfileClientStub struct {
	state       RuntimeProfileState
	getErr      error
	updateErr   error
	getCalls    int
	updateCalls int
	request     RuntimeProfileUpdate
}

func (stub *runtimeProfileClientStub) GetRuntimeProfile(context.Context) (RuntimeProfileState, error) {
	stub.getCalls++
	return stub.state, stub.getErr
}

func (stub *runtimeProfileClientStub) UpdateRuntimeProfile(_ context.Context, request RuntimeProfileUpdate) (RuntimeProfileState, error) {
	stub.updateCalls++
	stub.request = request
	return stub.state, stub.updateErr
}

func TestRuntimeProfileHandlersExposeOnlyDesecretedOwnerIntent(t *testing.T) {
	client := &runtimeProfileClientStub{state: RuntimeProfileState{
		Available:           true,
		Configured:          true,
		Revision:            7,
		AvailableProfileIDs: []string{"deepseek-v4", "openai-default"},
		Profile: &RuntimeProfile{
			ProfileID: "deepseek-v4", Provider: "deepseek", Model: "deepseekv4-pro",
			BaseURL: "https://api.deepseek.example/v1", MaxOutputTokens: 4096, ContextWindow: 65536,
		},
	}}
	module := New(Config{RuntimeProfiles: client})

	got, actionErr := module.Handlers()[actionRuntimeProfileGet](context.Background(), map[string]any{})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	want := map[string]any{
		"available": true, "configured": true, "revision": int64(7),
		"available_profile_ids": []string{"deepseek-v4", "openai-default"},
		"profile": map[string]any{
			"profile_id": "deepseek-v4", "provider": "deepseek", "model": "deepseekv4-pro",
			"base_url": "https://api.deepseek.example/v1", "temperature": nil, "top_p": nil,
			"max_output_tokens": int64(4096), "context_window": int64(65536), "reasoning_effort": "",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime profile response = %#v, want %#v", got, want)
	}

	got, actionErr = module.Handlers()[actionRuntimeProfileUpdate](context.Background(), map[string]any{
		"idempotency_key":   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"profile_id":        "openai-default",
		"expected_revision": int64(7),
	})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	if client.updateCalls != 1 || client.request.IdempotencyKey != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" ||
		client.request.ProfileID != "openai-default" || client.request.ExpectedRevision != 7 {
		t.Fatalf("runtime profile update = %#v calls=%d", client.request, client.updateCalls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("updated runtime profile response = %#v", got)
	}
}

func TestRuntimeProfileUpdateRejectsCredentialAndUnknownFieldsBeforeClient(t *testing.T) {
	valid := map[string]any{
		"idempotency_key":   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"profile_id":        "deepseek-v4",
		"expected_revision": int64(0),
	}
	for name, mutate := range map[string]func(map[string]any){
		"api key":             func(value map[string]any) { value["api_key"] = "must-not-cross" },
		"secret ref":          func(value map[string]any) { value["secret_ref"] = "mounted:model" },
		"owner":               func(value map[string]any) { value["owner_id"] = "attacker" },
		"provider":            func(value map[string]any) { value["provider"] = "deepseek" },
		"model":               func(value map[string]any) { value["model"] = "caller-model" },
		"base url":            func(value map[string]any) { value["base_url"] = "https://attacker.example" },
		"nested profile":      func(value map[string]any) { value["model_profile"] = map[string]any{"api_key": "must-not-cross"} },
		"unknown":             func(value map[string]any) { value["extra"] = true },
		"noncanonical UUID":   func(value map[string]any) { value["idempotency_key"] = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" },
		"fractional revision": func(value map[string]any) { value["expected_revision"] = 1.5 },
		"negative revision":   func(value map[string]any) { value["expected_revision"] = int64(-1) },
		"maximum revision":    func(value map[string]any) { value["expected_revision"] = int64(math.MaxInt64) },
		"invalid profile":     func(value map[string]any) { value["profile_id"] = "../secret" },
		"secret-shaped profile": func(value map[string]any) {
			value["profile_id"] = "sk-aaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"zero output limit": func(value map[string]any) { value["max_output_tokens"] = int64(0) },
	} {
		t.Run(name, func(t *testing.T) {
			client := &runtimeProfileClientStub{}
			params := make(map[string]any, len(valid)+1)
			for key, value := range valid {
				params[key] = value
			}
			mutate(params)
			_, actionErr := New(Config{RuntimeProfiles: client}).Handlers()[actionRuntimeProfileUpdate](context.Background(), params)
			if actionErr == nil || actionErr.Status != http.StatusBadRequest || client.updateCalls != 0 {
				t.Fatalf("error=%#v update_calls=%d", actionErr, client.updateCalls)
			}
		})
	}
}

func TestRuntimeProfileHandlersRejectInvalidClientProjection(t *testing.T) {
	for name, mutate := range map[string]func(*RuntimeProfileState){
		"secret-shaped ID": func(state *RuntimeProfileState) {
			state.AvailableProfileIDs = []string{"sk-aaaaaaaaaaaaaaaaaaaaaaaa"}
			state.Profile.ProfileID = "sk-aaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"secret-shaped model": func(state *RuntimeProfileState) {
			state.Profile.Model = "api_key=must-not-cross"
		},
		"non-HTTPS URL": func(state *RuntimeProfileState) {
			state.Profile.BaseURL = "http://model.example/v1"
		},
		"foreign profile": func(state *RuntimeProfileState) {
			state.Profile.ProfileID = "openai-default"
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := RuntimeProfileState{
				Available: true, Configured: true, Revision: 7,
				AvailableProfileIDs: []string{"deepseek-v4"},
				Profile: &RuntimeProfile{
					ProfileID: "deepseek-v4", Provider: "deepseek", Model: "deepseekv4-pro",
					BaseURL: "https://api.deepseek.example/v1", MaxOutputTokens: 4096, ContextWindow: 65536,
				},
			}
			mutate(&state)
			client := &runtimeProfileClientStub{state: state}
			_, actionErr := New(Config{RuntimeProfiles: client}).Handlers()[actionRuntimeProfileGet](context.Background(), map[string]any{})
			if actionErr == nil || actionErr.Status != http.StatusBadGateway || actionErr.Error != "Agent runtime profile response is invalid" {
				t.Fatalf("invalid projection error = %#v", actionErr)
			}
		})
	}
}

func TestRuntimeProfileHandlersFailClosedWithoutRemoteClient(t *testing.T) {
	module := New(Config{})
	got, actionErr := module.Handlers()[actionRuntimeProfileGet](context.Background(), nil)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	want := map[string]any{
		"available": false, "configured": false, "revision": int64(0),
		"available_profile_ids": []string{}, "profile": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local runtime profile response = %#v", got)
	}
	_, actionErr = module.Handlers()[actionRuntimeProfileUpdate](context.Background(), map[string]any{
		"idempotency_key": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "profile_id": "deepseek-v4", "expected_revision": int64(0),
	})
	if actionErr == nil || actionErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unavailable update error = %#v", actionErr)
	}
}
