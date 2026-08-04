package productcapability

import (
	"context"
	"encoding/json"
	"fmt"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// RegisterBuiltinCapabilities 注册所有内置的 Product capabilities
func RegisterBuiltinCapabilities(registry *Registry) error {
	// 注册 echo capability（用于测试）
	if err := registerEchoCapability(registry); err != nil {
		return fmt.Errorf("failed to register echo capability: %w", err)
	}

	// TODO: 注册 contacts capability
	// TODO: 注册 rooms capability
	// TODO: 注册 messages capability
	// TODO: 注册 members capability
	// TODO: 注册 channels capability

	return nil
}

// registerEchoCapability 注册一个简单的 echo capability 用于测试
func registerEchoCapability(registry *Registry) error {
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:      "product.echo.v1",
		SemanticVersion:   "1.0.0",
		ProtocolVersion:   1,
		CatalogDigest:     []byte("echo-v1"),
		DisplayName:       "Echo",
		Description:       "Simple echo capability for testing",
		Readiness:         true,
		ReadinessReason:   "",
		Operations: []*capv1.OperationDescriptor{
			{
				OperationId:          "echo",
				DisplayName:          "Echo",
				Description:          "Returns the input message",
				OperationType:        capv1.OperationType_OPERATION_TYPE_READ,
				Audience:             []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT},
				RiskLevel:            capv1.RiskLevel_RISK_LEVEL_SAFE,
				RequiredScopes:       []string{},
				RequiredGrants:       []string{},
				RequiresRevision:     false,
				MaxRequestSizeBytes:  1024 * 1024, // 1 MiB
				TimeoutClass:         "short",
				InputSchemaJson:      `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`,
				InputSchemaDigest:    []byte("echo-input-v1"),
				ResultSchemaJson:     `{"type":"object","properties":{"echo":{"type":"string"}},"required":["echo"]}`,
				ResultSchemaDigest:   []byte("echo-result-v1"),
			},
		},
	}

	handler := func(ctx context.Context, input []byte) ([]byte, error) {
		// 解析输入
		var req struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}

		// 构造响应
		resp := map[string]interface{}{
			"echo": req.Message,
		}

		return json.Marshal(resp)
	}

	return registry.Register(&Provider{
		Descriptor: descriptor,
		Handler:    handler,
	})
}
