package mimo

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

var (
	errInvalidBalanceResponse = errors.New("invalid balance response from Xiaomi MiMo")
	errMissingBalancePayload  = errors.New("missing balance payload in Xiaomi MiMo response")
	errInvalidBalanceValue    = errors.New("invalid balance amount in Xiaomi MiMo response")
	errMissingCurrency        = errors.New("missing currency in Xiaomi MiMo response")
	errSessionExpired         = errors.New("xiaomi mimo session expired or invalid")
)

type balanceResponse struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Data    *balanceData `json:"data"`
}

type balanceData struct {
	Balance     string  `json:"balance"`
	Currency    string  `json:"currency"`
	CashBalance *string `json:"cashBalance"`
	GiftBalance *string `json:"giftBalance"`
}

type tokenPlanDetailResponse struct {
	Code    int                  `json:"code"`
	Message string               `json:"message"`
	Data    *tokenPlanDetailData `json:"data"`
}

type tokenPlanDetailData struct {
	PlanCode         string `json:"planCode"`
	CurrentPeriodEnd string `json:"currentPeriodEnd"`
	Expired          bool   `json:"expired"`
}

type tokenPlanUsageResponse struct {
	Code    int                 `json:"code"`
	Message string              `json:"message"`
	Data    *tokenPlanUsageData `json:"data"`
}

type tokenPlanUsageData struct {
	MonthUsage *monthUsageData `json:"monthUsage"`
}

type monthUsageData struct {
	Percent float64         `json:"percent"`
	Items   []usageItemData `json:"items"`
}

type usageItemData struct {
	Name    string  `json:"name"`
	Used    int     `json:"used"`
	Limit   int     `json:"limit"`
	Percent float64 `json:"percent"`
}

func parseBalance(data []byte) (*balanceData, error) {
	var resp balanceResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidBalanceResponse, err)
	}

	if resp.Code == 401 || resp.Code == 403 {
		return nil, errSessionExpired
	}
	if resp.Code != 0 {
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("code %d", resp.Code)
		}
		return nil, fmt.Errorf("xiaomi mimo API error: %s", msg)
	}
	if resp.Data == nil {
		return nil, errMissingBalancePayload
	}
	return resp.Data, nil
}

func parseTokenPlanDetail(data []byte) (*tokenPlanDetailData, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var resp tokenPlanDetailResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 || resp.Data == nil {
		return nil, nil
	}
	return resp.Data, nil
}

func parseTokenPlanUsage(data []byte) (*tokenPlanUsageData, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var resp tokenPlanUsageResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 || resp.Data == nil {
		return nil, nil
	}
	return resp.Data, nil
}

func parsePeriodEnd(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

func parseSnapshot(balanceBytes, detailBytes, usageBytes []byte, now time.Time) (*models.UsageSnapshot, error) {
	balData, err := parseBalance(balanceBytes)
	if err != nil {
		return nil, err
	}

	balFloat, err := strconv.ParseFloat(strings.TrimSpace(balData.Balance), 64)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errInvalidBalanceValue, balData.Balance)
	}

	currency := strings.TrimSpace(balData.Currency)
	if currency == "" {
		return nil, errMissingCurrency
	}

	snapshot := &models.UsageSnapshot{
		Provider:  "mimo",
		FetchedAt: now,
		Billing: &models.BillingDetail{
			Balance: &balFloat,
		},
		Periods: []models.UsagePeriod{},
	}

	detailData, _ := parseTokenPlanDetail(detailBytes)
	usageData, _ := parseTokenPlanUsage(usageBytes)

	var planCode string
	var periodEnd *time.Time
	if detailData != nil {
		planCode = strings.TrimSpace(detailData.PlanCode)
		periodEnd = parsePeriodEnd(detailData.CurrentPeriodEnd)
	}

	if planCode != "" {
		snapshot.Identity = &models.ProviderIdentity{
			Plan: planCode,
		}
	}

	if usageData != nil && usageData.MonthUsage != nil {
		monthUsage := usageData.MonthUsage
		used := 0
		limit := 0
		utilization := 0

		if monthUsage.Percent > 0 {
			if monthUsage.Percent <= 1.0 {
				utilization = int(math.Round(monthUsage.Percent * 100))
			} else {
				utilization = int(math.Round(monthUsage.Percent))
			}
		}

		if len(monthUsage.Items) > 0 {
			first := monthUsage.Items[0]
			used = first.Used
			limit = first.Limit
			if first.Percent > 0 {
				if first.Percent <= 1.0 {
					utilization = int(math.Round(first.Percent * 100))
				} else {
					utilization = int(math.Round(first.Percent))
				}
			} else if limit > 0 {
				utilization = int(math.Round(float64(used) / float64(limit) * 100))
			}
		}

		if used > 0 && limit > 0 && utilization == 0 {
			utilization = int(math.Round(float64(used) / float64(limit) * 100))
			if utilization == 0 {
				utilization = 1
			}
		}

		if utilization > 100 {
			utilization = 100
		} else if utilization < 0 {
			utilization = 0
		}

		periodName := "Token Plan"
		if planCode != "" {
			periodName = fmt.Sprintf("Token Plan (%s)", planCode)
		}

		snapshot.Periods = append(snapshot.Periods, models.UsagePeriod{
			Name:        periodName,
			Model:       planCode,
			PeriodType:  models.PeriodMonthly,
			Utilization: utilization,
			Used:        &used,
			Limit:       &limit,
			ResetsAt:    periodEnd,
		})
	}

	return snapshot, nil
}
