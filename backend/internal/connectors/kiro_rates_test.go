package connectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mydisha/keirouter/backend/internal/core"
)

func TestKiroModelRates(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		want        *float64
		wantError   bool
	}{
		{"missing", "", nil, false},
		{"null", `,"rateMultiplier":null`, nil, false},
		{"zero", `,"rateMultiplier":0`, ratePointer(0), false},
		{"standard", `,"rateMultiplier":1`, ratePointer(1), false},
		{"fraction", `,"rateMultiplier":0.125`, ratePointer(0.125), false},
		{"premium", `,"rateMultiplier":2.4`, ratePointer(2.4), false},
		{"negative", `,"rateMultiplier":-1`, nil, true},
		{"malformed", `,"rateMultiplier":"bad"`, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previous := sharedClient
			t.Cleanup(func() { sharedClient = previous })
			sharedClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"test-model","modelName":"Test"` + tt.field + `}]}`))}, nil
			})}
			models, err := NewKiro("kiro", "").ListModels(context.Background(), core.Credentials{AccessToken: "test"})
			if tt.wantError {
				require.Error(t, err)
				require.Empty(t, models)
				return
			}
			require.NoError(t, err)
			require.Len(t, models, 4)
			for _, model := range models {
				require.Equal(t, tt.want, model.RateMultiplier)
				if tt.want != nil {
					require.Contains(t, model.Name, fmt.Sprintf("(%gx credit)", *tt.want))
				} else {
					require.NotContains(t, model.Name, "credit")
				}
			}
		})
	}
}

func ratePointer(value float64) *float64 { return &value }

func TestKiroModelRateCacheCopies(t *testing.T) {
	const account = "rate-cache-copy-test"
	t.Cleanup(func() { kiroModelCache.Delete(account) })
	rate := 0.125
	storeKiroModels(account, []ModelSpec{{ID: "priced", RateMultiplier: &rate}})
	rate = 99
	first, ok := loadKiroModels(account)
	require.True(t, ok)
	require.Equal(t, 0.125, *first[0].RateMultiplier)
	*first[0].RateMultiplier = 88
	second, ok := loadKiroModels(account)
	require.True(t, ok)
	require.Equal(t, 0.125, *second[0].RateMultiplier)
}
