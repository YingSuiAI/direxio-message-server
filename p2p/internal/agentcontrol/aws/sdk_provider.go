package aws

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

const changeSetDescriptionPrefix = "dirextalk-core-v1:"

func changeSetDescription(token, digest string) string {
	return changeSetDescriptionPrefix + token + ":" + digest
}

func parseChangeSetDescription(description string) (token, digest string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(description, changeSetDescriptionPrefix), ":")
	if !strings.HasPrefix(description, changeSetDescriptionPrefix) || len(parts) != 2 || !validUUID(parts[0]) || len(parts[1]) != 64 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// STSClient and CloudFormationClient are intentionally narrow seams for unit
// tests.  SDK clients are always created from the credential handle supplied
// for the operation; no ambient provider chain is consulted.
type STSClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type CloudFormationClient interface {
	CreateChangeSet(context.Context, *cloudformation.CreateChangeSetInput, ...func(*cloudformation.Options)) (*cloudformation.CreateChangeSetOutput, error)
	DescribeChangeSet(context.Context, *cloudformation.DescribeChangeSetInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeChangeSetOutput, error)
	ExecuteChangeSet(context.Context, *cloudformation.ExecuteChangeSetInput, ...func(*cloudformation.Options)) (*cloudformation.ExecuteChangeSetOutput, error)
	DeleteStack(context.Context, *cloudformation.DeleteStackInput, ...func(*cloudformation.Options)) (*cloudformation.DeleteStackOutput, error)
	DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
	GetTemplate(context.Context, *cloudformation.GetTemplateInput, ...func(*cloudformation.Options)) (*cloudformation.GetTemplateOutput, error)
}

// ClientFactory constructs clients scoped to one exact stored credential.
// Implementations must not accept endpoint overrides or ambient credentials.
type ClientFactory interface {
	NewSTS(CredentialHandle) (STSClient, error)
	NewCloudFormation(CredentialHandle) (CloudFormationClient, error)
}

// SDKClients is a convenient injectable factory for unit tests. Production
// callers should use SDKFactory so each operation receives static credentials.
type SDKClients struct {
	STS            STSClient
	CloudFormation CloudFormationClient
}

func (c SDKClients) NewSTS(CredentialHandle) (STSClient, error) {
	if c.STS == nil {
		return nil, ErrInvalid
	}
	return c.STS, nil
}

func (c SDKClients) NewCloudFormation(CredentialHandle) (CloudFormationClient, error) {
	if c.CloudFormation == nil {
		return nil, ErrInvalid
	}
	return c.CloudFormation, nil
}

type SDKFactory struct{}

func NewSDKFactory() *SDKFactory { return &SDKFactory{} }

func (f *SDKFactory) NewSTS(handle CredentialHandle) (STSClient, error) {
	cfg, err := staticAWSConfig(handle)
	if err != nil {
		return nil, err
	}
	return sts.NewFromConfig(cfg), nil
}

func (f *SDKFactory) NewCloudFormation(handle CredentialHandle) (CloudFormationClient, error) {
	cfg, err := staticAWSConfig(handle)
	if err != nil {
		return nil, err
	}
	return cloudformation.NewFromConfig(cfg), nil
}

func staticAWSConfig(handle CredentialHandle) (aws.Config, error) {
	if handle.credential == nil || !validRegion(handle.Region) || strings.TrimSpace(handle.credential.accessKeyID) == "" || strings.TrimSpace(handle.credential.secretAccessKey) == "" {
		return aws.Config{}, ErrInvalid
	}
	provider := credentials.NewStaticCredentialsProvider(handle.credential.accessKeyID, handle.credential.secretAccessKey, handle.credential.sessionToken)
	return aws.Config{Region: handle.Region, Credentials: aws.NewCredentialsCache(provider), RetryMode: aws.RetryModeStandard, RetryMaxAttempts: 3}, nil
}

// StaticAWSConfig exposes the strict static-credential construction for
// package-level tests and adapters. It never loads environment, profile, or
// metadata credentials.
func StaticAWSConfig(handle CredentialHandle) (aws.Config, error) {
	return staticAWSConfig(handle)
}

type SDKProvider struct {
	factory ClientFactory
	timeout time.Duration
	mu      sync.RWMutex
	known   map[string]ChangeSet
}

type SDKProviderOption func(*SDKProvider) error

func WithSDKTimeout(timeout time.Duration) SDKProviderOption {
	return func(provider *SDKProvider) error {
		if timeout <= 0 || timeout > 5*time.Minute {
			return ErrInvalid
		}
		provider.timeout = timeout
		return nil
	}
}

