package modelstudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

var errModelStudioSession = errors.New("Model Studio browser session is expired or invalid")

type consoleEnvelope struct {
	Code            json.RawMessage `json:"code"`
	HTTPStatusCode  json.RawMessage `json:"httpStatusCode"`
	Message         string          `json:"message"`
	Data            json.RawMessage `json:"data"`
	SuccessResponse json.RawMessage `json:"successResponse"`
}

type subscriptionSummaryResponse struct {
	Code    json.RawMessage `json:"Code"`
	Message string          `json:"Message"`
	Success bool            `json:"Success"`
	Data    json.RawMessage `json:"Data"`
}

type subscriptionSummaryData struct {
	TotalCount              json.RawMessage          `json:"TotalCount"`
	TotalValue              json.RawMessage          `json:"TotalValue"`
	UsedValue               json.RawMessage          `json:"UsedValue"`
	TotalSurplusValue       json.RawMessage          `json:"TotalSurplusValue"`
	NearestExpireDate       json.RawMessage          `json:"NearestExpireDate"`
	SubscriptionSummaryList []subscriptionSummaryRow `json:"SubscriptionSummaryList"`
}

type subscriptionSummaryRow struct {
	TotalValue        json.RawMessage `json:"TotalValue"`
	UsedValue         json.RawMessage `json:"UsedValue"`
	TotalSurplusValue json.RawMessage `json:"TotalSurplusValue"`
	CycleTotalValue   json.RawMessage `json:"CycleTotalValue"`
	CycleSurplusValue json.RawMessage `json:"CycleSurplusValue"`
	NearestExpireDate json.RawMessage `json:"NearestExpireDate"`
}

type consoleUserInfo struct {
	SECToken string          `json:"secToken"`
	Data     json.RawMessage `json:"data"`
}

type AccountDetailResponse struct {
	RequestId string      `json:"RequestId"`
	Code      string      `json:"Code"`
	Message   string      `json:"Message"`
	Success   bool        `json:"Success"`
	Data      AccountData `json:"Data"`
}

type AccountData struct {
	TotalQuota     float64 `json:"TotalQuota"`
	UsedQuota      float64 `json:"UsedQuota"`
	RemainingQuota float64 `json:"RemainingQuota"`
	QuotaUnit      string  `json:"QuotaUnit"`
	EffectiveTime  int64   `json:"EffectiveTime"`
	ExpireTime     int64   `json:"ExpireTime"`
	PlanName       string  `json:"PlanName"`
	PlanType       string  `json:"PlanType"`
	TotalTokens    float64 `json:"TotalTokens"`
	UsedTokens     float64 `json:"UsedTokens"`
}

type SubscriptionSeatResponse struct {
	RequestId string   `json:"RequestId"`
	Code      string   `json:"Code"`
	Message   string   `json:"Message"`
	Success   bool     `json:"Success"`
	Data      SeatData `json:"Data"`
}

type SeatData struct {
	TotalSeats      int          `json:"TotalSeats"`
	AssignedSeats   int          `json:"AssignedSeats"`
	UnassignedSeats int          `json:"UnassignedSeats"`
	Seats           []SeatDetail `json:"Seats"`
}

type SeatDetail struct {
	SeatId         string  `json:"SeatId"`
	UserId         string  `json:"UserId"`
	UserName       string  `json:"UserName"`
	Email          string  `json:"Email"`
	TotalQuota     float64 `json:"TotalQuota"`
	UsedQuota      float64 `json:"UsedQuota"`
	RemainingQuota float64 `json:"RemainingQuota"`
	Role           string  `json:"Role"`
	Status         string  `json:"Status"`
}

func parseModelStudioResponses(accountBody, seatBody []byte, source string) (*models.UsageSnapshot, error) {
	return parseModelStudioConsoleResponses(nil, accountBody, seatBody, nil, source)
}

