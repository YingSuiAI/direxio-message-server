package storage

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/google/uuid"
)

// catalogSensitiveKeyRE is intentionally conservative: catalog snapshots are
// metadata, not a secret transport.  Rejecting an unknown key is safer than
// persisting a value that may later be rendered or replayed to another node.
var catalogSensitiveKeyRE = regexp.MustCompile(`(?i)(^|[^a-z0-9])(access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization|headers?|cookies?|bearer|basic|secret|token|pass(?:word|wd|phrase)|credential|api[_-]?key|private[_-]?key|aws[_-]?(access[_-]?key|secret[_-]?access[_-]?key|session[_-]?token))([^a-z0-9]|$)`)

var (
	catalogBearerBasicRE         = regexp.MustCompile(`(?i)(?:^|[\s:=])(?:bearer|basic)\s+\S+`)
	catalogSensitiveAssignmentRE = regexp.MustCompile(`(?i)(?:^|[\s,;])(?:access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization|cookies?|set-cookie)\s*[:=]\s*\S+`)
	catalogAWSCredentialRE       = regexp.MustCompile(`(?i)(?:^|[\s:=,;])(?:aws[_-]?(?:access[_-]?key[_-]?id|secret[_-]?access[_-]?key|session[_-]?token)|x-amz-security-token|access[_-]?key[_-]?id|secret[_-]?access[_-]?key|session[_-]?token)[\s:=]+\S+`)
	catalogAWSAccessKeyRE        = regexp.MustCompile(`\b(?:AKIA|ASIA|AIDA|AROA|ANPA|ANVA|ASCA|AGPA|AIPA)[A-Z0-9]{16}\b`)
	catalogPrivateKeyRE          = regexp.MustCompile(`(?i)-----begin(?: [a-z0-9]+)* private key-----`)
	catalogDigestRE              = regexp.MustCompile(`(?i)^(?:(?:sha(?:1|224|256|384|512):)?[a-f0-9]{40}|(?:sha(?:1|224|256|384|512):)?[a-f0-9]{56}|(?:sha(?:1|224|256|384|512):)?[a-f0-9]{64}|(?:sha(?:1|224|256|384|512):)?[a-f0-9]{96}|(?:sha(?:1|224|256|384|512):)?[a-f0-9]{128})$`)
	catalogAWSAccountIDRE        = regexp.MustCompile(`^[0-9]{12}$`)
	catalogAWSRegionRE           = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z0-9-]+-[0-9]$`)
	catalogCFStackNameRE         = regexp.MustCompile(`^dirextalk-v2-[0-9a-f]{24}$`)
	catalogEC2CapabilityRE       = regexp.MustCompile(`^target\.instance\.i-(?:[0-9a-f]{8}|[0-9a-f]{17})$`)
	catalogExecutionFenceRE      = regexp.MustCompile(`^([0-9a-f-]{36}):([1-9][0-9]*):([a-z0-9][a-z0-9._-]{0,127})$`)
	catalogDiagnosticKeyRE       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	catalogPinnedOCIImageRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}(?::[1-9][0-9]{0,4})?(?:/[a-z0-9][a-z0-9._-]{0,127})+@sha256:[a-f0-9]{64}$`)
	catalogLocalProbePathRE      = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@/-]{0,255}$`)
)

// validateCatalogSensitiveData recursively validates JSON-shaped values.  It
// fails closed for values that cannot be represented as JSON and never puts a
// rejected value in the returned error.
func validateCatalogSensitiveData(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrExecutionStoreInvalid
	}
	var decoded any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&decoded); err != nil {
		return ErrExecutionStoreInvalid
	}
	if rejectedAt := catalogSensitiveJSONFailure(decoded, "", "root"); rejectedAt == "" {
		return nil
	} else {
		// Only JSON field names matching a closed diagnostic grammar are ever
		// included. Values remain redacted even on validation failures.
		return fmt.Errorf("%w: unsafe catalog metadata at %s", ErrExecutionStoreInvalid, rejectedAt)
	}
}

func catalogSensitiveJSONValue(value any, key string) bool {
	return catalogSensitiveJSONFailure(value, key, "root") == ""
}

func catalogSensitiveJSONFailure(value any, key, path string) string {
	if key != "" {
		if allowed, terminal := catalogSafeExecutionMetadata(key, value); terminal {
			if allowed {
				return ""
			}
			return path
		}
	}
	// Treat punctuation lookalikes as the same sensitive key.  JSON keys are
	// untrusted metadata, so `private.key`, `private-key`, and `private_key`
	// must all stay on the fail-closed path while exact typed metadata keys are
	// still handled above by their closed allowlists.
	normalizedKey := strings.NewReplacer(".", "_").Replace(key)
	if key != "" && catalogSensitiveKeyRE.MatchString(normalizedKey) {
		allowed, terminal := catalogSafeCredentialMetadata(key, value)
		if !allowed {
			return path
		}
		if terminal {
			return ""
		}
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childPath := path + ".field"
			if catalogDiagnosticKeyRE.MatchString(k) {
				childPath = path + "." + k
			}
			if rejectedAt := catalogSensitiveJSONFailure(v[k], k, childPath); rejectedAt != "" {
				return rejectedAt
			}
		}
		return ""
	case []any:
		for _, child := range v {
			if rejectedAt := catalogSensitiveJSONFailure(child, "", path+"[]"); rejectedAt != "" {
				return rejectedAt
			}
		}
		return ""
	case string:
		if catalogSensitiveString(v) {
			return path
		}
		return ""
	default:
		return ""
	}
}

func catalogSafeExecutionMetadata(key string, value any) (allowed, terminal bool) {
	switch key {
	case "ami_parameter":
		v, ok := value.(string)
		return ok && v == coreexecution.AWSAL2023X8664AMIParameter, true
	case "stack_name":
		v, ok := value.(string)
		return ok && catalogCFStackNameRE.MatchString(v), true
	case "stack_id":
		v, ok := value.(string)
		if !ok {
			return false, true
		}
		parsed, err := arn.Parse(v)
		parts := strings.Split(parsed.Resource, "/")
		if err != nil || parsed.Service != "cloudformation" || !catalogAWSAccountIDRE.MatchString(parsed.AccountID) || !catalogAWSRegionRE.MatchString(parsed.Region) || len(parts) != 3 || parts[0] != "stack" || !catalogCFStackNameRE.MatchString(parts[1]) {
			return false, true
		}
		_, err = uuid.Parse(parts[2])
		return err == nil, true
	case "image":
		v, ok := value.(string)
		return ok && len(v) <= 2048 && catalogPinnedOCIImageRE.MatchString(v), true
	case "url":
		v, ok := value.(string)
		return ok && catalogTargetLocalProbeURL(v), true
	case "owner_id":
		v, ok := value.(string)
		v = strings.TrimSpace(v)
		return ok && len(v) >= 4 && len(v) <= 255 && strings.HasPrefix(v, "@") && strings.Contains(v[1:], ":") && !strings.ContainsAny(v, "\x00\r\n\t "), true
	case "fence":
		v, ok := value.(string)
		if !ok {
			return false, true
		}
		parts := catalogExecutionFenceRE.FindStringSubmatch(v)
		if len(parts) != 4 {
			return false, true
		}
		_, err := uuid.Parse(parts[1])
		return err == nil, true
	case "capabilities":
		values, ok := value.([]any)
		if !ok || len(values) > 64 {
			return false, true
		}
		for _, raw := range values {
			capability, ok := raw.(string)
			if !ok || !catalogKnownExecutionCapability(capability) {
				return false, true
			}
		}
		return true, true
	default:
		return false, false
	}
}

// catalogTargetLocalProbeURL accepts only the typed SSM health-probe shape.
// It intentionally excludes remote hosts, userinfo, queries, fragments and
// encoded path bytes so a URL field cannot become an opaque token carrier.
func catalogTargetLocalProbeURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 320 || strings.Contains(raw, "%") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Port() == "" || !catalogLocalProbePathRE.MatchString(u.Path) {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535 && u.String() == raw
}

func catalogKnownExecutionCapability(value string) bool {
	switch value {
	case "transport.aws_ssm", "target.aws_ec2_instance", "target.aws_compute_reservation",
		"runtime.container", "runtime.systemd", "probe.http", "probe.tcp",
		"artifact.fetch", "artifact.collect", "secret.reference", "compute.catalog", "compute.provision":
		return true
	default:
		return catalogEC2CapabilityRE.MatchString(value)
	}
}

// Durable execution snapshots carry credential identities and immutable
// references, never credential values. Keep this exception deliberately
// closed: only the exact wire keys and independently valid metadata shapes
// pass, while lookalike keys and raw secret-bearing objects still fail.
func catalogSafeCredentialMetadata(key string, value any) (allowed, terminal bool) {
	switch key {
	case "credential_refs", "secret_refs":
		return true, false
	case "credential_id":
		v, ok := value.(string)
		if !ok {
			return false, true
		}
		_, err := uuid.Parse(v)
		return err == nil, true
	case "credential_revision":
		v, ok := value.(float64)
		return ok && v >= 1 && v <= float64(^uint64(0)) && math.Trunc(v) == v, true
	case "secret_ref":
		v, ok := value.(string)
		if !ok {
			return false, true
		}
		_, err := uuid.Parse(v)
		return err == nil, true
	case "secret_revision":
		v, ok := value.(float64)
		return ok && v >= 1 && v <= float64(^uint64(0)) && math.Trunc(v) == v, true
	case "secret_purpose":
		v, ok := value.(string)
		return ok && v == coreexecution.AISecretPurposeProviderAPIKey, true
	case "secret_binding_digest":
		v, ok := value.(string)
		return ok && coreexecution.Digest(v).Valid(), true
	case "credential_account_id":
		v, ok := value.(string)
		return ok && catalogAWSAccountIDRE.MatchString(v), true
	case "credential_region":
		v, ok := value.(string)
		return ok && catalogAWSRegionRE.MatchString(v), true
	case "credential_user_arn":
		v, ok := value.(string)
		if !ok || len(v) > 2048 {
			return false, true
		}
		parsed, err := arn.Parse(v)
		return err == nil && (parsed.Service == "iam" || parsed.Service == "sts") && catalogAWSAccountIDRE.MatchString(parsed.AccountID) && parsed.Resource != "" && parsed.String() == v, true
	default:
		return false, false
	}
}

func catalogSensitiveString(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || catalogDigestOrUUID(v) {
		return false
	}
	// Go's JSON encoding emits time.Time as RFC3339/RFC3339Nano. Timestamps
	// are common in immutable quote and observation metadata and are not a
	// secret transport. Sensitive field names are rejected before reaching
	// this value-level exception.
	if _, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return false
	}
	// This is a closed, server-generated observation enum, not caller data.
	// Its descriptive spelling happens to cross the generic entropy threshold;
	// rejecting it makes every successfully inspected AWS SSM target
	// impossible to persist and therefore impossible to plan against.
	if v == coreexecution.ObservationFactHTTPSEgressValue {
		return false
	}
	if catalogBearerBasicRE.MatchString(v) || catalogSensitiveAssignmentRE.MatchString(v) || catalogAWSCredentialRE.MatchString(v) || catalogAWSAccessKeyRE.MatchString(v) || catalogPrivateKeyRE.MatchString(v) {
		return true
	}
	return catalogHighEntropy(v)
}

func catalogDigestOrUUID(value string) bool {
	if _, err := uuid.Parse(value); err == nil {
		return true
	}
	return catalogDigestRE.MatchString(value)
}

func catalogHighEntropy(value string) bool {
	if len(value) < 24 {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			return false
		}
	}
	counts := make(map[rune]int)
	for _, r := range value {
		counts[r]++
	}
	length := float64(len([]rune(value)))
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.75
}
