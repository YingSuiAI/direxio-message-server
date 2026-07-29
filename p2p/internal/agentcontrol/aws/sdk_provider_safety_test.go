package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cloudformationtypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

type safetyCloudClient struct {
	createDescription         string
	executeToken, deleteToken string
	executeErr                error
	describeStatuses          []cloudformationtypes.ChangeSetStatus
	describeExecutions        []cloudformationtypes.ExecutionStatus
	describeCalls             int
}

func (c *safetyCloudClient) CreateChangeSet(_ context.Context, in *cloudformation.CreateChangeSetInput, _ ...func(*cloudformation.Options)) (*cloudformation.CreateChangeSetOutput, error) {
	c.createDescription = aws.ToString(in.Description)
	return &cloudformation.CreateChangeSetOutput{Id: aws.String("cs-safety")}, nil
}
func (c *safetyCloudClient) DescribeChangeSet(_ context.Context, _ *cloudformation.DescribeChangeSetInput, _ ...func(*cloudformation.Options)) (*cloudformation.DescribeChangeSetOutput, error) {
	c.describeCalls++
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
	return nil, ErrNotFound
}
func (c *safetyCloudClient) GetTemplate(context.Context, *cloudformation.GetTemplateInput, ...func(*cloudformation.Options)) (*cloudformation.GetTemplateOutput, error) {
	return nil, ErrNotFound
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
	req := ChangeSetRequest{Region: "us-east-1", StackName: "safety-stack", ChangeSetName: token, ClientToken: token, Operation: OperationCreate, Template: template, Parameters: map[string]string{}, Tags: map[string]string{}}
	if _, err = p.CreateChangeSet(context.Background(), safetyHandle(), req); err != nil {
		t.Fatal(err)
	}
	if client.createDescription == "" || len(client.createDescription) > 1024 || strings.Contains(client.createDescription, "secret") || strings.Contains(client.createDescription, "AKIA") {
		t.Fatalf("unsafe description %q", client.createDescription)
	}
	if _, _, ok := parseChangeSetDescription(client.createDescription); !ok {
		t.Fatalf("description is not token/digest binding: %q", client.createDescription)
	}
	// A new provider has no known map and must reconstruct the durable binding from AWS data.
	fresh, _ := NewSDKProvider(SDKClients{CloudFormation: client})
	got, err := fresh.DescribeChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", token)
	if err != nil || got.ClientToken != token || got.RequestDigest != ProviderRequestDigest(Plan{Region: req.Region, StackName: req.StackName, Operation: req.Operation, Template: template, Parameters: req.Parameters, Tags: req.Tags}, token) || digest == "" {
		t.Fatalf("fresh recovery=%+v err=%v", got, err)
	}
	if err = p.ExecuteChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", "cs-safety", token); err != nil || client.executeToken != token {
		t.Fatalf("execute token=%q err=%v", client.executeToken, err)
	}
	if err = p.DeleteStack(context.Background(), safetyHandle(), "us-east-1", "safety-stack", token); err != nil || client.deleteToken != token {
		t.Fatalf("delete token=%q err=%v", client.deleteToken, err)
	}
	client.executeErr = context.Canceled
	if err = p.ExecuteChangeSet(context.Background(), safetyHandle(), "us-east-1", "safety-stack", "cs-safety", token); err != ErrResponseUncertain {
		t.Fatalf("cancelled mutation err=%v", err)
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
		Region: "us-east-1", StackName: "pending-stack", ChangeSetName: "11111111-1111-4111-8111-111111111113", ClientToken: "11111111-1111-4111-8111-111111111113", Operation: OperationCreate, Template: template,
	})
	if err != nil {
		t.Fatalf("waited change set returned err=%v", err)
	}
	if client.describeCalls != 2 {
		t.Fatalf("describe calls=%d, want 2", client.describeCalls)
	}
}
