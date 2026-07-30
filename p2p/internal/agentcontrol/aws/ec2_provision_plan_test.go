package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validEC2ProvisionRequest() EC2ProvisionRequest {
	return EC2ProvisionRequest{
		OwnerID:                   "@owner:example.test",
		CredentialID:              "11111111-1111-4111-8111-111111111111",
		CredentialRevision:        3,
		Region:                    "us-east-1",
		StackName:                 "geolibre-prod",
		DisplayName:               "geolibre-prod",
		InstanceType:              "t3.small",
		VolumeGiB:                 20,
		PublicHTTP:                false,
		AcknowledgePublicExposure: true,
	}
}

func TestBuildEC2ProvisionPlanIsDeterministicAndCanonical(t *testing.T) {
	request := validEC2ProvisionRequest()
	one, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	two, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	onePlan, twoPlan := one.CorePlan(), two.CorePlan()
	if !bytes.Equal(one.CanonicalTemplate(), two.CanonicalTemplate()) || one.TemplateDigest() != two.TemplateDigest() || one.PlanDigest() != two.PlanDigest() || onePlan.ID != twoPlan.ID {
		t.Fatal("equal typed requests must produce equal canonical plan material")
	}
	if onePlan.Operation != OperationCreate || onePlan.CredentialRevision != request.CredentialRevision || onePlan.Capabilities[0] != "CAPABILITY_IAM" {
		t.Fatalf("unexpected core plan: %#v", onePlan)
	}
	if onePlan.Parameters["LatestAmiId"] != ec2LatestAMIParameter {
		t.Fatalf("fixed AMI parameter was not persisted: %#v", onePlan.Parameters)
	}
	if onePlan.Tags["dirextalk:template-profile"] != EC2ServiceProfile ||
		onePlan.Tags["dirextalk:template-version"] != ec2TemplateVersion ||
		onePlan.Tags["dirextalk:template-digest"] != one.TemplateDigest() ||
		onePlan.Tags["dirextalk:owner-binding"] != ec2OwnerBindingDigest(request.OwnerID) {
		t.Fatalf("plan identity metadata is not fully bound: %#v", onePlan.Tags)
	}
	if err := onePlan.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalJSONHash(one.CanonicalTemplate()); got != one.TemplateDigest() {
		t.Fatalf("canonical hash=%q template hash=%q", got, one.TemplateDigest())
	}
	for _, item := range one.CostBreakdown() {
		if !item.Informational || item.PriceStatus != "unavailable" || strings.Contains(item.Description, "$0.01") {
			t.Fatalf("unsafe cost item: %#v", item)
		}
	}
	encoded, err := json.Marshal(one)
	if err != nil || bytes.Contains(encoded, []byte("secret")) {
		t.Fatalf("unsafe metadata: %v %s", err, encoded)
	}
}

func TestBuildEC2ProvisionPlanContainsFixedSafeResources(t *testing.T) {
	request := validEC2ProvisionRequest()
	request.PublicHTTP = true
	request.AcknowledgePublicExposure = true
	request.InstanceType = "t3.medium"
	request.VolumeGiB = 200
	plan, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	corePlan := plan.CorePlan()
	template := string(corePlan.Template)
	for _, want := range []string{
		`"AWS::EC2::VPC"`, `"AWS::EC2::Subnet"`, `"AWS::EC2::InternetGateway"`, `"AWS::EC2::Route"`,
		`"AWS::EC2::SecurityGroup"`, `AmazonSSMManagedInstanceCore`, `"AWS::SSM::Parameter::Value`,
		`al2023-ami-kernel-default-x86_64`, `"gp3"`, `"InstanceId"`, `"PublicIp"`, `"SecurityGroupId"`,
		`dnf install -y docker`, `systemctl enable --now docker`,
	} {
		if !strings.Contains(template, want) {
			t.Fatalf("template missing %q", want)
		}
	}
	if strings.Contains(template, `"FromPort":22`) || strings.Contains(template, `"ToPort":22`) {
		t.Fatal("SSH ingress must never be present")
	}
	if !strings.Contains(template, `"CidrIp":"0.0.0.0/0"`) || !strings.Contains(template, `"FromPort":80`) {
		t.Fatal("acknowledged public HTTP ingress missing")
	}
	for _, want := range []string{`"Key":"owner","Value":"` + ec2OwnerBindingDigest(request.OwnerID) + `"`, `"Key":"managed","Value":"true"`, `"Key":"service","Value":"geolibre"`, `"Key":"dirextalk:plan-id","Value":"` + corePlan.ID + `"`} {
		if !strings.Contains(template, want) {
			t.Fatalf("forced tag missing %q", want)
		}
	}
	if strings.Contains(template, request.OwnerID) || corePlan.Tags["owner"] != ec2OwnerBindingDigest(request.OwnerID) {
		t.Fatal("raw owner leaked or owner digest was not bound")
	}
	if corePlan.Tags[RequiredOutputsTag] != "InstanceId+PublicIp+SecurityGroupId+StackId" {
		t.Fatalf("required outputs marker = %q", corePlan.Tags[RequiredOutputsTag])
	}
	providerTags, err := validateProviderTags(corePlan.Tags)
	if err != nil || providerTags[RequiredOutputsTag] != corePlan.Tags[RequiredOutputsTag] {
		t.Fatalf("complete EC2 tag set failed provider validation: tags=%v err=%v", providerTags, err)
	}
	if strings.Contains(template, `"SSMInstanceProfile":{"Properties":{"Roles":[{"Ref":"SSMRole"}],"Tags"`) {
		t.Fatal("AWS::IAM::InstanceProfile does not support Tags")
	}
}

