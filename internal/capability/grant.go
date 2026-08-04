package capability

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// CapabilityGrant 是 message-server 签发给 Agent 的短期授权
type CapabilityGrant struct {
	// RootOperationID 是触发此授权的根 operation
	RootOperationID string

	// OwnerID 是 owner 的身份标识
	OwnerID string

	// AccountGeneration 是账号生成标识
	AccountGeneration int64

	// GrantedScopes 是授予的权限范围
	GrantedScopes []string

	// CatalogDigest 是 capability catalog 的摘要
	CatalogDigest []byte

	// ExpiryUnixMs 是过期时间（Unix 毫秒）
	ExpiryUnixMs int64

	// MaxHop 是最大允许的调用深度
	MaxHop int32

	// IssuedAtUnixMs 是签发时间
	IssuedAtUnixMs int64
}

// GrantManager 管理 Capability Grant 的签发和验证
type GrantManager struct {
	secret []byte // HMAC 密钥
}

// NewGrantManager 创建新的 GrantManager
func NewGrantManager(secret []byte) *GrantManager {
	return &GrantManager{
		secret: secret,
	}
}

// IssueGrant 签发一个新的 Capability Grant
func (m *GrantManager) IssueGrant(ctx context.Context, grant *CapabilityGrant) ([]byte, error) {
	// 设置签发时间和过期时间（默认 5 分钟）
	now := time.Now().UnixMilli()
	grant.IssuedAtUnixMs = now
	if grant.ExpiryUnixMs == 0 {
		grant.ExpiryUnixMs = now + 5*60*1000 // 5 分钟
	}

	// 默认最大 hop
	if grant.MaxHop == 0 {
		grant.MaxHop = 2
	}

	// 构造 grant 数据
	grantData := fmt.Sprintf("%s|%s|%d|%s|%x|%d|%d|%d",
		grant.RootOperationID,
		grant.OwnerID,
		grant.AccountGeneration,
		joinScopes(grant.GrantedScopes),
		grant.CatalogDigest,
		grant.ExpiryUnixMs,
		grant.MaxHop,
		grant.IssuedAtUnixMs,
	)

	// 计算 HMAC
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(grantData))
	signature := mac.Sum(nil)

	// 组合为最终的 grant bytes
	result := append([]byte(grantData), '|')
	result = append(result, []byte(base64.StdEncoding.EncodeToString(signature))...)

	return result, nil
}

// VerifyGrant 验证一个 Capability Grant
func (m *GrantManager) VerifyGrant(ctx context.Context, grantBytes []byte) (*CapabilityGrant, error) {
	if len(grantBytes) == 0 {
		return nil, fmt.Errorf("empty grant")
	}

	// 分割 grant 数据和签名（最后一个 | 之后是签名）
	parts := splitGrantParts(string(grantBytes))
	if len(parts) != 9 {
		return nil, fmt.Errorf("invalid grant format: expected 9 parts, got %d", len(parts))
	}

	rootOperationID := parts[0]
	ownerID := parts[1]
	accountGeneration := parseInt64(parts[2])
	scopesStr := parts[3]
	catalogDigestHex := parts[4]
	expiryUnixMs := parseInt64(parts[5])
	maxHop := parseInt32(parts[6])
	issuedAtUnixMs := parseInt64(parts[7])
	signatureB64 := parts[8]

	// 重构 grant 数据（不包含签名）
	grantData := fmt.Sprintf("%s|%s|%d|%s|%s|%d|%d|%d",
		rootOperationID,
		ownerID,
		accountGeneration,
		scopesStr,
		catalogDigestHex,
		expiryUnixMs,
		maxHop,
		issuedAtUnixMs,
	)

	// 解码签名
	signature, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return nil, fmt.Errorf("invalid signature encoding: %w", err)
	}

	// 验证 HMAC
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(grantData))
	expectedSignature := mac.Sum(nil)

	if !hmac.Equal(signature, expectedSignature) {
		return nil, fmt.Errorf("signature verification failed")
	}

	// 检查过期时间
	now := time.Now().UnixMilli()
	if now > expiryUnixMs {
		return nil, fmt.Errorf("grant expired at %d, now is %d", expiryUnixMs, now)
	}

	// 解析 catalog digest
	catalogDigest := []byte{}
	if catalogDigestHex != "" {
		fmt.Sscanf(catalogDigestHex, "%x", &catalogDigest)
	}

	return &CapabilityGrant{
		RootOperationID:   rootOperationID,
		OwnerID:           ownerID,
		AccountGeneration: accountGeneration,
		GrantedScopes:     splitScopes(scopesStr),
		CatalogDigest:     catalogDigest,
		ExpiryUnixMs:      expiryUnixMs,
		MaxHop:            maxHop,
		IssuedAtUnixMs:    issuedAtUnixMs,
	}, nil
}

// BuildPermissionContext 从 grant 构造 PermissionContext
func BuildPermissionContext(grant *CapabilityGrant, grantBytes []byte) *capv1.PermissionContext {
	return &capv1.PermissionContext{
		AuthenticatedOwnerId: grant.OwnerID,
		GrantedScopes:        grant.GrantedScopes,
		CapabilityGrant:      grantBytes,
		AccountGeneration:    grant.AccountGeneration,
	}
}

// HasScope 检查 grant 是否包含指定的 scope
func (g *CapabilityGrant) HasScope(scope string) bool {
	for _, s := range g.GrantedScopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HasAnyScope 检查 grant 是否包含任意指定的 scopes
func (g *CapabilityGrant) HasAnyScope(scopes []string) bool {
	for _, scope := range scopes {
		if g.HasScope(scope) {
			return true
		}
	}
	return false
}

// HasAllScopes 检查 grant 是否包含所有指定的 scopes
func (g *CapabilityGrant) HasAllScopes(scopes []string) bool {
	for _, scope := range scopes {
		if !g.HasScope(scope) {
			return false
		}
	}
	return true
}

// joinScopes 将 scopes 连接为字符串
func joinScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	result := scopes[0]
	for i := 1; i < len(scopes); i++ {
		result += "," + scopes[i]
	}
	return result
}

// splitScopes 将字符串分割为 scopes
func splitScopes(s string) []string {
	if s == "" {
		return []string{}
	}
	result := []string{}
	current := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if current != "" {
				result = append(result, current)
				current = ""
			}
		} else {
			current += string(s[i])
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

// splitGrantParts 分割 grant 的各个部分（使用 | 分隔符）
func splitGrantParts(s string) []string {
	result := []string{}
	current := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			result = append(result, current)
			current = ""
		} else {
			current += string(s[i])
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

// parseInt64 解析 int64
func parseInt64(s string) int64 {
	var result int64
	fmt.Sscanf(s, "%d", &result)
	return result
}

// parseInt32 解析 int32
func parseInt32(s string) int32 {
	var result int32
	fmt.Sscanf(s, "%d", &result)
	return result
}
