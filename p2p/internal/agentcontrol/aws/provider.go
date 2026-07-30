package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

// AWSChangeTaskHandler is the narrow seam for a future durable Task executor.
// The Core AWS RPC/domain layer only creates and reads change facts; provider
// execution is intentionally supplied by a separate runtime handler.
type AWSChangeTaskHandler interface {
	HandleAWSChange(context.Context, string) error
}

// STSProvider is the sole credential-validation port.
type STSProvider interface {
	GetCallerIdentity(context.Context, CredentialHandle) (Identity, error)
}

type ChangeSetRequest struct {
	Region, StackName, ChangeSetName, ClientToken string
	Operation                                     Operation
	Template                                      []byte
	Parameters, Tags                              map[string]string
	Capabilities                                  []string
}

// providerChangeSetName keeps the durable client idempotency token unchanged
// while giving CloudFormation a name whose first character is alphabetic.
// The token is a canonical UUID, so this mapping is deterministic and remains
// within CloudFormation's change-set name limits.
func providerChangeSetName(token string) string { return "dirextalk-" + token }

type ChangeSet struct {
	ID, Name, StackName, ClientToken string
	Region                           string
	RequestDigest                    string
	Status                           string
	ExecutionStatus                  string
	Operation                        Operation
	TemplateSHA256                   string
	Parameters, Tags                 map[string]string
}

// StackOutputKey is the deliberately small set of CloudFormation output
// names that may cross the provider boundary.  Output values are template
// controlled, so arbitrary output maps must never be exposed to callers.
type StackOutputKey string

const (
	StackOutputInstanceID    StackOutputKey = "InstanceId"
	StackOutputPublicIP      StackOutputKey = "PublicIp"
	StackOutputSecurityGroup StackOutputKey = "SecurityGroupId"
	StackOutputStackID       StackOutputKey = "StackId"
)

// RequiredOutputsTag is a durable, plan-bound marker for typed provisions.
// Its persisted value uses '+' as the provider-safe delimiter. Legacy plans
// may still contain comma-delimited values and are normalized at the provider
// boundary without changing their durable digest.
const RequiredOutputsTag = "dirextalk:required-outputs"

// StackOutputs contains only validated values for the allowlisted output
// keys.  It intentionally remains map-shaped so generic providers can carry
// the typed keys without changing existing Stack consumers.
type StackOutputs map[string]string

func (o StackOutputs) Clone() StackOutputs {
	if len(o) == 0 {
		return nil
	}
	out := make(StackOutputs, len(o))
	for k, v := range o {
		out[k] = v
	}
	return out
}

// HasAll reports whether readback contains every requested typed output. An
// empty requirement is intentionally satisfied for legacy generic plans.
func (o StackOutputs) HasAll(required ...string) bool {
	for _, key := range required {
		if !isAllowedStackOutputKey(key) || strings.TrimSpace(o[key]) == "" {
			return false
		}
	}
	return true
}

func isAllowedStackOutputKey(key string) bool {
	switch StackOutputKey(key) {
	case StackOutputInstanceID, StackOutputPublicIP, StackOutputSecurityGroup, StackOutputStackID:
		return true
	default:
		return false
	}
}

