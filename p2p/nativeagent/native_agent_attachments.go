package nativeagent

import (
	"encoding/base64"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const (
	maxNativeAgentAttachments = 4
	maxNativeAgentImageBytes  = 4 << 20
	maxNativeAgentImageTotal  = 8 << 20
)

var nativeAgentImageMIMETypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

type nativeAgentAttachment struct {
	Name     string
	MIMEType string
	Base64   string
}

func parseNativeAgentAttachments(params map[string]any) ([]nativeAgentAttachment, error) {
	raw, present := params["attachments"]
	if !present || raw == nil {
		return nil, nil
	}
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []map[string]any:
		values = make([]any, len(typed))
		for i := range typed {
			values[i] = typed[i]
		}
	default:
		return nil, validationErrorf("attachments must be an array")
	}
	if len(values) > maxNativeAgentAttachments {
		return nil, validationErrorf("attachments must contain at most %d images", maxNativeAgentAttachments)
	}
	attachments := make([]nativeAgentAttachment, 0, len(values))
	total := 0
	for i, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, validationErrorf("attachments[%d] must be an object", i)
		}
		mimeType, mimeOK := item["mime_type"].(string)
		if !mimeOK {
			mimeType = ""
		}
		if _, ok := nativeAgentImageMIMETypes[mimeType]; !ok {
			return nil, validationErrorf("attachments[%d].mime_type must be image/jpeg, image/png, or image/webp", i)
		}
		typeName, typePresent := item["type"]
		if typePresent {
			if trimString(typeName) != "image" {
				return nil, validationErrorf("attachments[%d].type must be image", i)
			}
		} else if !strings.HasPrefix(mimeType, "image/") {
			return nil, validationErrorf("attachments[%d].type is required", i)
		}
		name, nameOK := item["name"].(string)
		if !nameOK {
			name = ""
		}
		if len(name) > 255 {
			return nil, validationErrorf("attachments[%d].name is too long", i)
		}
		encoded, encodedOK := item["data_base64"].(string)
		if !encodedOK {
			encoded = ""
		}
		if encoded == "" {
			return nil, validationErrorf("attachments[%d].data_base64 is required", i)
		}
		if len(encoded) > base64.StdEncoding.EncodedLen(maxNativeAgentImageBytes) {
			return nil, validationErrorf("attachments[%d] exceeds 4 MiB", i)
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
			return nil, validationErrorf("attachments[%d].data_base64 must be canonical standard base64", i)
		}
		if len(decoded) > maxNativeAgentImageBytes {
			return nil, validationErrorf("attachments[%d] exceeds 4 MiB", i)
		}
		total += len(decoded)
		if total > maxNativeAgentImageTotal {
			return nil, validationErrorf("attachments exceed 8 MiB total")
		}
		attachments = append(attachments, nativeAgentAttachment{Name: name, MIMEType: mimeType, Base64: encoded})
	}
	return attachments, nil
}

// ValidateNativeAgentChatParams validates the frozen V1 chat request shape.
// It is intentionally exported so the durable action boundary can validate
// before calculating or reserving a request digest.
func ValidateNativeAgentChatParams(params map[string]any) ([]nativeAgentAttachment, error) {
	attachments, err := parseNativeAgentAttachments(params)
	if err != nil {
		return nil, err
	}
	prompt := fallbackString(trimString(params["prompt"]), trimString(params["message"]))
	if strings.TrimSpace(prompt) == "" && len(attachments) == 0 && !hasExplicitRequestMessages(params) {
		return nil, validationErrorf("prompt or message is required unless an image attachment is provided")
	}
	return attachments, nil
}

func nativeAgentImageProfileAllowed(params map[string]any, profile nativeModelProfile, hasImages bool) error {
	if !hasImages {
		return nil
	}
	serverPinned := boolParam(params["_server_pinned_profile"])
	if _, inline := params["model_profile"]; inline && !serverPinned {
		return validationErrorf("image attachments require a server model profile")
	}
	if !serverPinned && trimString(params["model_profile_id"]) == "" && trimString(params["client_model_profile_id"]) == "" {
		return validationErrorf("image attachments require a server model profile")
	}
	if profile.ModelKind != "conversation" {
		return validationErrorf("image attachments require a conversation model profile")
	}
	for _, modality := range profile.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return nil
		}
	}
	return validationErrorf("server model profile does not support image input")
}

func nativeAgentAttachmentParts(prompt string, attachments []nativeAgentAttachment) []schema.MessageInputPart {
	parts := make([]schema.MessageInputPart, 0, 1+len(attachments))
	if prompt != "" {
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: prompt})
	}
	for _, attachment := range attachments {
		encoded := attachment.Base64
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: attachment.MIMEType}}})
	}
	return parts
}