func parseModelStudioConsoleResponses(summaryBody, accountBody, seatBody, personalBody []byte, source string) (*models.UsageSnapshot, error) {
	var periods []models.UsagePeriod
	var planName string
	var expireTime *time.Time

	// 1. Parse BssOpenAPI-V3 GetSubscriptionSummary (Team Plan / Bss summary)
	if len(summaryBody) > 0 {
		d, err := parseSubscriptionSummary(summaryBody)
		if err != nil {
			return nil, err
		}

		total, totalOK := parseJSONFloat(d.TotalValue)
		used, usedOK := parseJSONFloat(d.UsedValue)
		remaining, remainingOK := parseJSONFloat(d.TotalSurplusValue)
		expiresAt := parseJSONTime(d.NearestExpireDate)

		if len(d.SubscriptionSummaryList) > 0 {
			item := d.SubscriptionSummaryList[0]
			if !totalOK || total == 0 {
				total, totalOK = parseJSONFloat(item.TotalValue)
				if !totalOK || total == 0 {
					total, totalOK = parseJSONFloat(item.CycleTotalValue)
				}
			}
			if !usedOK {
				used, usedOK = parseJSONFloat(item.UsedValue)
			}
			if !remainingOK {
				remaining, remainingOK = parseJSONFloat(item.TotalSurplusValue)
				if !remainingOK {
					remaining, remainingOK = parseJSONFloat(item.CycleSurplusValue)
				}
			}
			if expiresAt == nil {
				expiresAt = parseJSONTime(item.NearestExpireDate)
			}
		}

		if !usedOK && totalOK && remainingOK {
			used = max(0, total-remaining)
			usedOK = true
		}

		if totalOK && total > 0 {
			util := 0
			if usedOK {
				util = int((used / total) * 100)
			}
			if util > 100 {
				util = 100
			}
			usedInt := int(used)
			totalInt := int(total)
			planName = "Team Token Plan"
			expireTime = expiresAt

			periods = append(periods, models.UsagePeriod{
				Name:        "Team Token Plan",
				Utilization: util,
				PeriodType:  models.PeriodMonthly,
				ResetsAt:    expireTime,
				Used:        &usedInt,
				Limit:       &totalInt,
			})
		}
	}

	// 2. Parse AccountDetailResponse
	if len(accountBody) > 0 {
		var accResp AccountDetailResponse
		if err := json.Unmarshal(accountBody, &accResp); err == nil && (accResp.Success || accResp.Code == "200" || accResp.Data.TotalQuota > 0 || accResp.Data.TotalTokens > 0) {
			data := accResp.Data
			total := data.TotalQuota
			if total == 0 {
				total = data.TotalTokens
			}
			used := data.UsedQuota
			if used == 0 && data.UsedTokens > 0 {
				used = data.UsedTokens
			}

			if data.PlanName != "" {
				planName = data.PlanName
			}

			if expireTime == nil {
				if data.ExpireTime > 0 {
					t := time.UnixMilli(data.ExpireTime).UTC()
					expireTime = &t
				} else if data.EffectiveTime > 0 {
					t := time.UnixMilli(data.EffectiveTime).AddDate(0, 1, 0).UTC()
					expireTime = &t
				}
			}

			if total > 0 && len(periods) == 0 {
				util := int((used / total) * 100)
				if util > 100 {
					util = 100
				}
				usedInt := int(used)
				totalInt := int(total)
				name := "Token Plan"
				if planName != "" {
					name = planName
				}
				periods = append(periods, models.UsagePeriod{
					Name:        name,
					Utilization: util,
					PeriodType:  models.PeriodMonthly,
					ResetsAt:    expireTime,
					Used:        &usedInt,
					Limit:       &totalInt,
				})
			}
		}
	}

	// 3. Parse SeatDetails
	if len(seatBody) > 0 {
		var seatResp SubscriptionSeatResponse
		if err := json.Unmarshal(seatBody, &seatResp); err == nil && len(seatResp.Data.Seats) > 0 {
			for _, seat := range seatResp.Data.Seats {
				if seat.TotalQuota <= 0 {
					continue
				}
				util := int((seat.UsedQuota / seat.TotalQuota) * 100)
				if util > 100 {
					util = 100
				}
				usedInt := int(seat.UsedQuota)
				totalInt := int(seat.TotalQuota)
				seatName := seat.UserName
				if seatName == "" {
					seatName = seat.Email
				}
				if seatName == "" {
					seatName = seat.SeatId
				}
				periods = append(periods, models.UsagePeriod{
					Name:        "Seat: " + seatName,
					Utilization: util,
					PeriodType:  models.PeriodMonthly,
					ResetsAt:    expireTime,
					Used:        &usedInt,
					Limit:       &totalInt,
				})
			}
		}
	}

	// 4. Parse Personal Rolling Window usage
	if len(personalBody) > 0 {
		var persResp struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Success bool   `json:"success"`
			Data    struct {
				Per5HourPercentage float64 `json:"per5HourPercentage"`
				Per1WeekPercentage float64 `json:"per1WeekPercentage"`
				Per5HourResetTime  int64   `json:"per5HourResetTime"`
				Per1WeekResetTime  int64   `json:"per1WeekResetTime"`
			} `json:"data"`
		}

		if err := json.Unmarshal(personalBody, &persResp); err == nil && (persResp.Success || persResp.Code == "200") {
			data := persResp.Data
			if data.Per5HourPercentage > 0 || data.Per1WeekPercentage > 0 || data.Per5HourResetTime > 0 {
				util5h := int(data.Per5HourPercentage)
				if util5h > 100 {
					util5h = 100
				}
				var resets5h *time.Time
				if data.Per5HourResetTime > 0 {
					t := time.UnixMilli(data.Per5HourResetTime).UTC()
					resets5h = &t
				}
				periods = append(periods, models.UsagePeriod{
					Name:        "5-Hour Window",
					Utilization: util5h,
					PeriodType:  models.PeriodSession,
					ResetsAt:    resets5h,
				})

				util1w := int(data.Per1WeekPercentage)
				if util1w > 100 {
					util1w = 100
				}
				var resets1w *time.Time
				if data.Per1WeekResetTime > 0 {
					t := time.UnixMilli(data.Per1WeekResetTime).UTC()
					resets1w = &t
				}
				periods = append(periods, models.UsagePeriod{
					Name:        "7-Day Window",
					Utilization: util1w,
					PeriodType:  models.PeriodWeekly,
					ResetsAt:    resets1w,
				})

				if planName == "" {
					planName = "Personal/Solo Plan"
				}
			}
		}
	}

	if len(periods) == 0 {
		return nil, fmt.Errorf("no valid token plan quota or seat data found in response")
	}

	var identity *models.ProviderIdentity
	if planName != "" {
		identity = &models.ProviderIdentity{
			Plan: planName,
		}
	}

	return &models.UsageSnapshot{
		Provider:  "modelstudio",
		FetchedAt: time.Now().UTC(),
		Periods:   periods,
		Identity:  identity,
		Source:    source,
	}, nil
}