func TestBuildEC2ProvisionPlanRejectsInvalidTypedInputs(t *testing.T) {
	base := validEC2ProvisionRequest()
	cases := []struct {
		name string
		edit func(*EC2ProvisionRequest)
	}{
		{"owner", func(r *EC2ProvisionRequest) { r.OwnerID = "" }},
		{"credential", func(r *EC2ProvisionRequest) { r.CredentialID = "not-a-uuid" }},
		{"revision", func(r *EC2ProvisionRequest) { r.CredentialRevision = 0 }},
		{"region", func(r *EC2ProvisionRequest) { r.Region = "us-east-1a" }},
		{"stack", func(r *EC2ProvisionRequest) { r.StackName = "-bad" }},
		{"display", func(r *EC2ProvisionRequest) { r.DisplayName = "bad/name" }},
		{"instance type", func(r *EC2ProvisionRequest) { r.InstanceType = "m5.large" }},
		{"small volume", func(r *EC2ProvisionRequest) { r.VolumeGiB = 19 }},
		{"large volume", func(r *EC2ProvisionRequest) { r.VolumeGiB = 201 }},
		{"unacknowledged exposure", func(r *EC2ProvisionRequest) { r.AcknowledgePublicExposure = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			tc.edit(&request)
			if !errors.Is(BuildErr(request), ErrInvalid) {
				t.Fatalf("request unexpectedly accepted: %#v", request)
			}
		})
	}
}

func TestBuildEC2ProvisionPlanBindsAuthenticatedOwner(t *testing.T) {
	one := validEC2ProvisionRequest()
	two := one
	two.OwnerID = "@different:example.test"
	onePlan, err := BuildEC2ProvisionPlan(one)
	if err != nil {
		t.Fatal(err)
	}
	twoPlan, err := BuildEC2ProvisionPlan(two)
	if err != nil {
		t.Fatal(err)
	}
	if onePlan.CorePlan().ID == twoPlan.CorePlan().ID || onePlan.PlanDigest() == twoPlan.PlanDigest() {
		t.Fatal("different authenticated owners must not share a plan identity")
	}
	if onePlan.CorePlan().Tags["owner"] == twoPlan.CorePlan().Tags["owner"] {
		t.Fatal("different authenticated owners must not share an owner binding")
	}
}

func TestEC2ProvisionPlanIsSealedAndPinsCredentialRevision(t *testing.T) {
	built, err := BuildEC2ProvisionPlan(validEC2ProvisionRequest())
	if err != nil {
		t.Fatal(err)
	}
	exposed := built.CorePlan()
	exposed.Template = []byte(`{"Resources":{"Injected":{"Type":"AWS::S3::Bucket"}}}`)
	exposed.Parameters["DisplayName"] = "mutated"
	exposed.Tags[RequiredOutputsTag] = ""
	exposed.Capabilities[0] = "CAPABILITY_AUTO_EXPAND"
	input := built.PlanInput(uuid.NewString())
	if bytes.Equal(input.Template, exposed.Template) || input.Parameters["DisplayName"] == "mutated" ||
		input.Tags[RequiredOutputsTag] == "" || input.Capabilities[0] != "CAPABILITY_IAM" {
		t.Fatalf("sealed plan was mutated through response projection: %#v", input)
	}
	if input.ExpectedCredentialRevision != validEC2ProvisionRequest().CredentialRevision {
		t.Fatalf("expected credential revision = %d", input.ExpectedCredentialRevision)
	}
}