func requiredStackOutputs(p Plan) ([]string, bool) {
	raw, ok := p.Tags[RequiredOutputsTag]
	if !ok {
		return nil, true
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	delimiter := "+"
	if strings.Contains(raw, ",") {
		if strings.Contains(raw, "+") {
			return nil, false
		}
		delimiter = ","
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, delimiter) {
		key := strings.TrimSpace(part)
		if !isAllowedStackOutputKey(key) || seen[key] {
			return nil, false
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, len(out) > 0
}

// canonicalProviderTags is the narrow compatibility codec for values sent to
// CloudFormation. Persisted plan tags and their digests remain untouched.
func canonicalProviderTags(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := cloneMap(values)
	if raw, ok := out[RequiredOutputsTag]; ok && strings.Contains(raw, ",") && !strings.Contains(raw, "+") {
		outputs, valid := requiredStackOutputs(Plan{Tags: values})
		if valid {
			out[RequiredOutputsTag] = strings.Join(outputs, "+")
		}
	}
	return out
}

// validateProviderTags applies the shared CloudFormation boundary contract.
// It returns the provider form while leaving the durable/raw tag map intact.
func validateProviderTags(values map[string]string) (map[string]string, error) {
	if len(values) > 50 {
		return nil, ErrInvalid
	}
	if _, valid := requiredStackOutputs(Plan{Tags: values}); !valid {
		return nil, ErrInvalid
	}
	if isTypedEC2Plan(Plan{Tags: values}) {
		if _, present := values[RequiredOutputsTag]; !present {
			return nil, ErrInvalid
		}
	}
	canonical := canonicalProviderTags(values)
	for key, value := range canonical {
		if strings.HasPrefix(strings.ToLower(key), "aws:") || !validCloudFormationTagText(key, 128) || !validCloudFormationTagText(value, 256) {
			return nil, ErrInvalid
		}
	}
	return canonical, nil
}

func validCloudFormationTagText(value string, maxRunes int) bool {
	if !utf8.ValidString(value) || value == "" || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Z, r) {
			continue
		}
		switch r {
		case '_', '.', ':', '/', '=', '+', '-', '@':
		default:
			return false
		}
	}
	return true
}

type Stack struct {
	Region, StackName string
	Status            string
	TemplateSHA256    string
	Parameters, Tags  map[string]string
	Outputs           StackOutputs
}

// ProvisionReadbackFromStack is the sole conversion from provider values to a
// durable typed provision readback. Unknown output keys are intentionally
// discarded before persistence.
func ProvisionReadbackFromStack(outputs StackOutputs, observedAt time.Time) (ProvisionReadback, error) {
	allowed := map[string]string{}
	for _, key := range []StackOutputKey{StackOutputStackID, StackOutputInstanceID, StackOutputPublicIP, StackOutputSecurityGroup} {
		if value := strings.TrimSpace(outputs[string(key)]); value != "" {
			allowed[string(key)] = value
		}
	}
	r := ProvisionReadback{StackID: allowed[string(StackOutputStackID)], InstanceID: allowed[string(StackOutputInstanceID)], PublicIP: allowed[string(StackOutputPublicIP)], SecurityGroupID: allowed[string(StackOutputSecurityGroup)], ObservedAt: observedAt.UTC()}
	r.OutputDigest = canonicalDigest(allowed)
	if r.Validate() != nil {
		return ProvisionReadback{}, ErrInvalid
	}
	return r, nil
}

// CloudProvider is deliberately closed over the exact CloudFormation calls
// Core v1 needs; no generic API or HTTP escape hatch is provided.
type CloudProvider interface {
	CreateChangeSet(context.Context, CredentialHandle, ChangeSetRequest) (ChangeSet, error)
	DescribeChangeSet(context.Context, CredentialHandle, string, string, string) (ChangeSet, error)
	ExecuteChangeSet(context.Context, CredentialHandle, string, string, string, string) error
	DeleteStack(context.Context, CredentialHandle, string, string, string) error
	DescribeStack(context.Context, CredentialHandle, string, string) (Stack, error)
}

type FakeSTSProvider struct {
	mu       sync.Mutex
	Identity Identity
	Calls    int
	Err      error
}

func (f *FakeSTSProvider) GetCallerIdentity(_ context.Context, handle CredentialHandle) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if !validRegion(handle.Region) || handle.credential == nil {
		return Identity{}, ErrInvalid
	}
	if f.Err != nil {
		return Identity{}, f.Err
	}
	if f.Identity.AccountID == "" {
		return Identity{}, ErrProvider
	}
	return f.Identity, nil
}

type fakeStack struct {
	stack    Stack
	sets     map[string]ChangeSet
	executed map[string]bool
}
type FakeProvider struct {
	mu                                                          sync.Mutex
	Stacks                                                      map[string]Stack
	Changes                                                     map[string]ChangeSet
	Calls                                                       []string
	DescribeChangeSetNames                                      []string
	ResponseLoss                                                bool
	ResponseLossCreate, ResponseLossExecute, ResponseLossDelete bool
	Async                                                       bool
	PollSequence                                                map[string][]string
	DeletedTokens                                               map[string]bool
	fail                                                        map[string]error
}