func parseConsoleSECToken(body []byte) (string, error) {
	payload, err := unwrapConsoleSuccessResponse(body)
	if err != nil {
		return "", err
	}

	var info consoleUserInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return "", fmt.Errorf("decode user info: %w", err)
	}
	if info.SECToken != "" {
		return info.SECToken, nil
	}
	if len(bytes.TrimSpace(info.Data)) > 0 && !bytes.Equal(bytes.TrimSpace(info.Data), []byte("null")) {
		data, err := decodeEmbeddedJSON(info.Data)
		if err == nil {
			var nested consoleUserInfo
			if json.Unmarshal(data, &nested) == nil && nested.SECToken != "" {
				return nested.SECToken, nil
			}
		}
	}
	return "", fmt.Errorf("sec_token missing from user info response")
}

func parseSubscriptionSummary(body []byte) (subscriptionSummaryData, error) {
	if isLikelyLoginHTML(body) {
		return subscriptionSummaryData{}, fmt.Errorf("%w: Alibaba console returned a login page", errModelStudioSession)
	}
	payload, err := unwrapConsoleSuccessResponse(body)
	if err != nil {
		return subscriptionSummaryData{}, err
	}

	var response subscriptionSummaryResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return subscriptionSummaryData{}, fmt.Errorf("decode subscription summary response: %w", err)
	}
	code := rawJSONScalar(response.Code)
	if err := alibabaConsoleAPIError(code, response.Message); err != nil {
		return subscriptionSummaryData{}, err
	}
	if len(bytes.TrimSpace(response.Data)) == 0 || bytes.Equal(bytes.TrimSpace(response.Data), []byte("null")) {
		return subscriptionSummaryData{}, fmt.Errorf("subscription summary response has no data")
	}

	data, err := decodeEmbeddedJSON(response.Data)
	if err != nil {
		return subscriptionSummaryData{}, fmt.Errorf("decode subscription summary data: %w", err)
	}
	var summary subscriptionSummaryData
	if err := json.Unmarshal(data, &summary); err != nil {
		return subscriptionSummaryData{}, fmt.Errorf("decode subscription summary data: %w", err)
	}
	return summary, nil
}