func TestEC2ProvisionPlanRejectsCredentialRotationAndUsesUnavailableQuote(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	service := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	created, err := saveVerifiedCredential(t, service, repo, CredentialInput{
		Name: "aws", Region: "us-east-1", AccessKeyID: "access", SecretAccessKey: "secret",
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := validEC2ProvisionRequest()
	request.CredentialID = created.ID
	request.CredentialRevision = created.Revision
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReplaceCredential(ctx, CredentialInput{
		ID: created.ID, Name: "aws-rotated", Region: "us-east-1",
		AccessKeyID: "access-2", SecretAccessKey: "secret-2",
	}, created.Revision, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreatePlan(ctx, built.PlanInput(uuid.NewString())); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("rotated credential error = %v, want ErrRevisionConflict", err)
	}

	request.CredentialRevision++
	if _, err = repo.RecordCredentialIdentity(ctx, created.ID, request.CredentialRevision, Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.CreatePlan(ctx, fresh.PlanInput(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.Quote(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if quote.PriceStatus != "unavailable" || quote.EstimatedMonthlyUSD != 0 || strings.Contains(quote.Summary, "$0.01") {
		t.Fatalf("typed quote is misleading: %#v", quote)
	}
	if quote.ResourceCount != 11 {
		t.Fatalf("typed quote resource count = %d, want 11", quote.ResourceCount)
	}
}

func TestEC2ProvisionPlanReconcilesDefaultedAMIParameter(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	provider := NewFakeProvider()
	service := NewService(repo, &testConfirm{}, testTasks{}, nil, provider, time.Now)
	credential, err := saveVerifiedCredential(t, service, repo, CredentialInput{
		Name: "aws", Region: "us-east-1", AccessKeyID: "access", SecretAccessKey: "secret",
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := validEC2ProvisionRequest()
	request.CredentialID = credential.ID
	request.CredentialRevision = credential.Revision
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.CreatePlan(ctx, built.PlanInput(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := service.RequestChange(ctx, RequestChangeInput{PlanID: view.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, service, repo, requested)
	plan, err := repo.GetPlan(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	change, err := repo.GetChange(ctx, requested.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider.Stacks[plan.Region+"/"+plan.StackName] = Stack{
		Region: plan.Region, StackName: plan.StackName, Status: "CREATE_COMPLETE",
		TemplateSHA256: plan.TemplateSHA256,
		Parameters:     cloneMap(plan.Parameters),
		Tags:           cloneMap(plan.Tags),
		Outputs: StackOutputs{
			string(StackOutputInstanceID):    "i-0123456789abcdef0",
			string(StackOutputPublicIP):      "192.0.2.10",
			string(StackOutputSecurityGroup): "sg-0123456789abcdef0",
			string(StackOutputStackID):       "arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-prod/01234567-89ab-cdef-0123-456789abcdef",
		},
	}
	provider.Stacks[plan.Region+"/"+plan.StackName].Parameters["LatestAmiId"] = "ami-0123456789abcdef0"
	done, err := service.reconcileChange(ctx, change, plan)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("defaulted AMI parameter did not reconcile: done=%#v err=%v", done, err)
	}
}

func TestQuoteCountsOnlyTopLevelCloudFormationResources(t *testing.T) {
	quote := quoteFor(Plan{
		ID:        uuid.NewString(),
		Operation: OperationCreate,
		Region:    "us-east-1",
		StackName: "quote-test",
		Template: []byte(`{
			"Parameters":{"Name":{"Type":"String"}},
			"Resources":{"OnlyResource":{"Type":"AWS::SQS::Queue"}}
		}`),
	})
	if quote.ResourceCount != 1 {
		t.Fatalf("resource count = %d, want 1", quote.ResourceCount)
	}
	unknown := quoteFor(Plan{
		ID:        uuid.NewString(),
		Operation: OperationCreate,
		Region:    "us-east-1",
		StackName: "quote-unknown",
		Template:  []byte("not-json"),
	})
	if unknown.ResourceCount != 0 || unknown.PriceStatus != "unavailable" || strings.Contains(unknown.Summary, "$") {
		t.Fatalf("unknown quote was not fail closed: %#v", unknown)
	}
}

func TestPlanParametersMatchReadbackResolvesFixedSSMAMI(t *testing.T) {
	request := validEC2ProvisionRequest()
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	plan := built.CorePlan()
	observed := cloneMap(plan.Parameters)
	observed["LatestAmiId"] = "ami-0123456789abcdef0"
	if !PlanParametersMatchReadback(plan, observed) {
		t.Fatalf("resolved AMI readback should match: %#v", observed)
	}
	for name, mutate := range map[string]func(map[string]string){
		"wrong ami":             func(v map[string]string) { v["LatestAmiId"] = "ami-not-hex" },
		"wrong fixed parameter": func(v map[string]string) { v["DisplayName"] = "other" },
		"unknown parameter":     func(v map[string]string) { v["Unexpected"] = "value" },
		"missing parameter":     func(v map[string]string) { delete(v, "InstanceType") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneMap(observed)
			mutate(candidate)
			if PlanParametersMatchReadback(plan, candidate) {
				t.Fatalf("malformed readback accepted: %#v", candidate)
			}
		})
	}
	generic := Plan{Parameters: map[string]string{"LatestAmiId": ec2LatestAMIParameter}}
	if PlanParametersMatchReadback(generic, map[string]string{"LatestAmiId": "ami-0123456789abcdef0"}) || !PlanParametersMatchReadback(generic, generic.Parameters) {
		t.Fatal("AMI resolution must remain profile-scoped")
	}
}

func BuildErr(request EC2ProvisionRequest) error {
	_, err := BuildEC2ProvisionPlan(request)
	return err
}