func NewSDKProvider(factory ClientFactory, options ...SDKProviderOption) (*SDKProvider, error) {
	if factory == nil {
		return nil, ErrInvalid
	}
	p := &SDKProvider{factory: factory, timeout: 30 * time.Second, known: make(map[string]ChangeSet)}
	for _, option := range options {
		if option == nil || option(p) != nil {
			return nil, ErrInvalid
		}
	}
	return p, nil
}

func (p *SDKProvider) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, p.timeout)
}

func (p *SDKProvider) GetCallerIdentity(ctx context.Context, handle CredentialHandle) (Identity, error) {
	if p == nil || p.factory == nil || !validRegion(handle.Region) || handle.credential == nil {
		return Identity{}, ErrInvalid
	}
	client, err := p.factory.NewSTS(handle)
	if err != nil {
		return Identity{}, ErrInvalid
	}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	out, err := client.GetCallerIdentity(callCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Identity{}, mapSDKError(err, false)
	}
	if out == nil || out.Account == nil || out.Arn == nil || out.UserId == nil {
		return Identity{}, ErrProvider
	}
	parsed, err := arn.Parse(aws.ToString(out.Arn))
	if err != nil || (parsed.Service != "iam" && parsed.Service != "sts") || !accountIDValid(aws.ToString(out.Account)) || parsed.AccountID != aws.ToString(out.Account) || aws.ToString(out.UserId) == "" || handle.AccountID != "" && handle.AccountID != aws.ToString(out.Account) || handle.UserARN != "" && handle.UserARN != parsed.String() {
		return Identity{}, ErrProvider
	}
	return Identity{AccountID: parsed.AccountID, UserARN: parsed.String(), PrincipalID: aws.ToString(out.UserId)}, nil
}

func (p *SDKProvider) CreateChangeSet(ctx context.Context, handle CredentialHandle, req ChangeSetRequest) (ChangeSet, error) {
	if err := validateHandleRegion(handle, req.Region); err != nil {
		return ChangeSet{}, err
	}
	if (req.Operation != OperationCreate && req.Operation != OperationUpdate) || !validStackName(req.StackName) || strings.TrimSpace(req.ChangeSetName) == "" || strings.TrimSpace(req.ClientToken) == "" || len(req.Template) == 0 {
		return ChangeSet{}, ErrInvalid
	}
	client, err := p.factory.NewCloudFormation(handle)
	if err != nil {
		return ChangeSet{}, ErrInvalid
	}
	changeType := cloudformationtypes.ChangeSetTypeCreate
	if req.Operation == OperationUpdate {
		changeType = cloudformationtypes.ChangeSetTypeUpdate
	}
	digest := providerRequestDigest(Plan{Region: req.Region, StackName: req.StackName, Operation: req.Operation, Template: req.Template, Parameters: req.Parameters, Tags: req.Tags, Capabilities: req.Capabilities}, req.ClientToken)
	in := &cloudformation.CreateChangeSetInput{StackName: aws.String(req.StackName), ChangeSetName: aws.String(req.ChangeSetName), ClientToken: aws.String(req.ClientToken), Description: aws.String(changeSetDescription(req.ClientToken, digest)), ChangeSetType: changeType, TemplateBody: aws.String(string(req.Template)), Parameters: parametersToSDK(req.Parameters), Tags: tagsToSDK(req.Tags), Capabilities: capabilitiesToSDK(req.Capabilities)}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	out, err := client.CreateChangeSet(callCtx, in)
	if err != nil {
		return ChangeSet{}, mapSDKError(err, true)
	}
	if out == nil || out.Id == nil && out.StackId == nil {
		return ChangeSet{}, ErrProvider
	}
	id := aws.ToString(out.Id)
	if id == "" {
		id = req.ChangeSetName
	}
	_, templateSHA, _ := normalizeTemplate(req.Template)
	cs := ChangeSet{ID: id, Name: req.ChangeSetName, StackName: req.StackName, ClientToken: req.ClientToken, Region: req.Region, RequestDigest: canonicalDigest(struct {
		Region, Stack, Name string
		Operation           Operation
		Template            []byte
		Parameters, Tags    map[string]string
		Capabilities        []string
	}{req.Region, req.StackName, req.ChangeSetName, req.Operation, req.Template, req.Parameters, req.Tags, req.Capabilities}), Operation: req.Operation, TemplateSHA256: templateSHA, Parameters: cloneMap(req.Parameters), Tags: cloneMap(req.Tags)}
	p.mu.Lock()
	p.known[changeSetKey(req.Region, req.StackName, req.ChangeSetName)] = cs
	p.known[changeSetKey(req.Region, req.StackName, id)] = cs
	p.mu.Unlock()
	// CreateChangeSet is asynchronous. Do not hand an unverified change set to
	// the execution stage: CloudFormation must report CREATE_COMPLETE and
	// AVAILABLE through a typed read first. If that read cannot establish the
	// outcome, return uncertainty so the durable workflow can reconcile it.
	return p.waitForChangeSet(callCtx, client, req.Region, req.StackName, req.ChangeSetName, cs)
}

