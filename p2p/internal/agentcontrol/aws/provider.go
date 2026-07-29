package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
type Stack struct {
	Region, StackName string
	Status            string
	TemplateSHA256    string
	Parameters, Tags  map[string]string
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
	f.Calls = append(f.Calls, "create_change_set")
	if handle.Region != r.Region {
		return ChangeSet{}, ErrConflict
	}
	if e := f.maybeFail("create_change_set"); e != nil {
		return ChangeSet{}, e
	}
	digest := canonicalDigest(struct {
		Region, Stack, Name string
		Operation           Operation
		Template            []byte
		Parameters, Tags    map[string]string
		Capabilities        []string
	}{r.Region, r.StackName, r.ChangeSetName, r.Operation, r.Template, r.Parameters, r.Tags, r.Capabilities})
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
	cs := ChangeSet{ID: id, Name: r.ChangeSetName, StackName: r.StackName, Region: r.Region, RequestDigest: digest, ClientToken: r.ClientToken, Status: "CREATE_COMPLETE", ExecutionStatus: "AVAILABLE", Operation: r.Operation, TemplateSHA256: templateDigest, Parameters: cloneMap(r.Parameters), Tags: cloneMap(r.Tags)}
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
	if _, ok := f.Stacks[region+"/"+stack]; !ok {
		if f.ResponseLoss || f.ResponseLossDelete {
			f.DeletedTokens[token] = true
			return ErrResponseUncertain
		}
		return ErrNotFound
	}
	if f.Async {
		current := f.Stacks[region+"/"+stack]
		current.Status = "DELETE_IN_PROGRESS"
		f.Stacks[region+"/"+stack] = current
	} else {
		delete(f.Stacks, region+"/"+stack)
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
