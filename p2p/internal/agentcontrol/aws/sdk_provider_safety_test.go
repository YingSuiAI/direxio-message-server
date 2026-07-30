package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

type safetyCloudClient struct {
	createDescription         string
	createName, createToken   string
	createCalls               int
	createTags                []cloudformationtypes.Tag
	describeNames             []string
	executeToken, deleteToken string
	executeErr                error
	describeStatuses          []cloudformationtypes.ChangeSetStatus
	describeExecutions        []cloudformationtypes.ExecutionStatus
	describeCalls             int
	describeStack             *cloudformation.DescribeStacksOutput
	templateBody              string
}

func (c *safetyCloudClient) CreateChangeSet(_ context.Context, in *cloudformation.CreateChangeSetInput, _ ...func(*cloudformation.Options)) (*cloudformation.CreateChangeSetOutput, error) {
	c.createCalls++
	c.createDescription = aws.ToString(in.Description)
	c.createName, c.createToken = aws.ToString(in.ChangeSetName), aws.ToString(in.ClientToken)
	c.createTags = append([]cloudformationtypes.Tag(nil), in.Tags...)
	return &cloudformation.CreateChangeSetOutput{Id: aws.String("cs-safety")}, nil
}
func (c *safetyCloudClient) DescribeChangeSet(_ context.Context, in *cloudformation.DescribeChangeSetInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeChangeSetOutput, error) {
	c.describeCalls++
	c.describeNames = append(c.describeNames, aws.ToString(in.ChangeSetName))
	status := cloudformationtypes.ChangeSetStatusCreateComplete
	execution := cloudformationtypes.ExecutionStatusAvailable
	if len(c.describeStatuses) > 0 {
		status = c.describeStatuses[0]
		c.describeStatuses = c.describeStatuses[1:]
	}
	if len(c.describeExecutions) > 0 {
		execution = c.describeExecutions[0]
		c.describeExecutions = c.describeExecutions[1:]
	}
	return &cloudformation.DescribeChangeSetOutput{ChangeSetId: aws.String("cs-safety"), ChangeSetName: aws.String("change"), Description: aws.String(c.createDescription), Status: status, ExecutionStatus: execution}, nil
}
func (c *safetyCloudClient) ExecuteChangeSet(_ context.Context, in *cloudformation.ExecuteChangeSetInput, _ ...func(*cloudformation.Options)) (*cloudformation.ExecuteChangeSetOutput, error) {
	c.executeToken = aws.ToString(in.ClientRequestToken)
	return &cloudformation.ExecuteChangeSetOutput{}, c.executeErr
}
func (c *safetyCloudClient) DeleteStack(_ context.Context, in *cloudformation.DeleteStackInput, _ ...func(*cloudformation.Options)) (*cloudformation.DeleteStackOutput, error) {
	c.deleteToken = aws.ToString(in.ClientRequestToken)
	return &cloudformation.DeleteStackOutput{}, nil
}
func (c *safetyCloudClient) DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	if c.describeStack == nil {
		return nil, ErrNotFound
	}
	return c.describeStack, nil
}
func (c *safetyCloudClient) GetTemplate(context.Context, *cloudformation.GetTemplateInput, ...func(*cloudformation.Options)) (*cloudformation.GetTemplateOutput, error) {
	if c.templateBody == "" {
		return nil, ErrNotFound
	}
	return &cloudformation.GetTemplateOutput{TemplateBody: aws.String(c.templateBody)}, nil
}

func safetyHandle() CredentialHandle {
	now := time.Now().UTC()
	return RehydrateCredentials("11111111-1111-4111-8111-111111111111", "safety", "us-east-1", "", "", []byte("AKIA"), []byte("secret"), nil, 0, 1, now, now).handle()
}

