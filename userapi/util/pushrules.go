package util

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/YingSuiAI/dirextalk-message-server/internal/eventutil"
	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/producers"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/storage"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

const pushRulesAccountDataType = "m.push_rules"

// QueryAndReconcilePushRules lazily persists the current default notification
// sound rules for an existing account. Publishing the account-data update keeps
// incremental /sync clients on the same durable rule set used for evaluation.
func QueryAndReconcilePushRules(
	ctx context.Context,
	db storage.UserDatabase,
	syncProducer *producers.SyncAPI,
	localpart string,
	serverName spec.ServerName,
) (*pushrules.AccountRuleSets, error) {
	ruleSets, err := db.QueryPushRules(ctx, localpart, serverName)
	if err != nil {
		return nil, err
	}
	if !pushrules.ReconcileDefaultNotificationSoundRules(ruleSets) {
		return ruleSets, nil
	}
	if syncProducer == nil {
		return nil, fmt.Errorf("sync producer is required to reconcile push rules")
	}

	rulesJSON, err := json.Marshal(ruleSets)
	if err != nil {
		return nil, fmt.Errorf("marshal reconciled push rules: %w", err)
	}
	if err = db.SaveAccountData(ctx, localpart, serverName, "", pushRulesAccountDataType, rulesJSON); err != nil {
		return nil, fmt.Errorf("save reconciled push rules: %w", err)
	}
	userID := fmt.Sprintf("@%s:%s", localpart, serverName)
	if err = syncProducer.SendAccountData(userID, eventutil.AccountData{Type: pushRulesAccountDataType}); err != nil {
		return nil, fmt.Errorf("publish reconciled push rules: %w", err)
	}
	return ruleSets, nil
}
