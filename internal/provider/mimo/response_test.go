package mimo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func TestParseBalance_Success(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"message": "success",
		"data": {
			"balance": "250.75",
			"currency": "CNY",
			"cashBalance": "200.00",
			"giftBalance": "50.75"
		}
	}`)

	data, err := parseBalance(raw)
	require.NoError(t, err)
	assert.Equal(t, "250.75", data.Balance)
	assert.Equal(t, "CNY", data.Currency)
	require.NotNil(t, data.CashBalance)
	assert.Equal(t, "200.00", *data.CashBalance)
	require.NotNil(t, data.GiftBalance)
	assert.Equal(t, "50.75", *data.GiftBalance)
}

func TestParseBalance_SessionExpired(t *testing.T) {
	raw := []byte(`{"code": 401, "message": "unauthorized"}`)
	_, err := parseBalance(raw)
	assert.ErrorIs(t, err, errSessionExpired)

	raw403 := []byte(`{"code": 403, "message": "forbidden"}`)
	_, err = parseBalance(raw403)
	assert.ErrorIs(t, err, errSessionExpired)
}

func TestParseBalance_APIError(t *testing.T) {
	raw := []byte(`{"code": 1001, "message": "server error"}`)
	_, err := parseBalance(raw)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "server error")
}

func TestParseSnapshot_Full(t *testing.T) {
	balanceJSON := []byte(`{
		"code": 0,
		"message": "success",
		"data": {
			"balance": "99.50",
			"currency": "USD"
		}
	}`)
	detailJSON := []byte(`{
		"code": 0,
		"message": "success",
		"data": {
			"planCode": "ENTERPRISE",
			"currentPeriodEnd": "2026-12-31 23:59:59",
			"expired": false
		}
	}`)
	usageJSON := []byte(`{
		"code": 0,
		"message": "success",
		"data": {
			"monthUsage": {
				"percent": 75.5,
				"items": [
					{
						"name": "Token Plan",
						"used": 75500000,
						"limit": 100000000,
						"percent": 75.5
					}
				]
			}
		}
	}`)

	now := time.Now()
	snapshot, err := parseSnapshot(balanceJSON, detailJSON, usageJSON, now)
	require.NoError(t, err)

	assert.Equal(t, "mimo", snapshot.Provider)
	assert.Equal(t, now, snapshot.FetchedAt)
	require.NotNil(t, snapshot.Billing)
	assert.Equal(t, 99.50, *snapshot.Billing.Balance)
	require.NotNil(t, snapshot.Identity)
	assert.Equal(t, "ENTERPRISE", snapshot.Identity.Plan)

	require.Len(t, snapshot.Periods, 1)
	p := snapshot.Periods[0]
	assert.Equal(t, "Token Plan (ENTERPRISE)", p.Name)
	assert.Equal(t, models.PeriodMonthly, p.PeriodType)
	assert.Equal(t, 76, p.Utilization)
	require.NotNil(t, p.Used)
	assert.Equal(t, 75500000, *p.Used)
	require.NotNil(t, p.Limit)
	assert.Equal(t, 100000000, *p.Limit)
	require.NotNil(t, p.ResetsAt)
	assert.Equal(t, 2026, p.ResetsAt.Year())
	assert.Equal(t, time.December, p.ResetsAt.Month())
	assert.Equal(t, 31, p.ResetsAt.Day())
}

func TestParseSnapshot_BalanceOnly(t *testing.T) {
	balanceJSON := []byte(`{
		"code": 0,
		"message": "success",
		"data": {
			"balance": "10.00",
			"currency": "CNY"
		}
	}`)

	now := time.Now()
	snapshot, err := parseSnapshot(balanceJSON, nil, nil, now)
	require.NoError(t, err)

	assert.Equal(t, "mimo", snapshot.Provider)
	require.NotNil(t, snapshot.Billing)
	assert.Equal(t, 10.00, *snapshot.Billing.Balance)
	assert.Empty(t, snapshot.Periods)
	assert.Nil(t, snapshot.Identity)
}
