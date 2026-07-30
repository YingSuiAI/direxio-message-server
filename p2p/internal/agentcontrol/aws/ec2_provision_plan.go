package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// EC2ServiceProfile is the only service profile currently supported by the
// typed EC2 provisioner.  It is intentionally not a request-controlled value.
const (
	EC2ServiceProfile     = "geolibre"
	ec2TemplateVersion    = "ec2-geolibre-v1"
	ec2LatestAMIParameter = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
)

// EC2ProvisionRequest is the complete public input to the typed provision
// builder.  In particular, it has no template, image, network, role, shell, or
// arbitrary provider-payload fields.
type EC2ProvisionRequest struct {
	// OwnerID is supplied from the authenticated service context. It is bound
	// into the plan identity and resource tags, but the raw identifier is never
	// sent to AWS.
	OwnerID                   string
	CredentialID              string
	CredentialRevision        int64
	Region                    string
	StackName                 string
	DisplayName               string
	InstanceType              string
	VolumeGiB                 int64
	PublicHTTP                bool
	AcknowledgePublicExposure bool
}

// EC2ProvisionPlan is the immutable, canonical result of BuildEC2ProvisionPlan.
// Template and Plan are copied when exposed through the helper methods; callers
// should persist the Plan as-is and never alter its template, parameters, tags,
// or capabilities.
type EC2ProvisionPlan struct {
	plan           Plan
	canonicalJSON  []byte
	templateSHA256 string
	digest         string
	summary        string
	costItems      []EC2ProvisionCostItem
}

// EC2ProvisionCostItem deliberately carries no made-up price.  Prices depend
// on account, region, usage, and AWS pricing updates; this domain only reports
// informational resource categories.
type EC2ProvisionCostItem struct {
	Resource      string `json:"resource"`
	Description   string `json:"description"`
	PriceStatus   string `json:"price_status"`
	Informational bool   `json:"informational"`
}

// BuildEC2ProvisionPlan creates a deterministic CloudFormation template and a
// coreaws Plan for an EC2 create operation.
func BuildEC2ProvisionPlan(req EC2ProvisionRequest) (EC2ProvisionPlan, error) {
	if err := validateEC2ProvisionRequest(req); err != nil {
		return EC2ProvisionPlan{}, err
	}

	ownerDigest := ec2OwnerBindingDigest(req.OwnerID)
	requestDigest := canonicalDigest(struct {
		TemplateVersion, OwnerID, CredentialID, Region, StackName, DisplayName, InstanceType string
		CredentialRevision, VolumeGiB                                                        int64
		PublicHTTP, AcknowledgePublicExposure                                                bool
	}{
		TemplateVersion:           ec2TemplateVersion,
		OwnerID:                   strings.TrimSpace(req.OwnerID),
		CredentialID:              req.CredentialID,
		Region:                    req.Region,
		StackName:                 req.StackName,
		DisplayName:               req.DisplayName,
		InstanceType:              req.InstanceType,
		CredentialRevision:        req.CredentialRevision,
		VolumeGiB:                 req.VolumeGiB,
		PublicHTTP:                req.PublicHTTP,
		AcknowledgePublicExposure: req.AcknowledgePublicExposure,
	})
	// The ID is derived from the authenticated, canonical request so equal
	// requests produce equal plans without introducing a random digest input.
	planID := uuid.NewSHA1(uuid.Nil, []byte("ec2-provision:"+requestDigest)).String()

	// CloudFormation's JSON encoder sorts map keys, and NormalizeTemplate then
	// performs the repository's canonical JSON normalization once more.
	templateValue := ec2Template(req, ownerDigest, planID)
	raw, err := json.Marshal(templateValue)
	if err != nil {
		return EC2ProvisionPlan{}, ErrInvalid
	}
	normalized, templateDigest, err := NormalizeTemplate(raw)
	if err != nil {
		return EC2ProvisionPlan{}, err
	}

	parameters := map[string]string{
		"DisplayName":  req.DisplayName,
		"InstanceType": req.InstanceType,
		"VolumeSize":   strconv.FormatInt(req.VolumeGiB, 10),
		"PublicHTTP":   strconv.FormatBool(req.PublicHTTP),
		"LatestAmiId":  ec2LatestAMIParameter,
	}
	tags := map[string]string{
		"owner":                      ownerDigest,
		"managed":                    "true",
		"service":                    EC2ServiceProfile,
		"dirextalk:plan-id":          planID,
		"dirextalk:request-digest":   requestDigest,
		RequiredOutputsTag:           strings.Join([]string{string(StackOutputInstanceID), string(StackOutputPublicIP), string(StackOutputSecurityGroup), string(StackOutputStackID)}, ","),
		"dirextalk:price-status":     "unavailable",
		"dirextalk:template-version": ec2TemplateVersion,
	}
	plan := Plan{
		ID:                 planID,
		CredentialID:       req.CredentialID,
		CredentialRevision: req.CredentialRevision,
		Region:             req.Region,
		StackName:          req.StackName,
		Operation:          OperationCreate,
		Template:           append([]byte(nil), normalized...),
		TemplateSHA256:     templateDigest,
		Parameters:         cloneMap(parameters),
		Tags:               cloneMap(tags),
		Capabilities:       []string{"CAPABILITY_IAM"},
		Revision:           1,
	}
	if err := plan.Validate(); err != nil {
		return EC2ProvisionPlan{}, err
	}

	canonical := append([]byte(nil), normalized...)
	digest := planDigest(plan)
	summary := fmt.Sprintf("provision %s EC2 in %s (%s; %d GiB root; public_http=%t)", EC2ServiceProfile, req.Region, req.InstanceType, req.VolumeGiB, req.PublicHTTP)
	costItems := []EC2ProvisionCostItem{
		{Resource: "ec2", Description: "EC2 instance and gp3 root volume", PriceStatus: "unavailable", Informational: true},
		{Resource: "network", Description: "dedicated VPC, subnet, internet gateway, route, and security group", PriceStatus: "unavailable", Informational: true},
	}
	return EC2ProvisionPlan{plan: cloneEC2CorePlan(plan), canonicalJSON: canonical, templateSHA256: templateDigest, digest: digest, summary: summary, costItems: append([]EC2ProvisionCostItem(nil), costItems...)}, nil
}

