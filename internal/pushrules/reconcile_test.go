package pushrules

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
)

func TestReconcileDefaultNotificationSoundRules(t *testing.T) {
	ruleSets := legacySilentRuleSet(t)
	beforeOrder := ruleIDs(ruleSets.Global.Underride)

	if !ReconcileDefaultNotificationSoundRules(ruleSets) {
		t.Fatal("expected historical generic message rules to be reconciled")
	}
	if got := ruleIDs(ruleSets.Global.Underride); !reflect.DeepEqual(got, beforeOrder) {
		t.Fatalf("rule ordering changed: got %v want %v", got, beforeOrder)
	}
	for _, ruleID := range []string{MRuleMessage, MRuleEncrypted} {
		rule := ruleByID(ruleSets.Global.Underride, ruleID)
		want := []*Action{
			{Kind: NotifyAction},
			{Kind: SetTweakAction, Tweak: SoundTweak, Value: "default"},
		}
		if !reflect.DeepEqual(rule.Actions, want) {
			t.Fatalf("%s actions = %#v, want %#v", ruleID, rule.Actions, want)
		}
	}

	firstJSON, err := json.Marshal(ruleSets)
	if err != nil {
		t.Fatal(err)
	}
	if ReconcileDefaultNotificationSoundRules(ruleSets) {
		t.Fatal("expected repeated reconciliation to be a no-op")
	}
	secondJSON, err := json.Marshal(ruleSets)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondJSON) != string(firstJSON) {
		t.Fatal("repeated reconciliation changed the rule set")
	}
}

func TestReconcileDefaultNotificationSoundRulesPreservesCustomizedRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Rule)
	}{
		{
			name: "disabled",
			mutate: func(rule *Rule) {
				rule.Enabled = false
			},
		},
		{
			name: "custom actions",
			mutate: func(rule *Rule) {
				rule.Actions = []*Action{
					{Kind: NotifyAction},
					{Kind: SetTweakAction, Tweak: HighlightTweak, Value: true},
				}
			},
		},
		{
			name: "custom conditions",
			mutate: func(rule *Rule) {
				rule.Conditions = append(rule.Conditions, &Condition{Kind: RoomMemberCountCondition, Is: ">2"})
			},
		},
		{
			name: "non-default",
			mutate: func(rule *Rule) {
				rule.Default = false
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ruleSets := legacySilentRuleSet(t)
			messageRule := ruleByID(ruleSets.Global.Underride, MRuleMessage)
			tc.mutate(messageRule)
			before, err := json.Marshal(messageRule)
			if err != nil {
				t.Fatal(err)
			}

			if !ReconcileDefaultNotificationSoundRules(ruleSets) {
				t.Fatal("expected the unchanged encrypted rule to be reconciled")
			}
			after, err := json.Marshal(messageRule)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("customized message rule changed:\n before: %s\n after:  %s", before, after)
			}
		})
	}
}

func legacySilentRuleSet(t *testing.T) *AccountRuleSets {
	t.Helper()
	ruleSets := DefaultAccountRuleSets("alice", spec.ServerName("example.com"))
	rulesJSON, err := json.Marshal(ruleSets)
	if err != nil {
		t.Fatal(err)
	}
	var copy AccountRuleSets
	if err = json.Unmarshal(rulesJSON, &copy); err != nil {
		t.Fatal(err)
	}
	for _, rule := range copy.Global.Underride {
		switch rule.RuleID {
		case MRuleMessage, MRuleEncrypted:
			rule.Actions = []*Action{{Kind: NotifyAction}}
		}
	}
	return &copy
}

func ruleByID(rules []*Rule, ruleID string) *Rule {
	for _, rule := range rules {
		if rule != nil && rule.RuleID == ruleID {
			return rule
		}
	}
	return nil
}

func ruleIDs(rules []*Rule) []string {
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule == nil {
			ids = append(ids, "")
			continue
		}
		ids = append(ids, rule.RuleID)
	}
	return ids
}
