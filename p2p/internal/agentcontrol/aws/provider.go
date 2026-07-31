package aws

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type STSProvider interface {
	GetCallerIdentity(context.Context, CredentialHandle) (Identity, error)
}

type STSClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}
type EC2Client interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
}
type SSMClient interface {
	DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
	CancelCommand(context.Context, *ssm.CancelCommandInput, ...func(*ssm.Options)) (*ssm.CancelCommandOutput, error)
}
type SSMCommandClient interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
}

// EC2ReservationClient and PricingClient are deliberately separate from the
// execution transport clients. Reserving capacity is a read-only catalog
// operation and must not accidentally acquire any EC2 mutation method.
type EC2ReservationClient interface {
	DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
}

type PricingClient interface {
	GetProducts(context.Context, *pricing.GetProductsInput, ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

type CloudFormationProvisionClient interface {
	CreateStack(context.Context, *cloudformation.CreateStackInput, ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error)
	DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error)
	GetTemplate(context.Context, *cloudformation.GetTemplateInput, ...func(*cloudformation.Options)) (*cloudformation.GetTemplateOutput, error)
}

type ClientFactory interface {
	NewSTS(CredentialHandle) (STSClient, error)
}
type TypedClientFactory interface {
	NewSTS(CredentialHandle) (STSClient, error)
	NewEC2(CredentialHandle) (EC2Client, error)
	NewSSM(CredentialHandle) (SSMClient, error)
	NewSSMCommand(CredentialHandle) (SSMCommandClient, error)
}

// ReservationClientFactory is optional on custom factories. Production
// reservation readiness remains false unless both live EC2 catalog and AWS
// Price List Query clients can be constructed.
type ReservationClientFactory interface {
	NewEC2Reservation(Credentials) (EC2ReservationClient, error)
	NewPricing(Credentials) (PricingClient, error)
}

// CloudFormationProvisionClientFactory is the narrow production mutation
// boundary. The mutation client is configured with one SDK attempt; after a
// durable intent is written, every ambiguous outcome is readback-only.
type CloudFormationProvisionClientFactory interface {
	NewCloudFormationProvision(Credentials) (CloudFormationProvisionClient, error)
	NewEC2ProvisionReadback(Credentials) (EC2Client, error)
	NewSSMProvisionReadback(Credentials) (SSMClient, error)
}

type SDKClients struct {
	STS            STSClient
	EC2            EC2Client
	SSM            SSMClient
	SSMCommand     SSMCommandClient
	EC2Reservation EC2ReservationClient
	Pricing        PricingClient
	CloudFormation CloudFormationProvisionClient
	SSMParameter   SSMSecretParameterClient
}

func (c SDKClients) NewEC2Reservation(Credentials) (EC2ReservationClient, error) {
	if c.EC2Reservation != nil {
		return c.EC2Reservation, nil
	}
	if c.EC2 != nil {
		if client, ok := c.EC2.(EC2ReservationClient); ok {
			return client, nil
		}
	}
	return nil, ErrInvalid
}
func (c SDKClients) NewPricing(Credentials) (PricingClient, error) {
	if c.Pricing == nil {
		return nil, ErrInvalid
	}
	return c.Pricing, nil
}
func (c SDKClients) NewCloudFormationProvision(Credentials) (CloudFormationProvisionClient, error) {
	if c.CloudFormation == nil {
		return nil, ErrInvalid
	}
	return c.CloudFormation, nil
}
func (c SDKClients) NewEC2ProvisionReadback(Credentials) (EC2Client, error) {
	return c.NewEC2(CredentialHandle{})
}
func (c SDKClients) NewSSMProvisionReadback(Credentials) (SSMClient, error) {
	return c.NewSSM(CredentialHandle{})
}
func (c SDKClients) NewSSMSecretParameter(Credentials) (SSMSecretParameterClient, error) {
	if c.SSMParameter != nil {
		return c.SSMParameter, nil
	}
	if client, ok := c.SSM.(SSMSecretParameterClient); ok {
		return client, nil
	}
	return nil, ErrInvalid
}

func (c SDKClients) NewSTS(CredentialHandle) (STSClient, error) {
	if c.STS == nil {
		return nil, ErrInvalid
	}
	return c.STS, nil
}
func (c SDKClients) NewEC2(CredentialHandle) (EC2Client, error) {
	if c.EC2 == nil {
		return nil, ErrInvalid
	}
	return c.EC2, nil
}
func (c SDKClients) NewSSM(CredentialHandle) (SSMClient, error) {
	if c.SSM == nil {
		return nil, ErrInvalid
	}
	return c.SSM, nil
}
func (c SDKClients) NewSSMCommand(CredentialHandle) (SSMCommandClient, error) {
	if c.SSMCommand == nil {
		return nil, ErrInvalid
	}
	return c.SSMCommand, nil
}

// Keep a mutex in the concrete factory so it remains safe to share between
// concurrent typed execution requests.
type SDKFactory struct{ mu sync.Mutex }

func NewSDKFactory() *SDKFactory { return &SDKFactory{} }
func (f *SDKFactory) NewSTS(handle CredentialHandle) (STSClient, error) {
	cfg, err := staticAWSConfig(handle)
	if err != nil {
		return nil, err
	}
	return sts.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewEC2(handle CredentialHandle) (EC2Client, error) {
	cfg, err := staticAWSConfig(handle)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewSSM(handle CredentialHandle) (SSMClient, error) {
	cfg, err := staticAWSConfig(handle)
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewSSMCommand(handle CredentialHandle) (SSMCommandClient, error) {
	cfg, err := staticAWSConfigWithRetry(handle, 1)
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewEC2Reservation(credential Credentials) (EC2ReservationClient, error) {
	cfg, err := staticAWSConfig(credential.handle())
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewPricing(credential Credentials) (PricingClient, error) {
	cfg, err := staticAWSConfig(credential.handle())
	if err != nil {
		return nil, err
	}
	// The Price List Query API is a global commercial-partition service whose
	// signed endpoint is us-east-1. Product region selection remains an exact
	// regionCode filter in the request; it is never inferred from this endpoint.
	cfg.Region = "us-east-1"
	return pricing.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewCloudFormationProvision(credential Credentials) (CloudFormationProvisionClient, error) {
	cfg, err := staticAWSConfigWithRetry(credential.handle(), 1)
	if err != nil {
		return nil, err
	}
	return cloudformation.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewEC2ProvisionReadback(credential Credentials) (EC2Client, error) {
	cfg, err := staticAWSConfig(credential.handle())
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewSSMProvisionReadback(credential Credentials) (SSMClient, error) {
	cfg, err := staticAWSConfig(credential.handle())
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}
func (f *SDKFactory) NewSSMSecretParameter(credential Credentials) (SSMSecretParameterClient, error) {
	cfg, err := staticAWSConfigWithRetry(credential.handle(), 1)
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
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