func NewFakeProvider() *FakeProvider {
	return &FakeProvider{Stacks: map[string]Stack{}, Changes: map[string]ChangeSet{}, DeletedTokens: map[string]bool{}, fail: map[string]error{}, PollSequence: map[string][]string{}}
}
func (f *FakeProvider) SetFailure(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[op] = err
}
func (f *FakeProvider) maybeFail(op string) error {
	if e := f.fail[op]; e != nil {
		return e
	}
	return nil
}
func (f *FakeProvider) CreateChangeSet(_ context.Context, handle CredentialHandle, r ChangeSetRequest) (ChangeSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if handle.Region != r.Region {
		return ChangeSet{}, ErrConflict
	}
	if !validChangeSetName(r.ChangeSetName) {
		return ChangeSet{}, ErrInvalid
	}
	canonicalTags, validationErr := validateProviderTags(r.Tags)
	if validationErr != nil {
		return ChangeSet{}, validationErr
	}
	f.Calls = append(f.Calls, "create_change_set")
	if e := f.maybeFail("create_change_set"); e != nil {
		return ChangeSet{}, e
	}
	digest := providerRequestDigest(Plan{Region: r.Region, StackName: r.StackName, Operation: r.Operation, Template: r.Template, Parameters: r.Parameters, Tags: r.Tags, Capabilities: r.Capabilities}, r.ClientToken)
	for _, v := range f.Changes {
		if v.ClientToken == r.ClientToken {
			if v.Region != r.Region || v.StackName != r.StackName || v.RequestDigest != digest {
				return ChangeSet{}, ErrIdempotencyConflict
			}
			return v, nil
		}
	}
	id := fmt.Sprintf("cs-%d", len(f.Changes)+1)
	_, templateDigest, _ := normalizeTemplate(r.Template)
	cs := ChangeSet{ID: id, Name: r.ChangeSetName, StackName: r.StackName, Region: r.Region, RequestDigest: digest, ClientToken: r.ClientToken, Status: "CREATE_COMPLETE", ExecutionStatus: "AVAILABLE", Operation: r.Operation, TemplateSHA256: templateDigest, Parameters: cloneMap(r.Parameters), Tags: canonicalTags}
	f.Changes[id] = cs
	if f.ResponseLoss || f.ResponseLossCreate {
		return ChangeSet{}, ErrResponseUncertain
	}
	return cs, nil
}
func (f *FakeProvider) DescribeChangeSet(_ context.Context, handle CredentialHandle, region, stack, name string) (ChangeSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "describe_change_set")
	f.DescribeChangeSetNames = append(f.DescribeChangeSetNames, name)
	if handle.Region != region {
		return ChangeSet{}, ErrConflict
	}
	for _, v := range f.Changes {
		if v.StackName == stack && v.Name == name {
			if v.Region != region {
				return ChangeSet{}, ErrConflict
			}
			return v, nil
		}
	}
	return ChangeSet{}, ErrNotFound
}
func (f *FakeProvider) ExecuteChangeSet(_ context.Context, handle CredentialHandle, region, stack, id, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "execute_change_set")
	if handle.Region != region {
		return ErrConflict
	}
	if strings.TrimSpace(token) == "" {
		return ErrInvalid
	}
	if e := f.maybeFail("execute_change_set"); e != nil {
		return e
	}
	cs, ok := f.Changes[id]
	if !ok {
		return ErrNotFound
	}
	if cs.Region != region || cs.StackName != stack {
		return ErrConflict
	}
	if cs.ExecutionStatus == "EXECUTE_COMPLETE" {
		return nil
	}
	cs.ExecutionStatus = "EXECUTE_COMPLETE"
	f.Changes[id] = cs
	status := "CREATE_COMPLETE"
	if cs.Operation == OperationUpdate {
		status = "UPDATE_COMPLETE"
	}
	if f.Async {
		if cs.Operation == OperationUpdate {
			status = "UPDATE_IN_PROGRESS"
		} else {
			status = "CREATE_IN_PROGRESS"
		}
	}
	if cs.Operation == OperationUpdate {
		status = "UPDATE_COMPLETE"
	}
	f.Stacks[region+"/"+stack] = Stack{Region: region, StackName: stack, Status: status, TemplateSHA256: cs.TemplateSHA256, Parameters: cloneMap(cs.Parameters), Tags: cloneMap(cs.Tags)}
	if f.ResponseLoss || f.ResponseLossExecute {
		return ErrResponseUncertain
	}
	return nil
}
func (f *FakeProvider) DeleteStack(_ context.Context, handle CredentialHandle, region, stack, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "delete_stack")
	if handle.Region != region {
		return ErrConflict
	}
	if e := f.maybeFail("delete_stack"); e != nil {
		return e
	}
	if f.DeletedTokens[token] {
		return nil
	}
	stackKey := region + "/" + stack
	if parsed, err := arn.Parse(stack); err == nil && parsed.Service == "cloudformation" {
		parts := strings.Split(parsed.Resource, "/")
		if len(parts) == 3 && parts[0] == "stack" {
			stackKey = region + "/" + parts[1]
		}
	}
	if _, ok := f.Stacks[stackKey]; !ok {
		if f.ResponseLoss || f.ResponseLossDelete {
			f.DeletedTokens[token] = true
			return ErrResponseUncertain
		}
		return ErrNotFound
	}
	if f.Async {
		current := f.Stacks[stackKey]
		current.Status = "DELETE_IN_PROGRESS"
		f.Stacks[stackKey] = current
	} else {
		delete(f.Stacks, stackKey)
	}
	f.DeletedTokens[token] = true
	if f.ResponseLoss || f.ResponseLossDelete {
		return ErrResponseUncertain
	}
	return nil
}
func (f *FakeProvider) DescribeStack(_ context.Context, handle CredentialHandle, region, stack string) (Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "describe_stack")
	if handle.Region != region {
		return Stack{}, ErrConflict
	}
	s, ok := f.Stacks[region+"/"+stack]
	if !ok {
		return Stack{}, ErrNotFound
	}
	if seq := f.PollSequence[region+"/"+stack]; len(seq) > 0 {
		s.Status = seq[0]
		f.PollSequence[region+"/"+stack] = seq[1:]
		f.Stacks[region+"/"+stack] = s
	}
	s.Outputs = s.Outputs.Clone()
	return s, nil
}
func (f *FakeProvider) UnconfirmedMutationCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == "execute_change_set" || c == "delete_stack" {
			n++
		}
	}
	return n
}
