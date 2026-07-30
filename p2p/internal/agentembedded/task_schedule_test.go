package agentembedded

import (
	"encoding/json"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

func TestTaskMapUsesArraysForMissingReferences(t *testing.T) {
	projected := taskMap(task.Task{})
	attachments, ok := projected["attachment_refs"].([]string)
	if !ok || attachments == nil || len(attachments) != 0 {
		t.Fatalf("attachment_refs = %#v, want non-nil empty []string", projected["attachment_refs"])
	}
	knowledge, ok := projected["knowledge_refs"].([]string)
	if !ok || knowledge == nil || len(knowledge) != 0 {
		t.Fatalf("knowledge_refs = %#v, want non-nil empty []string", projected["knowledge_refs"])
	}
}

func TestTaskMapSerializesMissingReferencesAsJSONArrays(t *testing.T) {
	raw, err := json.Marshal(taskMap(task.Task{}))
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"attachment_refs", "knowledge_refs"} {
		if got := string(object[field]); got != "[]" {
			t.Fatalf("%s JSON = %s, want []", field, got)
		}
	}
}

func TestTaskMapPreservesReferenceValues(t *testing.T) {
	projected := taskMap(task.Task{Spec: task.TaskSpec{
		AttachmentRefs: []string{"mxc://attachment"},
		KnowledgeRefs:  []string{"knowledge-1"},
	}})
	if got := projected["attachment_refs"].([]string); len(got) != 1 || got[0] != "mxc://attachment" {
		t.Fatalf("attachment_refs = %#v", got)
	}
	if got := projected["knowledge_refs"].([]string); len(got) != 1 || got[0] != "knowledge-1" {
		t.Fatalf("knowledge_refs = %#v", got)
	}
}