// BuildEC2Plan is a concise compatibility spelling for callers that do not
// need to distinguish this builder from other typed plan builders.
func BuildEC2Plan(req EC2ProvisionRequest) (EC2ProvisionPlan, error) {
	return BuildEC2ProvisionPlan(req)
}

// ValidateEC2ProvisionRequest exposes the same validation used by the builder.
func ValidateEC2ProvisionRequest(req EC2ProvisionRequest) error {
	return validateEC2ProvisionRequest(req)
}

var ec2DisplayNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$`)

func validateEC2ProvisionRequest(req EC2ProvisionRequest) error {
	ownerID := strings.TrimSpace(req.OwnerID)
	if ownerID == "" || len(ownerID) > 512 {
		return ErrInvalid
	}
	if !validUUID(strings.TrimSpace(req.CredentialID)) || req.CredentialRevision < 1 || !validRegion(req.Region) || !validStackName(req.StackName) {
		return ErrInvalid
	}
	if !ec2DisplayNameRE.MatchString(req.DisplayName) {
		return ErrInvalid
	}
	if req.InstanceType != "t3.small" && req.InstanceType != "t3.medium" {
		return ErrInvalid
	}
	if req.VolumeGiB < 20 || req.VolumeGiB > 200 {
		return ErrInvalid
	}
	// The fixed topology always assigns a public IPv4 so SSM bootstrap and
	// result readback are deterministic. Port 80 remains separately gated by
	// PublicHTTP, but the public address and its AWS charge always require an
	// explicit owner acknowledgement.
	if !req.AcknowledgePublicExposure {
		return ErrInvalid
	}
	return nil
}

func ec2Template(req EC2ProvisionRequest, ownerDigest, planID string) map[string]any {
	publicIngress := []any{}
	if req.PublicHTTP {
		publicIngress = append(publicIngress, map[string]any{"IpProtocol": "tcp", "FromPort": 80, "ToPort": 80, "CidrIp": "0.0.0.0/0"})
	}
	return map[string]any{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Description":              "Dirextalk geolibre managed EC2 service",
		"Parameters": map[string]any{
			"DisplayName":  map[string]any{"Type": "String", "Default": req.DisplayName, "AllowedPattern": "^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$"},
			"InstanceType": map[string]any{"Type": "String", "Default": req.InstanceType, "AllowedValues": []string{"t3.small", "t3.medium"}},
			"VolumeSize":   map[string]any{"Type": "Number", "Default": req.VolumeGiB, "MinValue": 20, "MaxValue": 200},
			"PublicHTTP":   map[string]any{"Type": "String", "Default": strconv.FormatBool(req.PublicHTTP), "AllowedValues": []string{"true", "false"}},
			"LatestAmiId":  map[string]any{"Type": "AWS::SSM::Parameter::Value<AWS::EC2::Image::Id>", "Default": ec2LatestAMIParameter},
		},
		"Resources": map[string]any{
			"VPC":                         map[string]any{"Type": "AWS::EC2::VPC", "Properties": map[string]any{"CidrBlock": "10.0.0.0/16", "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"InternetGateway":             map[string]any{"Type": "AWS::EC2::InternetGateway", "Properties": map[string]any{"Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"VPCGatewayAttachment":        map[string]any{"Type": "AWS::EC2::VPCGatewayAttachment", "Properties": map[string]any{"InternetGatewayId": map[string]any{"Ref": "InternetGateway"}, "VpcId": map[string]any{"Ref": "VPC"}}},
			"PublicSubnet":                map[string]any{"Type": "AWS::EC2::Subnet", "Properties": map[string]any{"VpcId": map[string]any{"Ref": "VPC"}, "CidrBlock": "10.0.1.0/24", "MapPublicIpOnLaunch": true, "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"RouteTable":                  map[string]any{"Type": "AWS::EC2::RouteTable", "Properties": map[string]any{"VpcId": map[string]any{"Ref": "VPC"}, "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"DefaultRoute":                map[string]any{"Type": "AWS::EC2::Route", "DependsOn": "VPCGatewayAttachment", "Properties": map[string]any{"RouteTableId": map[string]any{"Ref": "RouteTable"}, "DestinationCidrBlock": "0.0.0.0/0", "GatewayId": map[string]any{"Ref": "InternetGateway"}}},
			"SubnetRouteTableAssociation": map[string]any{"Type": "AWS::EC2::SubnetRouteTableAssociation", "Properties": map[string]any{"RouteTableId": map[string]any{"Ref": "RouteTable"}, "SubnetId": map[string]any{"Ref": "PublicSubnet"}}},
			"SecurityGroup":               map[string]any{"Type": "AWS::EC2::SecurityGroup", "Properties": map[string]any{"GroupDescription": "Dirextalk geolibre web service (no SSH)", "VpcId": map[string]any{"Ref": "VPC"}, "SecurityGroupIngress": publicIngress, "SecurityGroupEgress": []any{map[string]any{"IpProtocol": "-1", "CidrIp": "0.0.0.0/0"}}, "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"SSMRole":                     map[string]any{"Type": "AWS::IAM::Role", "Properties": map[string]any{"AssumeRolePolicyDocument": map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{"Effect": "Allow", "Principal": map[string]any{"Service": "ec2.amazonaws.com"}, "Action": "sts:AssumeRole"}}}, "ManagedPolicyArns": []string{"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}, "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
			"SSMInstanceProfile":          map[string]any{"Type": "AWS::IAM::InstanceProfile", "Properties": map[string]any{"Roles": []any{map[string]any{"Ref": "SSMRole"}}}},
			"Instance":                    map[string]any{"Type": "AWS::EC2::Instance", "Properties": map[string]any{"ImageId": map[string]any{"Ref": "LatestAmiId"}, "InstanceType": map[string]any{"Ref": "InstanceType"}, "SubnetId": map[string]any{"Ref": "PublicSubnet"}, "SecurityGroupIds": []any{map[string]any{"Ref": "SecurityGroup"}}, "IamInstanceProfile": map[string]any{"Ref": "SSMInstanceProfile"}, "BlockDeviceMappings": []any{map[string]any{"DeviceName": "/dev/xvda", "Ebs": map[string]any{"VolumeType": "gp3", "VolumeSize": map[string]any{"Ref": "VolumeSize"}, "Encrypted": true, "DeleteOnTermination": true}}}, "UserData": map[string]any{"Fn::Base64": "#!/bin/bash\nset -euo pipefail\ndnf install -y docker\nsystemctl enable --now docker\n"}, "Tags": forcedTags(req.DisplayName, ownerDigest, planID)}},
		},
		"Outputs": map[string]any{
			"InstanceId":      map[string]any{"Value": map[string]any{"Ref": "Instance"}},
			"PublicIp":        map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Instance", "PublicIp"}}},
			"SecurityGroupId": map[string]any{"Value": map[string]any{"Ref": "SecurityGroup"}},
		},
	}
}

func forcedTags(displayName, ownerDigest, planID string) []any {
	return []any{
		map[string]any{"Key": "owner", "Value": ownerDigest},
		map[string]any{"Key": "managed", "Value": "true"},
		map[string]any{"Key": "service", "Value": EC2ServiceProfile},
		map[string]any{"Key": "dirextalk:plan-id", "Value": planID},
		map[string]any{"Key": "Name", "Value": displayName},
	}
}

func ec2OwnerBindingDigest(ownerID string) string {
	return OwnerBindingDigest(ownerID)
}

// CanonicalJSONHash returns the SHA-256 hash of canonical JSON after the same
// normalization used by Plan. It is useful to callers storing audit metadata.
func CanonicalJSONHash(canonicalJSON []byte) string {
	normalized, _, err := NormalizeTemplate(canonicalJSON)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

// PlanInput returns the existing core PlanInput shape for service persistence.
// The service will pin the credential revision from its repository; callers
// must ensure it matches the request's revision before persisting.
func (p EC2ProvisionPlan) PlanInput(idempotencyKey string) PlanInput {
	sealed := cloneEC2CorePlan(p.plan)
	return PlanInput{ID: sealed.ID, CredentialID: sealed.CredentialID, ExpectedCredentialRevision: sealed.CredentialRevision, Region: sealed.Region, StackName: sealed.StackName, Operation: sealed.Operation, Template: append([]byte(nil), sealed.Template...), Parameters: cloneMap(sealed.Parameters), Tags: cloneMap(sealed.Tags), Capabilities: append([]string(nil), sealed.Capabilities...), IdempotencyKey: idempotencyKey}
}

// CorePlan returns an isolated copy for safe response projection. Mutating the
// returned value cannot alter the sealed material used by PlanInput.
func (p EC2ProvisionPlan) CorePlan() Plan { return cloneEC2CorePlan(p.plan) }
func (p EC2ProvisionPlan) CanonicalTemplate() []byte {
	return append([]byte(nil), p.canonicalJSON...)
}
func (p EC2ProvisionPlan) TemplateDigest() string { return p.templateSHA256 }
func (p EC2ProvisionPlan) PlanDigest() string     { return p.digest }
func (p EC2ProvisionPlan) SummaryText() string    { return p.summary }
func (p EC2ProvisionPlan) CostBreakdown() []EC2ProvisionCostItem {
	return append([]EC2ProvisionCostItem(nil), p.costItems...)
}

func cloneEC2CorePlan(plan Plan) Plan {
	plan.Template = append([]byte(nil), plan.Template...)
	plan.Parameters = cloneMap(plan.Parameters)
	plan.Tags = cloneMap(plan.Tags)
	plan.Capabilities = append([]string(nil), plan.Capabilities...)
	return plan
}

// MarshalJSON keeps the result metadata safe and deterministic. Plan itself
// already has no secret fields; CanonicalJSON is copied to prevent accidental
// mutation of the builder's internal value before marshaling.
func (p EC2ProvisionPlan) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Plan           Plan                   `json:"plan"`
		CanonicalJSON  []byte                 `json:"canonical_json"`
		TemplateSHA256 string                 `json:"template_sha256"`
		Digest         string                 `json:"digest"`
		Summary        string                 `json:"summary"`
		CostItems      []EC2ProvisionCostItem `json:"cost_items"`
	}{p.CorePlan(), p.CanonicalTemplate(), p.TemplateDigest(), p.PlanDigest(), p.SummaryText(), p.CostBreakdown()})
}