func TestSDKProviderSafetyTokensAndFreshRecovery(t *testing.T) {
	client := &safetyCloudClient{}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client})
	if err != nil {
		t.Fatal(err)
	}
	template, digest, err := NormalizeTemplate([]byte(`{"Resources":{"Bucket":{"Type":"AWS::S3::Bucket"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	token := "11111111-1111-4111-8111-111111111112"
	if operationKey("change", token, string(ProviderMutationCreate), 1, 1) != operationKey("change", token, string(ProviderMutationCreate), 2, 99) {
		t.Fatal("provider action identity changed across lease reclaim")
	}
	req := ChangeSetRequest{Region: "us-east-1", StackName: "safety-stack", ChangeSetName: providerChangeSetName(token), ClientToken: token, Operation: OperationCreate, Template: template, Parameters: map[string]string{}, Tags: map[string]string{RequiredOutputsTag: "InstanceId,PublicIp,SecurityGroupId,StackId"}}
	if _, err = p.CreateChangeSet(context.Background(), safetyHandle(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "1") || !strings.HasPrefix(client.createName, "dirextalk-") || client.createName != providerChangeSetName(token) || client.createToken != token || client.createName == client.createToken {
		t.Fatalf("create input name/token=%q/%q", client.createName, client.createToken)
	}
	if len(client.createTags) != 1 || aws.ToString(client.createTags[0].Value) != "InstanceId+PublicIp+SecurityGroupId+StackId" {
		t.Fatalf("provider tags=%v", client.createTags)
	}
	if client.createDescription == "" || len(client.createDescription) > 1024 || strings.Contains(client.createDescription, "secret") || strings.Contains(client.createDescription, "AKIA") {
		t.Fatalf("unsafe description %q", client.createDescription)
	}
	if _, _, ok := parseChangeSetDescription(client.createDescription); !ok {
		t.Fatalf("description is not token/digest binding: %q", client.createDescription)
	}
	// A new provider has no known map and must reconstruct the durable binding from AWS data.
	fresh, _ := NewSDKProvider(SDKClients{CloudFormation: client})
	got, err := fresh.DescribeChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", providerChangeSetName(token))
	if err != nil || got.ClientToken != token || got.RequestDigest != ProviderRequestDigest(Plan{Region: req.Region, StackName: req.StackName, Operation: req.Operation, Template: template, Parameters: req.Parameters, Tags: req.Tags}, token) || digest == "" {
		t.Fatalf("fresh recovery=%+v err=%v", got, err)
	}
	if len(client.describeNames) == 0 || client.describeNames[len(client.describeNames)-1] != providerChangeSetName(token) {
		t.Fatalf("recovery describe names=%v", client.describeNames)
	}
	if err = p.ExecuteChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", "cs-safety", token); err != nil || client.executeToken != token {
		t.Fatalf("execute token=%q err=%v", client.executeToken, err)
	}
	if err = p.DeleteStack(context.Background(), safetyHandle(), "us-east-1", "safety-stack", token); err != nil || client.deleteToken != token {
		t.Fatalf("delete token=%q err=%v", client.deleteToken, err)
	}
	stackARN := "arn:aws:cloudformation:us-east-1:123456789012:stack/safety-stack/01234567-89ab-cdef-0123-456789abcdef"
	if err = p.DeleteStack(context.Background(), safetyHandle(), "us-east-1", stackARN, token); err != nil || client.deleteToken != token {
		t.Fatalf("authoritative stack ARN delete token=%q err=%v", client.deleteToken, err)
	}
	if err = p.DeleteStack(context.Background(), safetyHandle(), "us-east-1", "arn:aws:cloudformation:eu-west-1:123456789012:stack/safety-stack/01234567-89ab-cdef-0123-456789abcdef", token); err != ErrInvalid {
		t.Fatalf("cross-region stack ARN err=%v, want ErrInvalid", err)
	}
	client.executeErr = context.Canceled
	if err = p.ExecuteChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", "cs-safety", token); err != ErrResponseUncertain {
		t.Fatalf("cancelled mutation err=%v", err)
	}
}

func TestSDKProviderRejectsDigitLeadingChangeSetNameLocally(t *testing.T) {
	client := &safetyCloudClient{}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client})
	if err != nil {
		t.Fatal(err)
	}
	token := "11111111-1111-4111-8111-111111111114"
	_, err = p.CreateChangeSet(context.Background(), safetyHandle(), ChangeSetRequest{
		Region: "us-east-1", StackName: "safety-stack", ChangeSetName: token, ClientToken: token,
		Operation: OperationCreate, Template: []byte(`{"Resources":{}}`),
	})
	if err != ErrInvalid || client.createCalls != 0 {
		t.Fatalf("digit-leading name err=%v create calls=%d", err, client.createCalls)
	}
}

func TestSDKProviderRejectsInvalidTagsBeforeCreateCall(t *testing.T) {
	base := ChangeSetRequest{Region: "us-east-1", StackName: "safety-stack", ChangeSetName: "safe-change", ClientToken: "11111111-1111-4111-8111-111111111115", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`)}
	cases := []struct {
		name string
		tags map[string]string
	}{
		{name: "reserved", tags: map[string]string{"aws:reserved": "x"}},
		{name: "invalid key", tags: map[string]string{"bad?key": "x"}},
		{name: "invalid value", tags: map[string]string{"key": "bad?value"}},
		{name: "empty key", tags: map[string]string{"": "x"}},
		{name: "empty value", tags: map[string]string{"key": ""}},
		{name: "quote key", tags: map[string]string{"quote\"key": "x"}},
		{name: "malformed required outputs", tags: map[string]string{RequiredOutputsTag: "InstanceId+InstanceId"}},
		{name: "typed marker missing", tags: map[string]string{"service": EC2ServiceProfile, "dirextalk:template-profile": EC2ServiceProfile, "dirextalk:template-version": ec2TemplateVersion}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &safetyCloudClient{}
			p, err := NewSDKProvider(SDKClients{CloudFormation: client})
			if err != nil {
				t.Fatal(err)
			}
			req := base
			req.Tags = tc.tags
			if _, err = p.CreateChangeSet(context.Background(), safetyHandle(), req); err != ErrInvalid || client.createCalls != 0 {
				t.Fatalf("invalid tags err=%v create calls=%d", err, client.createCalls)
			}
		})
	}
	tooMany := map[string]string{}
	for i := 0; i < 51; i++ {
		tooMany[fmt.Sprintf("key-%d", i)] = "x"
	}
	client := &safetyCloudClient{}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client})
	if err != nil {
		t.Fatal(err)
	}
	base.Tags = tooMany
	if _, err = p.CreateChangeSet(context.Background(), safetyHandle(), base); err != ErrInvalid || client.createCalls != 0 {
		t.Fatalf("too many tags err=%v create calls=%d", err, client.createCalls)
	}
}

