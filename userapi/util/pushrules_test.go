package util_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/eventutil"
	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/producers"
	userUtil "github.com/YingSuiAI/dirextalk-message-server/userapi/util"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/nats-io/nats.go"
)

type recordingAccountDataPublisher struct {
	mu   sync.Mutex
	msgs []*nats.Msg
}

func (p *recordingAccountDataPublisher) PublishMsg(msg *nats.Msg, _ ...nats.PubOpt) (*nats.PubAck, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copy := *msg
	copy.Header = make(nats.Header, len(msg.Header))
	for key, values := range msg.Header {
		copy.Header[key] = append([]string(nil), values...)
	}
	copy.Data = append([]byte(nil), msg.Data...)
	p.msgs = append(p.msgs, &copy)
	return &nats.PubAck{}, nil
}

func (p *recordingAccountDataPublisher) messages() []*nats.Msg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*nats.Msg(nil), p.msgs...)
}

func TestQueryAndReconcilePushRulesPersistsAndPublishesOnce(t *testing.T) {
	ctx := context.Background()
	localpart := "alice"
	serverName := spec.ServerName("test")

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, ctx, dbType)
		defer closeDB()
		if _, err := db.CreateAccount(ctx, localpart, serverName, "", "", api.AccountTypeUser); err != nil {
			t.Fatal(err)
		}

		legacyRules := deepCopyRuleSets(t, pushrules.DefaultAccountRuleSets(localpart, serverName))
		for _, rule := range legacyRules.Global.Underride {
			switch rule.RuleID {
			case pushrules.MRuleMessage, pushrules.MRuleEncrypted:
				rule.Actions = []*pushrules.Action{{Kind: pushrules.NotifyAction}}
			}
		}
		legacyJSON, err := json.Marshal(legacyRules)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.SaveAccountData(ctx, localpart, serverName, "", "m.push_rules", legacyJSON); err != nil {
			t.Fatal(err)
		}

		publisher := &recordingAccountDataPublisher{}
		syncProducer := producers.NewSyncAPI(db, publisher, "client_data", "notification_data")
		got, err := userUtil.QueryAndReconcilePushRules(ctx, db, syncProducer, localpart, serverName)
		if err != nil {
			t.Fatal(err)
		}
		assertGenericMessageSounds(t, got)

		persisted, err := db.QueryPushRules(ctx, localpart, serverName)
		if err != nil {
			t.Fatal(err)
		}
		assertGenericMessageSounds(t, persisted)

		msgs := publisher.messages()
		if len(msgs) != 1 {
			t.Fatalf("published account-data updates = %d, want 1", len(msgs))
		}
		if gotUserID := msgs[0].Header.Get("user_id"); gotUserID != "@alice:test" {
			t.Fatalf("published user ID = %q, want @alice:test", gotUserID)
		}
		var accountData eventutil.AccountData
		if err = json.Unmarshal(msgs[0].Data, &accountData); err != nil {
			t.Fatal(err)
		}
		if accountData.Type != "m.push_rules" || accountData.RoomID != "" {
			t.Fatalf("unexpected account-data update: %#v", accountData)
		}

		second, err := userUtil.QueryAndReconcilePushRules(ctx, db, syncProducer, localpart, serverName)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(second, persisted) {
			t.Fatal("repeated reconciliation changed the persisted rules")
		}
		if got := len(publisher.messages()); got != 1 {
			t.Fatalf("repeated reconciliation published %d total updates, want 1", got)
		}
	})
}

func deepCopyRuleSets(t *testing.T, source *pushrules.AccountRuleSets) *pushrules.AccountRuleSets {
	t.Helper()
	rulesJSON, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var copy pushrules.AccountRuleSets
	if err = json.Unmarshal(rulesJSON, &copy); err != nil {
		t.Fatal(err)
	}
	return &copy
}

func assertGenericMessageSounds(t *testing.T, ruleSets *pushrules.AccountRuleSets) {
	t.Helper()
	for _, ruleID := range []string{pushrules.MRuleMessage, pushrules.MRuleEncrypted} {
		var actions []*pushrules.Action
		for _, rule := range ruleSets.Global.Underride {
			if rule.RuleID == ruleID {
				actions = rule.Actions
				break
			}
		}
		want := []*pushrules.Action{
			{Kind: pushrules.NotifyAction},
			{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"},
		}
		if !reflect.DeepEqual(actions, want) {
			t.Fatalf("%s actions = %#v, want %#v", ruleID, actions, want)
		}
	}
}
