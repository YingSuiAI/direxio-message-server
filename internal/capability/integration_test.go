package capability

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/productcapability"
	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// TestEchoCapability 测试 echo capability 的基本功能
func TestEchoCapability(t *testing.T) {
	// 创建 registry 并注册内置 capabilities
	registry := productcapability.NewRegistry()
	if err := productcapability.RegisterBuiltinCapabilities(registry); err != nil {
		t.Fatalf("Failed to register builtin capabilities: %v", err)
	}

	// 测试 DescribeCapabilities
	descriptors := registry.List()
	if len(descriptors) == 0 {
		t.Fatal("No capabilities registered")
	}

	foundEcho := false
	for _, desc := range descriptors {
		if desc.CapabilityId == "product.echo.v1" {
			foundEcho = true
			if !desc.Readiness {
				t.Error("Echo capability not ready")
			}
			if len(desc.Operations) != 1 {
				t.Errorf("Expected 1 operation, got %d", len(desc.Operations))
			}
		}
	}

	if !foundEcho {
		t.Fatal("Echo capability not found")
	}

	// 测试 Query echo capability
	provider, ok := registry.Get("product.echo.v1")
	if !ok {
		t.Fatal("Failed to get echo capability")
	}

	input := map[string]interface{}{
		"message": "hello world",
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Failed to marshal input: %v", err)
	}

	ctx := context.Background()
	resultJSON, err := provider.Handler(ctx, inputJSON)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if result["echo"] != "hello world" {
		t.Errorf("Expected echo='hello world', got '%v'", result["echo"])
	}
}

// TestRequestDigest 测试 request digest 计算
func TestRequestDigest(t *testing.T) {
	input := map[string]interface{}{
		"message": "test",
	}

	digest1, err := capv1.ComputeRequestDigest(
		1,
		"product.echo.v1",
		"1.0.0",
		[]byte("echo-v1"),
		"echo",
		0,
		input,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Failed to compute digest: %v", err)
	}

	// 计算第二次，验证确定性
	digest2, err := capv1.ComputeRequestDigest(
		1,
		"product.echo.v1",
		"1.0.0",
		[]byte("echo-v1"),
		"echo",
		0,
		input,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Failed to compute digest second time: %v", err)
	}

	// 验证两次计算结果相同
	hash1 := sha256.Sum256(digest1)
	hash2 := sha256.Sum256(digest2)
	if hash1 != hash2 {
		t.Error("Digest not deterministic")
	}

	t.Logf("Request digest: %x", digest1)
}

// TestCallContextValidation 测试 CallContext 验证
func TestCallContextValidation(t *testing.T) {
	now := time.Now().UnixMilli()
	deadline := now + 30000

	tests := []struct {
		name    string
		ctx     *capv1.CallContext
		wantErr bool
	}{
		{
			name: "valid context from flutter",
			ctx: &capv1.CallContext{
				ChainId:         "chain-123",
				RootOperationId: "op-456",
				Hop:             0,
				Route:           "",
				DeadlineUnixMs:  deadline,
			},
			wantErr: false,
		},
		{
			name: "valid context agent->ms",
			ctx: &capv1.CallContext{
				ChainId:         "chain-123",
				RootOperationId: "op-456",
				Hop:             1,
				Route:           "agent",
				DeadlineUnixMs:  deadline,
			},
			wantErr: false,
		},
		{
			name: "exceeds max hop",
			ctx: &capv1.CallContext{
				ChainId:         "chain-123",
				RootOperationId: "op-456",
				Hop:             3,
				Route:           "agent→ms→agent",
				DeadlineUnixMs:  deadline,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := capv1.ValidateCallContext(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCallContext() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestCycleDetection 测试循环调用检测
func TestCycleDetection(t *testing.T) {
	tests := []struct {
		name    string
		route   string
		wantErr bool
	}{
		{
			name:    "no cycle",
			route:   "flutter→agent→ms",
			wantErr: false,
		},
		{
			name:    "back and forth allowed",
			route:   "agent→ms→agent",
			wantErr: false,
		},
		{
			name:    "cycle detected",
			route:   "agent→ms→agent→ms→agent",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := capv1.DetectCycle(tt.route)
			if (err != nil) != tt.wantErr {
				t.Errorf("DetectCycle() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