func TestSDKProviderWaitsForChangeSetAvailability(t *testing.T) {
	client := &safetyCloudClient{
		describeStatuses:   []cloudformationtypes.ChangeSetStatus{cloudformationtypes.ChangeSetStatusCreateInProgress, cloudformationtypes.ChangeSetStatusCreateComplete},
		describeExecutions: []cloudformationtypes.ExecutionStatus{cloudformationtypes.ExecutionStatusUnavailable, cloudformationtypes.ExecutionStatusAvailable},
	}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client}, WithSDKTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	template, _, err := NormalizeTemplate([]byte(`{"Resources":{"Queue":{"Type":"AWS::SQS::Queue"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.CreateChangeSet(context.Background(), safetyHandle(), ChangeSetRequest{
		Region: "us-east-1", StackName: "pending-stack", ChangeSetName: providerChangeSetName("11111111-1111-4111-8111-111111111113"), ClientToken: "11111111-1111-4111-8111-111111111113", Operation: OperationCreate, Template: template,
	})
	if err != nil {
		t.Fatalf("waited change set returned err=%v", err)
	}
	if client.describeCalls != 2 {
		t.Fatalf("describe calls=%d, want 2", client.describeCalls)
	}
}

func TestSDKProviderDescribeStackAllowlistedOutputs(t *testing.T) {
	client := &safetyCloudClient{
		describeStack: &cloudformation.DescribeStacksOutput{Stacks: []cloudformationtypes.Stack{{
			StackName: aws.String("output-stack"), StackStatus: cloudformationtypes.StackStatusCreateComplete,
			StackId: aws.String("arn:aws:cloudformation:us-east-1:123456789012:stack/output-stack/01234567-89ab-cdef-0123-456789abcdef"),
			Outputs: []cloudformationtypes.Output{
				{OutputKey: aws.String("InstanceId"), OutputValue: aws.String(" I-0123456789abcdef0 ")},
				{OutputKey: aws.String("PublicIp"), OutputValue: aws.String("192.0.2.10")},
				{OutputKey: aws.String("SecurityGroupId"), OutputValue: aws.String("sg-0123456789abcdef0")},
				{OutputKey: aws.String("StackId"), OutputValue: aws.String("secret-forged-stack-id")},
				{OutputKey: aws.String("Secret"), OutputValue: aws.String("do-not-return")},
				{OutputKey: aws.String("InstanceId"), OutputValue: aws.String("not-an-instance")},
				{OutputKey: aws.String("PublicIp"), OutputValue: aws.String("not-an-ip")},
			},
		}}},
		templateBody: `{"Resources":{}}`,
	}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client})
	if err != nil {
		t.Fatal(err)
	}
	stack, err := p.DescribeStack(context.Background(), safetyHandle(), "us-east-1", "output-stack")
	if err != nil {
		t.Fatal(err)
	}
	if got := stack.Outputs[string(StackOutputInstanceID)]; got != "i-0123456789abcdef0" {
		t.Fatalf("instance output=%q", got)
	}
	if got := stack.Outputs[string(StackOutputPublicIP)]; got != "192.0.2.10" {
		t.Fatalf("ip output=%q", got)
	}
	if _, ok := stack.Outputs["Secret"]; ok || len(stack.Outputs) != 4 {
		t.Fatalf("outputs leaked or invalid values retained: %#v", stack.Outputs)
	}
	if got := stack.Outputs[string(StackOutputStackID)]; got != "arn:aws:cloudformation:us-east-1:123456789012:stack/output-stack/01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("authoritative stack id=%q", got)
	}
}

func TestSDKProviderDescribeStackUsesResolvedAMIOnlyForTypedGeoLibreTags(t *testing.T) {
	stack := cloudformationtypes.Stack{
		StackName:   aws.String("geolibre-stack"),
		StackStatus: cloudformationtypes.StackStatusCreateComplete,
		StackId:     aws.String("arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-stack/01234567-89ab-cdef-0123-456789abcdef"),
		Parameters:  []cloudformationtypes.Parameter{{ParameterKey: aws.String("LatestAmiId"), ParameterValue: aws.String("/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"), ResolvedValue: aws.String("ami-0123456789abcdef0")}},
		Tags: []cloudformationtypes.Tag{
			{Key: aws.String("service"), Value: aws.String(EC2ServiceProfile)},
			{Key: aws.String("dirextalk:template-profile"), Value: aws.String(EC2ServiceProfile)},
			{Key: aws.String("dirextalk:template-version"), Value: aws.String(ec2TemplateVersion)},
		},
	}
	client := &safetyCloudClient{describeStack: &cloudformation.DescribeStacksOutput{Stacks: []cloudformationtypes.Stack{stack}}, templateBody: `{"Resources":{}}`}
	p, err := NewSDKProvider(SDKClients{CloudFormation: client})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.DescribeStack(context.Background(), safetyHandle(), "us-east-1", "geolibre-stack")
	if err != nil || got.Parameters["LatestAmiId"] != "ami-0123456789abcdef0" {
		t.Fatalf("typed resolved AMI=%#v err=%v", got.Parameters, err)
	}

	client.describeStack.Stacks[0].Tags[0].Value = aws.String("other")
	client.describeStack.Stacks[0].Parameters[0].ResolvedValue = aws.String("not-an-ami")
	got, err = p.DescribeStack(context.Background(), safetyHandle(), "us-east-1", "geolibre-stack")
	if err != nil || got.Parameters["LatestAmiId"] != "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64" {
		t.Fatalf("untagged stack must retain parameter value=%#v err=%v", got.Parameters, err)
	}

	client.describeStack.Stacks[0].Tags[0].Value = aws.String(EC2ServiceProfile)
	client.describeStack.Stacks[0].Parameters[0].ResolvedValue = nil
	if _, err = p.DescribeStack(context.Background(), safetyHandle(), "us-east-1", "geolibre-stack"); err != ErrProvider {
		t.Fatalf("typed stack without resolved AMI err=%v, want ErrProvider", err)
	}
}
