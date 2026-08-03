package execution

import "testing"

func apiKeyAIPlan() ExecutionPlan {
	p := plan()
	ref := CredentialRef{Ref: "44444444-4444-4444-8444-444444444444", Purpose: AISecretPurposeProviderAPIKey, Revision: 1, BindingDigest: sha}
	p.AIConfiguration = &AIConfiguration{Mode: AIAuthModeAPIKey, Provider: "openai", SecretRef: ref.Ref, SecretRevision: ref.Revision, SecretPurpose: ref.Purpose, SecretBindingDigest: ref.BindingDigest}
	p.Stages[0].DependsOn = []string{"authorize-ai"}
	p.Stages[0].Effects = append(p.Stages[0].Effects, GateSecretAccess)
	p.Stages[0].Steps[0].SecretRefs = []CredentialRef{ref}
	p.Stages[0].Steps[0].ScriptRun.SecretRefs = []CredentialRef{ref}
	p.Stages = append([]ExecutionStage{{
		StageKey: "authorize-ai", Revision: 1, Kind: string(StepSecretProvision), Risk: RiskR2, Gate: GateSecretAccess,
		Effects: []Gate{GateSecretAccess}, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest,
		Steps: []ExecutionStep{{StepKey: "provision-ai-key", Kind: StepSecretProvision, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, SecretRefs: []CredentialRef{ref}, TimeoutSeconds: 30, IdempotencyMarker: "provision-ai-key", OutputPolicy: OutputDiscard, SecretProvision: &SecretProvisionStep{Delivery: "target_secure_parameter"}}}, TimeoutSeconds: 60,
	}}, p.Stages...)
	return p
}

func authGateAIPlan() ExecutionPlan {
	p := plan()
	p.AIConfiguration = &AIConfiguration{Mode: AIAuthModeAuthGate, Provider: "anthropic", Status: AIExternalAuthPending}
	p.Stages[0].DependsOn = []string{"authorize-ai"}
	p.Stages = append([]ExecutionStage{{
		StageKey: "authorize-ai", Revision: 1, Kind: string(StepExternalAuth), Risk: RiskR2, Gate: GateExternalAuth,
		Effects: []Gate{GateExternalAuth}, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest,
		Steps: []ExecutionStep{{StepKey: "external-ai-auth", Kind: StepExternalAuth, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, TimeoutSeconds: 30, IdempotencyMarker: "external-ai-auth", OutputPolicy: OutputDiscard, ExternalAuth: &ExternalAuthStep{Provider: "anthropic", Status: AIExternalAuthPending}}}, TimeoutSeconds: 60,
	}}, p.Stages...)
	return p
}

func TestAIConfigurationRequiresDistinctAuthorizationStage(t *testing.T) {
	apiPlan := apiKeyAIPlan()
	if _, err := apiPlan.Normalize(); err != nil {
		t.Fatalf("api key plan rejected: %v", err)
	}
	authPlan := authGateAIPlan()
	if _, err := authPlan.Normalize(); err != nil {
		t.Fatalf("auth gate plan rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ExecutionPlan){
		"api key without authorization dependency": func(p *ExecutionPlan) { p.Stages[1].DependsOn = nil },
		"api key ref drift":                        func(p *ExecutionPlan) { p.Stages[1].Steps[0].SecretRefs[0].Revision++ },
		"inline secret stage under remote gate":    func(p *ExecutionPlan) { p.Stages[0].Gate = GateRemoteExecution },
	} {
		t.Run(name, func(t *testing.T) {
			p := apiKeyAIPlan()
			mutate(&p)
			if _, err := p.Normalize(); err == nil {
				t.Fatal("unsafe api key topology accepted")
			}
		})
	}

	for name, mutate := range map[string]func(*ExecutionPlan){
		"auth gate without dependency": func(p *ExecutionPlan) { p.Stages[1].DependsOn = nil },
		"auth gate with secret": func(p *ExecutionPlan) {
			p.Stages[0].Steps[0].SecretRefs = []CredentialRef{{Ref: "44444444-4444-4444-8444-444444444444", Purpose: AISecretPurposeProviderAPIKey, Revision: 1, BindingDigest: sha}}
		},
		"auth status drift": func(p *ExecutionPlan) { p.Stages[0].Steps[0].ExternalAuth.Status = "complete" },
	} {
		t.Run(name, func(t *testing.T) {
			p := authGateAIPlan()
			mutate(&p)
			if _, err := p.Normalize(); err == nil {
				t.Fatal("unsafe auth gate topology accepted")
			}
		})
	}
}

func TestAIControlStepsRequireConfigurationAndNeverCarryInlineValue(t *testing.T) {
	p := apiKeyAIPlan()
	p.AIConfiguration = nil
	if _, err := p.Normalize(); err == nil {
		t.Fatal("secret.provision accepted without AI configuration")
	}
	p = authGateAIPlan()
	p.AIConfiguration = nil
	if _, err := p.Normalize(); err == nil {
		t.Fatal("auth.external accepted without AI configuration")
	}
}