func (p *SDKProvider) waitForChangeSet(ctx context.Context, client CloudFormationClient, region, stack, name string, known ChangeSet) (ChangeSet, error) {
	if client == nil {
		return ChangeSet{}, ErrInvalid
	}
	for {
		out, err := client.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{StackName: aws.String(stack), ChangeSetName: aws.String(name)})
		if err == nil {
			cs := p.decodeChangeSet(out, region, stack, name, known)
			switch {
			case cs.Status == string(cloudformationtypes.ChangeSetStatusCreateComplete) && cs.ExecutionStatus == string(cloudformationtypes.ExecutionStatusAvailable):
				p.mu.Lock()
				p.known[changeSetKey(region, stack, cs.Name)] = cs
				p.known[changeSetKey(region, stack, cs.ID)] = cs
				p.mu.Unlock()
				return cs, nil
			case cs.Status == "FAILED" || cs.Status == "DELETE_COMPLETE":
				return cs, ErrProvider
			}
		}
		select {
		case <-ctx.Done():
			return ChangeSet{}, ErrResponseUncertain
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (p *SDKProvider) decodeChangeSet(out *cloudformation.DescribeChangeSetOutput, region, stack, name string, known ChangeSet) ChangeSet {
	if out == nil {
		return known
	}
	cs := ChangeSet{ID: aws.ToString(out.ChangeSetId), Name: aws.ToString(out.ChangeSetName), StackName: stack, Region: region, Status: string(out.Status), ExecutionStatus: string(out.ExecutionStatus), Parameters: parametersFromSDK(out.Parameters), Tags: tagsFromSDK(out.Tags)}
	if cs.Name == "" {
		cs.Name = name
	}
	if cs.ID == "" {
		cs.ID = cs.Name
	}
	if token, digest, ok := parseChangeSetDescription(aws.ToString(out.Description)); ok {
		cs.ClientToken, cs.RequestDigest = token, digest
	}
	if known.ID != "" {
		cs.ClientToken, cs.RequestDigest, cs.Operation, cs.TemplateSHA256 = known.ClientToken, known.RequestDigest, known.Operation, known.TemplateSHA256
		if len(cs.Parameters) == 0 {
			cs.Parameters = cloneMap(known.Parameters)
		}
		if len(cs.Tags) == 0 {
			cs.Tags = cloneMap(known.Tags)
		}
	}
	return cs
}

func (p *SDKProvider) DescribeChangeSet(ctx context.Context, handle CredentialHandle, region, stack, name string) (ChangeSet, error) {
	if err := validateHandleRegion(handle, region); err != nil {
		return ChangeSet{}, err
	}
	if !validStackName(stack) || strings.TrimSpace(name) == "" {
		return ChangeSet{}, ErrInvalid
	}
	client, err := p.factory.NewCloudFormation(handle)
	if err != nil {
		return ChangeSet{}, ErrInvalid
	}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	out, err := client.DescribeChangeSet(callCtx, &cloudformation.DescribeChangeSetInput{StackName: aws.String(stack), ChangeSetName: aws.String(name)})
	if err != nil {
		return ChangeSet{}, mapSDKError(err, false)
	}
	if out == nil || out.ChangeSetName == nil {
		return ChangeSet{}, ErrProvider
	}
	p.mu.RLock()
	known := p.known[changeSetKey(region, stack, name)]
	if known.ID == "" {
		known = p.known[changeSetKey(region, stack, aws.ToString(out.ChangeSetId))]
	}
	p.mu.RUnlock()
	return p.decodeChangeSet(out, region, stack, name, known), nil
}

func changeSetKey(region, stack, name string) string { return region + "\x00" + stack + "\x00" + name }

func (p *SDKProvider) ExecuteChangeSet(ctx context.Context, handle CredentialHandle, region, stack, id, token string) error {
	if err := validateHandleRegion(handle, region); err != nil {
		return err
	}
	if !validStackName(stack) || strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
		return ErrInvalid
	}
	client, err := p.factory.NewCloudFormation(handle)
	if err != nil {
		return ErrInvalid
	}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	_, err = client.ExecuteChangeSet(callCtx, &cloudformation.ExecuteChangeSetInput{StackName: aws.String(stack), ChangeSetName: aws.String(id), ClientRequestToken: aws.String(token)})
	if err != nil {
		return mapSDKError(err, true)
	}
	return nil
}

func (p *SDKProvider) DeleteStack(ctx context.Context, handle CredentialHandle, region, stack, token string) error {
	if err := validateHandleRegion(handle, region); err != nil {
		return err
	}
	if !validStackName(stack) || strings.TrimSpace(token) == "" {
		return ErrInvalid
	}
	client, err := p.factory.NewCloudFormation(handle)
	if err != nil {
		return ErrInvalid
	}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	_, err = client.DeleteStack(callCtx, &cloudformation.DeleteStackInput{StackName: aws.String(stack), ClientRequestToken: aws.String(token)})
	if err != nil {
		return mapSDKError(err, true)
	}
	return nil
}

func (p *SDKProvider) DescribeStack(ctx context.Context, handle CredentialHandle, region, stack string) (Stack, error) {
	if err := validateHandleRegion(handle, region); err != nil {
		return Stack{}, err
	}
	if !validStackName(stack) {
		return Stack{}, ErrInvalid
	}
	client, err := p.factory.NewCloudFormation(handle)
	if err != nil {
		return Stack{}, ErrInvalid
	}
	callCtx, cancel := p.operationContext(ctx)
	defer cancel()
	out, err := client.DescribeStacks(callCtx, &cloudformation.DescribeStacksInput{StackName: aws.String(stack)})
	if err != nil {
		return Stack{}, mapSDKError(err, false)
	}
	if out == nil || len(out.Stacks) != 1 {
		return Stack{}, ErrProvider
	}
	s := out.Stacks[0]
	if aws.ToString(s.StackName) != stack {
		return Stack{}, ErrConflict
	}
	stackOut := Stack{Region: region, StackName: stack, Status: string(s.StackStatus), Parameters: parametersFromSDK(s.Parameters), Tags: tagsFromSDK(s.Tags)}
	templateOut, err := client.GetTemplate(callCtx, &cloudformation.GetTemplateInput{StackName: aws.String(stack)})
	if err != nil {
		return Stack{}, mapSDKError(err, false)
	}
	if templateOut == nil || templateOut.TemplateBody == nil {
		return Stack{}, ErrProvider
	}
	_, digest, err := normalizeTemplate([]byte(aws.ToString(templateOut.TemplateBody)))
	if err != nil {
		return Stack{}, ErrProvider
	}
	stackOut.TemplateSHA256 = digest
	return stackOut, nil
}

func validateHandleRegion(handle CredentialHandle, region string) error {
	if handle.credential == nil || handle.credential.accessKeyID == "" || handle.credential.secretAccessKey == "" || !validRegion(handle.Region) || !validRegion(region) {
		return ErrInvalid
	}
	if handle.Region != region {
		return ErrConflict
	}
	return nil
}

func accountIDValid(account string) bool {
	if len(account) != 12 {
		return false
	}
	for _, r := range account {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func mapSDKError(err error, mutation bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		if mutation {
			return ErrResponseUncertain
		}
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if mutation {
			return ErrResponseUncertain
		}
		return ErrProvider
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ValidationError", "ResourceNotFoundException", "ChangeSetNotFound":
			if strings.Contains(strings.ToLower(apiErr.ErrorMessage()), "does not exist") || apiErr.ErrorCode() != "ValidationError" {
				return ErrNotFound
			}
		case "AlreadyExistsException", "TokenAlreadyExists":
			return ErrIdempotencyConflict
		}
	}
	var netErr net.Error
	if mutation && errors.As(err, &netErr) {
		return ErrResponseUncertain
	}
	return ErrProvider
}

func parametersToSDK(values map[string]string) []cloudformationtypes.Parameter {
	out := make([]cloudformationtypes.Parameter, 0, len(values))
	for k, v := range values {
		out = append(out, cloudformationtypes.Parameter{ParameterKey: aws.String(k), ParameterValue: aws.String(v)})
	}
	return out
}

func parametersFromSDK(values []cloudformationtypes.Parameter) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		if v.ParameterKey != nil {
			out[aws.ToString(v.ParameterKey)] = aws.ToString(v.ParameterValue)
		}
	}
	return out
}

func tagsToSDK(values map[string]string) []cloudformationtypes.Tag {
	out := make([]cloudformationtypes.Tag, 0, len(values))
	for k, v := range values {
		out = append(out, cloudformationtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func tagsFromSDK(values []cloudformationtypes.Tag) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		if v.Key != nil {
			out[aws.ToString(v.Key)] = aws.ToString(v.Value)
		}
	}
	return out
}

func capabilitiesToSDK(values []string) []cloudformationtypes.Capability {
	out := make([]cloudformationtypes.Capability, 0, len(values))
	for _, v := range values {
		out = append(out, cloudformationtypes.Capability(v))
	}
	return out
}
