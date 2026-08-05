package agent

import (
	"context"
	"testing"
)

type searchProfileClientStub struct {
	state  SearchProfileState
	update SearchProfileUpdate
}

func (stub *searchProfileClientStub) GetSearchProfile(context.Context) (SearchProfileState, error) {
	return stub.state, nil
}

func (stub *searchProfileClientStub) UpdateSearchProfile(_ context.Context, update SearchProfileUpdate) (SearchProfileState, error) {
	stub.update = update
	return stub.state, nil
}

func TestSearchProfileActionsExposeOnlyCatalogBoundPublicState(t *testing.T) {
	client := &searchProfileClientStub{state: SearchProfileState{
		Available: true, Configured: true, Revision: 4,
		AvailableProfileIDs: []string{"brave-default", "tavily-default"},
		Profile: &SearchProfile{
			ProfileID: "brave-default", Provider: "brave",
			BaseURL:    "https://api.search.brave.com/res/v1/web/search",
			MaxResults: 10, TimeoutSeconds: 15,
		},
	}}
	handlers := New(Config{SearchProfiles: client}).Handlers()
	value, actionErr := handlers[actionSearchProfileGet](context.Background(), map[string]any{})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	state := value.(map[string]any)
	profile := state["profile"].(map[string]any)
	if state["configured"] != true || profile["profile_id"] != "brave-default" || profile["max_results"] != int64(10) {
		t.Fatalf("search profile response = %#v", state)
	}
	if _, exposed := profile["secret_ref"]; exposed {
		t.Fatal("search credential reference crossed ProductCore")
	}

	value, actionErr = handlers[actionSearchProfileUpdate](context.Background(), map[string]any{
		"idempotency_key": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"profile_id":      "brave-default", "expected_revision": 4,
		"max_results": 6, "timeout_seconds": 12,
	})
	if actionErr != nil || value == nil {
		t.Fatalf("search profile update = %#v, %v", value, actionErr)
	}
	if client.update.ProfileID != "brave-default" || client.update.MaxResults == nil || *client.update.MaxResults != 6 ||
		client.update.TimeoutSeconds == nil || *client.update.TimeoutSeconds != 12 {
		t.Fatalf("search profile update request = %#v", client.update)
	}
}

func TestSearchProfileActionsFailClosedForCredentialOrUnknownFields(t *testing.T) {
	client := &searchProfileClientStub{}
	handler := New(Config{SearchProfiles: client}).Handlers()[actionSearchProfileUpdate]
	for name, params := range map[string]map[string]any{
		"credential": {
			"idempotency_key": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"profile_id":      "brave-default", "expected_revision": 0,
			"api_key": "sk-abcdefghijklmnopqrstuvwxyz",
		},
		"excess limit": {
			"idempotency_key": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"profile_id":      "brave-default", "expected_revision": 0,
			"max_results": 51,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if value, actionErr := handler(context.Background(), params); actionErr == nil || value != nil {
				t.Fatalf("unsafe update succeeded: value=%#v error=%#v", value, actionErr)
			}
		})
	}

	value, actionErr := New(Config{}).Handlers()[actionSearchProfileGet](context.Background(), map[string]any{})
	if actionErr != nil || value.(map[string]any)["available"] != false {
		t.Fatalf("unavailable state = %#v, %v", value, actionErr)
	}
}

func TestSearchProfileStateAcceptsServerOwnedDeepSeekNativeSearch(t *testing.T) {
	state := SearchProfileState{
		Available: true, Configured: true, Revision: 2,
		AvailableProfileIDs: []string{"deepseek-native-default"},
		Profile: &SearchProfile{
			ProfileID: "deepseek-native-default", Provider: "deepseek_native",
			BaseURL:    "https://api.deepseek.com/anthropic/v1/messages",
			MaxResults: 8, TimeoutSeconds: 45,
		},
	}
	if err := validateSearchProfileState(state); err != nil {
		t.Fatalf("DeepSeek native search state was rejected: %v", err)
	}
}
