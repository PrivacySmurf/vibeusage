package mimo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

func TestMimo_Metadata(t *testing.T) {
	m := Mimo{}
	meta := m.Meta()
	assert.Equal(t, "mimo", meta.ID)
	assert.Equal(t, "Xiaomi MiMo", meta.Name)
	assert.NotEmpty(t, meta.Description)
	assert.NotEmpty(t, meta.Homepage)
	assert.NotEmpty(t, meta.DashboardURL)
}

func TestMimo_Registration(t *testing.T) {
	p, ok := provider.Get("mimo")
	assert.True(t, ok)
	assert.Equal(t, "mimo", p.Meta().ID)
}

func TestMimo_CredentialSources(t *testing.T) {
	m := Mimo{}
	sources := m.CredentialSources()
	assert.True(t, sources.CheckStrategy)
	assert.Contains(t, sources.EnvVars, "MIMO_COOKIE")
}

func TestMimo_Status(t *testing.T) {
	m := Mimo{}
	status := m.FetchStatus(context.Background())
	assert.Equal(t, models.StatusUnknown, status.Level)
}

func TestMimo_ValidateCookie(t *testing.T) {
	assert.Error(t, validateMimoCookie(""))
	assert.Error(t, validateMimoCookie("   "))
	assert.Error(t, validateMimoCookie("invalid_cookie_string"))

	assert.NoError(t, validateMimoCookie("api-platform_serviceToken=abc; userId=123"))
	assert.NoError(t, validateMimoCookie("foo=bar"))
}

func TestMimo_AcceptCredential(t *testing.T) {
	m := Mimo{}

	err := m.AcceptCredential("")
	assert.Error(t, err)

	err = m.AcceptCredential("api-platform_serviceToken=token123; userId=user456")
	assert.NoError(t, err)

	// JSON format
	err = m.AcceptCredential(`{"cookie":"api-platform_serviceToken=token123; userId=user456"}`)
	assert.NoError(t, err)
}

func TestWebConsoleStrategy_FetchSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Cookie"), "api-platform_serviceToken=test")

		switch r.URL.Path {
		case "/balance":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"message": "success",
				"data": {
					"balance": "128.50",
					"currency": "CNY",
					"cashBalance": "100.00",
					"giftBalance": "28.50"
				}
			}`))
		case "/tokenPlan/detail":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"message": "success",
				"data": {
					"planCode": "TEAM_PRO",
					"currentPeriodEnd": "2026-11-01 00:00:00",
					"expired": false
				}
			}`))
		case "/tokenPlan/usage":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"code": 0,
				"message": "success",
				"data": {
					"monthUsage": {
						"percent": 42.0,
						"items": [
							{
								"name": "Token Plan",
								"used": 42000000,
								"limit": 100000000,
								"percent": 42.0
							}
						]
					}
				}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("MIMO_COOKIE", "api-platform_serviceToken=test; userId=user123")

	strat := &WebConsoleStrategy{
		HTTPTimeout: 5.0,
		BaseURL:     server.URL,
	}

	res, err := strat.Fetch(context.Background())
	require.NoError(t, err)
	assert.True(t, res.Success)

	snap := res.Snapshot
	require.NotNil(t, snap)
	assert.Equal(t, "mimo", snap.Provider)
	require.NotNil(t, snap.Billing)
	assert.Equal(t, 128.50, *snap.Billing.Balance)
	require.NotNil(t, snap.Identity)
	assert.Equal(t, "TEAM_PRO", snap.Identity.Plan)

	require.Len(t, snap.Periods, 1)
	period := snap.Periods[0]
	assert.Equal(t, "Token Plan (TEAM_PRO)", period.Name)
	assert.Equal(t, 42, period.Utilization)
	assert.Equal(t, 42000000, *period.Used)
	assert.Equal(t, 100000000, *period.Limit)
	require.NotNil(t, period.ResetsAt)
	assert.Equal(t, 2026, period.ResetsAt.Year())
	assert.Equal(t, time.November, period.ResetsAt.Month())
}

func TestWebConsoleStrategy_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	t.Setenv("MIMO_COOKIE", "api-platform_serviceToken=expired; userId=123")

	strat := &WebConsoleStrategy{
		HTTPTimeout: 5.0,
		BaseURL:     server.URL,
	}

	res, err := strat.Fetch(context.Background())
	require.NoError(t, err)
	assert.False(t, res.Success)
	assert.False(t, res.ShouldFallback)
	assert.Contains(t, res.Error, "session expired")
}
