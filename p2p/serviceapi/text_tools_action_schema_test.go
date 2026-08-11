package serviceapi

import "testing"

func TestTextToolsActionSurfaceIsOwnerHTTPAndWSTyped(t *testing.T) {
	for _, action := range []string{"agent.text_tools.config.get", "agent.text_tools.config.update", "agent.text_tools.execute"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPAndWS || spec.Schema == nil {
			t.Fatalf("%s spec = %#v", action, spec)
		}
	}
	get, _ := ActionSpecFor("agent.text_tools.config.get")
	if len(get.Schema.Request) != 0 {
		t.Fatalf("config get request must be empty: %#v", get.Schema.Request)
	}
	update, _ := ActionSpecFor("agent.text_tools.config.update")
	for _, field := range []string{"idempotency_key", "expected_revision", "enabled", "tools"} {
		if !update.Schema.Request[field].Required {
			t.Errorf("config update %s is not required", field)
		}
	}
	tool := update.Schema.Request["tools"].Items
	if tool == nil {
		t.Fatal("config tools item schema is missing")
	}
	for _, field := range []string{"tool_id", "name", "system_prompt", "order", "enabled"} {
		if !tool.Properties[field].Required {
			t.Errorf("config tool %s is not required", field)
		}
	}
	execute, _ := ActionSpecFor("agent.text_tools.execute")
	if len(execute.Schema.Request) != 3 || !execute.Schema.Request["tool_id"].Required || !execute.Schema.Request["selected_text"].Required || !execute.Schema.Request["output_language"].Required {
		t.Fatalf("execute request is not closed and typed: %#v", execute.Schema.Request)
	}
	if got := execute.Schema.Request["output_language"].Presence; got == nil || got.Present != "one_of:zh|en" || got.Empty != "rejected" {
		t.Fatalf("execute output_language contract = %#v", got)
	}
	for _, field := range []string{"tool_id", "output", "sources"} {
		if !execute.Schema.Response[field].Required {
			t.Errorf("execute response %s is not required", field)
		}
	}
}
