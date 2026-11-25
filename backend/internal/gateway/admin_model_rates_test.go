package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mydisha/keirouter/backend/internal/connectors"
	"github.com/mydisha/keirouter/backend/internal/core"
	"github.com/mydisha/keirouter/backend/internal/store"
	"github.com/mydisha/keirouter/backend/internal/vault"
)

type modelRatesSource struct {
	calls  int
	models []connectors.ModelSpec
}

func (s *modelRatesSource) ListModels(context.Context, core.Credentials) ([]connectors.ModelSpec, error) {
	s.calls++
	return s.models, nil
}

func TestProviderModelRatesEnrichStaticEntries(t *testing.T) {
	s, db := newCustomProviderTestServer(t)
	rate := 1.25
	source := &modelRatesSource{models: []connectors.ModelSpec{{
		ID: "gpt-5.6-sol", Name: "Live GPT (1.25x credit)", Kind: core.ServiceLLM, RateMultiplier: &rate,
	}}}
	previous := connectors.GetLiveModelSource("kiro")
	connectors.RegisterLiveModelSource("kiro", source)
	t.Cleanup(func() { connectors.RegisterLiveModelSource("kiro", previous) })
	for i, id := range []string{"rate-account-1", "rate-account-2"} {
		account := store.Account{ID: id, TenantID: adminTenant, Provider: "kiro", AuthKind: "oauth", Priority: i}
		require.NoError(t, s.vault.Seal(&account, vault.NewSecret{AccessToken: "test"}))
		require.NoError(t, db.Accounts().Create(context.Background(), account))
	}
	rec := httptest.NewRecorder()
	s.adminProviderModels(rec, withChiID(http.MethodGet, "/providers/kiro/models", "kiro"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, source.calls, "first valid account's rate must not be overwritten")
	var response struct {
		Models []map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	matches := 0
	for _, model := range response.Models {
		if model["id"] == "gpt-5.6-sol" {
			matches++
			require.Equal(t, 1.25, model["rate_multiplier"])
			require.Equal(t, "Live GPT (1.25x credit)", model["name"])
		} else {
			require.NotContains(t, model, "rate_multiplier", "missing rates must not become 0x or 1x")
		}
	}
	require.Equal(t, 1, matches, "live enrichment must not duplicate a static model")

	custom := connectors.ModelSpec{ID: "gpt-5.6-sol", Name: "My custom label", Kind: core.ServiceLLM}
	connectors.SetDynamicModels("kiro", []connectors.ModelSpec{custom})
	t.Cleanup(func() { connectors.SetDynamicModels("kiro", nil) })
	require.NoError(t, db.CustomProviders().CreateModel(context.Background(), store.CustomModel{
		ID: "custom-priced-model", TenantID: adminTenant, ProviderID: "kiro",
		ModelID: custom.ID, DisplayName: custom.Name, Kind: string(custom.Kind),
	}))
	models, pending := s.providerModelInfos(context.Background(), "kiro", core.ServiceLLM, false, 0)
	require.False(t, pending)
	for _, model := range models {
		if model.ID == custom.ID {
			require.True(t, model.Custom)
			require.Equal(t, custom.Name, model.Name)
			require.Equal(t, rate, *model.RateMultiplier)
		}
	}
	source.models[0].RateMultiplier = nil
	source.calls = 0
	models, _ = s.providerModelInfos(context.Background(), "kiro", core.ServiceLLM, false, 0)
	require.Equal(t, 1, source.calls, "even an unpriced overlap must use only the first valid account")
	for _, model := range models {
		require.Nil(t, model.RateMultiplier)
	}

	zero := 0.0
	source.models = []connectors.ModelSpec{{ID: "new-live-model", Name: "New", Kind: core.ServiceLLM, RateMultiplier: &zero}}
	models, _ = s.providerModelInfos(context.Background(), "kiro", core.ServiceLLM, false, 0)
	last := models[len(models)-1]
	require.Equal(t, "new-live-model", last.ID)
	require.True(t, last.Discovered)
	payload, err := json.Marshal(last)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"rate_multiplier":0`)
}
