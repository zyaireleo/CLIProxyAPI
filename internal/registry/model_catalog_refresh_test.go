package registry

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCatalogRegistrationPreservesQuotaAndSuspension(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("catalog-account", "antigravity", []*ModelInfo{{ID: "old"}})
	r.SetModelQuotaExceeded("catalog-account", "old")
	r.SuspendClientModel("catalog-account", "old", "quota")
	r.RegisterClientPreservingState("catalog-account", "antigravity", []*ModelInfo{{ID: "old"}, {ID: "new"}})
	require.NotNil(t, r.models["old"].QuotaExceededClients["catalog-account"])
	require.Equal(t, "quota", r.models["old"].SuspendedClients["catalog-account"])
	require.True(t, r.ClientSupportsModel("catalog-account", "new"))
}
