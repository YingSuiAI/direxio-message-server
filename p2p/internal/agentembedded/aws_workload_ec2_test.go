package agentembedded

import (
	"testing"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestEC2ChangeProjectionCarriesProvisionBinding(t *testing.T) {
	got := changeMap(coreaws.Change{ID: "change", ProvisionID: "provision", Revision: 1})
	if got["provision_id"] != "provision" { t.Fatalf("change projection lost provision binding: %#v", got) }
}
