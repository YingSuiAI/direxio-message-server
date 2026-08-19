package pushrules

// ReconcileDefaultNotificationSoundRules upgrades the two historical generic
// message rules that notified without a sound tweak. It deliberately matches
// the complete old default shape so disabled or otherwise customized rules are
// left untouched, and it never changes rule ordering.
func ReconcileDefaultNotificationSoundRules(ruleSets *AccountRuleSets) bool {
	if ruleSets == nil {
		return false
	}

	changed := false
	for _, rule := range ruleSets.Global.Underride {
		if !isLegacySilentMessageRule(rule) {
			continue
		}
		rule.Actions = []*Action{
			{Kind: NotifyAction},
			{Kind: SetTweakAction, Tweak: SoundTweak, Value: "default"},
		}
		changed = true
	}
	return changed
}

func isLegacySilentMessageRule(rule *Rule) bool {
	if rule == nil || !rule.Default || !rule.Enabled || rule.Pattern != nil {
		return false
	}
	if len(rule.Actions) != 1 || rule.Actions[0] == nil || rule.Actions[0].Kind != NotifyAction {
		return false
	}
	if rule.Actions[0].Tweak != UnknownTweak || rule.Actions[0].Value != nil {
		return false
	}
	if len(rule.Conditions) != 1 || rule.Conditions[0] == nil {
		return false
	}
	condition := rule.Conditions[0]
	if condition.Kind != EventMatchCondition || condition.Key != "type" || condition.Pattern == nil || condition.Is != "" {
		return false
	}

	switch rule.RuleID {
	case MRuleMessage:
		return *condition.Pattern == "m.room.message"
	case MRuleEncrypted:
		return *condition.Pattern == "m.room.encrypted"
	default:
		return false
	}
}
