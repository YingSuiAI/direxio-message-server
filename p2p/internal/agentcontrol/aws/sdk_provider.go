package aws

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

type SDKProvider struct {
	factory ClientFactory
	timeout time.Duration
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
	p := &SDKProvider{factory: factory, timeout: 30 * time.Second}
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
		return Identity{}, mapSDKError(err)
	}
	if out == nil || out.Account == nil || out.Arn == nil || out.UserId == nil {
		return Identity{}, ErrProvider
	}
	parsed, err := arn.Parse(aws.ToString(out.Arn))
	if err != nil || (parsed.Service != "iam" && parsed.Service != "sts") || !accountIDValid(aws.ToString(out.Account)) || parsed.AccountID != aws.ToString(out.Account) || aws.ToString(out.UserId) == "" || (handle.AccountID != "" && handle.AccountID != aws.ToString(out.Account)) || (handle.UserARN != "" && handle.UserARN != parsed.String()) {
		return Identity{}, ErrProvider
	}
	return Identity{AccountID: parsed.AccountID, UserARN: parsed.String(), PrincipalID: aws.ToString(out.UserId)}, nil
}

func staticAWSConfig(handle CredentialHandle) (aws.Config, error) {
	return staticAWSConfigWithRetry(handle, 3)
}
func staticAWSConfigWithRetry(handle CredentialHandle, maxAttempts int) (aws.Config, error) {
	if maxAttempts < 1 || handle.credential == nil || !validRegion(handle.Region) || strings.TrimSpace(handle.credential.accessKeyID) == "" || strings.TrimSpace(handle.credential.secretAccessKey) == "" {
		return aws.Config{}, ErrInvalid
	}
	provider := credentials.NewStaticCredentialsProvider(handle.credential.accessKeyID, handle.credential.secretAccessKey, handle.credential.sessionToken)
	return aws.Config{Region: handle.Region, Credentials: aws.NewCredentialsCache(provider), RetryMode: aws.RetryModeStandard, RetryMaxAttempts: maxAttempts}, nil
}
func StaticAWSConfig(handle CredentialHandle) (aws.Config, error) { return staticAWSConfig(handle) }
func StaticAWSCommandConfig(handle CredentialHandle) (aws.Config, error) {
	return staticAWSConfigWithRetry(handle, 1)
}

func mapSDKError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "InvalidClientTokenId", "UnrecognizedClientException":
			return ErrProvider
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return ErrProvider
	}
	return ErrProvider
}

func accountIDValid(v string) bool {
	if len(v) != 12 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
