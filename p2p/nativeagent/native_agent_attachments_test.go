package nativeagent

import (
	"encoding/base64"
	"strings"
	"testing"
)

func attachmentParams(encoded string) map[string]any {
	return map[string]any{"prompt": "describe", "attachments": []any{map[string]any{"type": "image", "mime_type": "image/png", "data_base64": encoded}}}
}

func TestNativeAgentAttachmentValidation(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("image"))
	if got, err := ValidateNativeAgentChatParams(attachmentParams(encoded)); err != nil || len(got) != 1 {
		t.Fatalf("valid attachment: %#v %v", got, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"whitespace": func(p map[string]any) { p["attachments"].([]any)[0].(map[string]any)["data_base64"] = " " + encoded },
		"wrong type": func(p map[string]any) { p["attachments"].([]any)[0].(map[string]any)["type"] = "file" },
		"wrong mime": func(p map[string]any) { p["attachments"].([]any)[0].(map[string]any)["mime_type"] = "image/gif" },
	} {
		params := attachmentParams(encoded)
		mutate(params)
		if _, err := ValidateNativeAgentChatParams(params); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ValidateNativeAgentChatParams(map[string]any{"prompt": ""}); err == nil {
		t.Fatal("empty prompt without image accepted")
	}
	if _, err := ValidateNativeAgentChatParams(map[string]any{"prompt": "", "attachments": []any{map[string]any{"mime_type": "image/png", "data_base64": encoded}}}); err != nil {
		t.Fatalf("omitted image type: %v", err)
	}
	tooMany := attachmentParams(encoded)
	tooMany["attachments"] = []any{map[string]any{"mime_type": "image/png", "data_base64": encoded}, map[string]any{"mime_type": "image/png", "data_base64": encoded}, map[string]any{"mime_type": "image/png", "data_base64": encoded}, map[string]any{"mime_type": "image/png", "data_base64": encoded}, map[string]any{"mime_type": "image/png", "data_base64": encoded}}
	if _, err := ValidateNativeAgentChatParams(tooMany); err == nil {
		t.Fatal("attachment count limit not enforced")
	}
	oversize := attachmentParams(base64.StdEncoding.EncodeToString(make([]byte, maxNativeAgentImageBytes+1)))
	if _, err := ValidateNativeAgentChatParams(oversize); err == nil {
		t.Fatal("per-image size limit not enforced")
	}
}

func TestNativeAgentMultimodalProviderPayloadsAndMemoryRedaction(t *testing.T) {
	params := attachmentParams(base64.StdEncoding.EncodeToString([]byte("image")))
	messages := requestEinoMessages(params)
	if len(messages) != 1 || len(messages[0].UserInputMultiContent) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	openai := openAICompatibleMessages(messages)[0]["content"].([]map[string]any)
	if !strings.HasPrefix(openai[1]["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Fatalf("openai payload = %#v", openai)
	}
	_, gemini := geminiDirectContents(messages)
	if _, ok := gemini[0]["parts"].([]map[string]any)[1]["inlineData"]; !ok {
		t.Fatalf("gemini payload = %#v", gemini)
	}
	_, anthropic := anthropicDirectMessages(messages)
	if _, ok := anthropic[0]["content"].([]map[string]any)[1]["source"]; !ok {
		t.Fatalf("anthropic payload = %#v", anthropic)
	}
	stored := trimEinoMessageForMemory(messages[0])
	if strings.Contains(stored.Content, "aW1hZ2U") || !strings.Contains(stored.Content, "describe") || len(stored.UserInputMultiContent) != 0 {
		t.Fatalf("memory leaked multimodal content: %#v", stored)
	}
	if clone := cloneEinoMessages(messages); &clone[0].UserInputMultiContent[0] == &messages[0].UserInputMultiContent[0] {
		t.Fatal("multimodal slice aliased")
	}
}