func unwrapConsoleSuccessResponse(body []byte) ([]byte, error) {
	payload, err := decodeEmbeddedJSON(body)
	if err != nil {
		return nil, fmt.Errorf("decode Alibaba console response: %w", err)
	}

	var envelope consoleEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode Alibaba console response: %w", err)
	}
	isConsoleEnvelope := len(bytes.TrimSpace(envelope.SuccessResponse)) > 0 ||
		len(bytes.TrimSpace(envelope.HTTPStatusCode)) > 0
	if isConsoleEnvelope {
		if successPayload, err := decodeEmbeddedJSON(envelope.SuccessResponse); err == nil &&
			len(successPayload) > 0 && successPayload[0] == '{' {
			return successPayload, nil
		}
		if dataPayload, err := decodeEmbeddedJSON(envelope.Data); err == nil &&
			len(dataPayload) > 0 && dataPayload[0] == '{' {
			return dataPayload, nil
		}
	}

	code := rawJSONScalar(envelope.Code)
	if err := alibabaConsoleAPIError(code, envelope.Message); err != nil {
		return nil, err
	}
	return payload, nil
}

func alibabaConsoleAPIError(code, message string) error {
	if isModelStudioSessionError(code, message) {
		if code != "" && message != "" {
			return fmt.Errorf("%w: Alibaba console API error %s: %s", errModelStudioSession, code, message)
		}
		if code != "" {
			return fmt.Errorf("%w: Alibaba console API error %s", errModelStudioSession, code)
		}
		return fmt.Errorf("%w: %s", errModelStudioSession, message)
	}
	if code == "" || code == "200" || strings.EqualFold(code, "OK") || strings.EqualFold(code, "Success") {
		return nil
	}
	if message != "" {
		return fmt.Errorf("Alibaba console API error %s: %s", code, message)
	}
	return fmt.Errorf("Alibaba console API error %s", code)
}

func isModelStudioSessionError(code, message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(code + " " + message))
	if strings.Contains(normalized, "workspace.notauthorised") ||
		strings.Contains(normalized, "workspace.notauthorized") {
		return false
	}
	for _, marker := range []string{
		"needlogin",
		"postonlyortokenerror",
		"tokenerror",
		"token expired",
		"token is expired",
		"session expired",
		"session is expired",
		"request has expired",
		"refresh the page",
		"refresh page",
		"need to log in",
		"login required",
		"sign in",
		"unauthorized",
		"unauthorised",
		"forbidden",
		"请求已经过期",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func isLikelyLoginHTML(body []byte) bool {
	normalized := strings.ToLower(string(bytes.TrimSpace(body)))
	return strings.Contains(normalized, "<html") &&
		(strings.Contains(normalized, "login") || strings.Contains(normalized, "sign in"))
}

func decodeEmbeddedJSON(raw []byte) ([]byte, error) {
	decoded := bytes.TrimSpace(raw)
	for range 4 {
		if len(decoded) == 0 {
			return nil, fmt.Errorf("empty JSON value")
		}
		if decoded[0] != '"' {
			return decoded, nil
		}

		var embedded string
		if err := json.Unmarshal(decoded, &embedded); err != nil {
			return nil, err
		}
		decoded = bytes.TrimSpace([]byte(embedded))
	}
	return nil, fmt.Errorf("too many embedded JSON layers")
}

func parseJSONFloat(raw json.RawMessage) (float64, bool) {
	decoded, err := decodeEmbeddedJSON(raw)
	if err != nil {
		return 0, false
	}

	var number float64
	if err := json.Unmarshal(decoded, &number); err == nil {
		return number, true
	}
	var text string
	if err := json.Unmarshal(decoded, &text); err != nil {
		return 0, false
	}
	number, err = strconv.ParseFloat(text, 64)
	return number, err == nil
}

func parseJSONTime(raw json.RawMessage) *time.Time {
	decoded := bytes.TrimSpace(raw)
	var text string
	if err := json.Unmarshal(decoded, &text); err == nil {
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			return unixJSONTime(value)
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				utc := parsed.UTC()
				return &utc
			}
		}
		return nil
	}

	if value, ok := parseJSONFloat(decoded); ok {
		return unixJSONTime(value)
	}
	return nil
}

func unixJSONTime(value float64) *time.Time {
	if value > 0 {
		var parsed time.Time
		if value >= 1e12 {
			parsed = time.UnixMilli(int64(value)).UTC()
		} else {
			parsed = time.Unix(int64(value), 0).UTC()
		}
		return &parsed
	}
	return nil
}

func rawJSONScalar(raw json.RawMessage) string {
	decoded, err := decodeEmbeddedJSON(raw)
	if err != nil {
		return ""
	}
	var text string
	if err := json.Unmarshal(decoded, &text); err == nil {
		return text
	}
	return string(decoded)
}
