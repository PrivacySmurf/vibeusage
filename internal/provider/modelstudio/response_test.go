package modelstudio

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseModelStudioResponses(t *testing.T) {
	accountJSON := []byte(`{
		"RequestId": "51AE6C6E-9B5C-4D3B-AAC4-0500EBCA5E89",
		"Code": "200",
		"Success": true,
		"Data": {
			"TotalQuota": 1000000,
			"UsedQuota": 250000,
			"RemainingQuota": 750000,
			"EffectiveTime": 1700000000000,
			"ExpireTime": 1730000000000,
			"PlanName": "Team Plan"
		}
	}`)

	seatJSON := []byte(`{
		"RequestId": "51AE6C6E-9B5C-4D3B-AAC4-0500EBCA5E89",
		"Code": "200",
		"Success": true,
		"Data": {
			"TotalSeats": 5,
			"AssignedSeats": 2,
			"Seats": [
				{
					"SeatId": "seat-1",
					"UserName": "Alice",
					"TotalQuota": 500000,
					"UsedQuota": 100000
				},
				{
					"SeatId": "seat-2",
					"UserName": "Bob",
					"TotalQuota": 500000,
					"UsedQuota": 150000
				}
			]
		}
	}`)

	snapshot, err := parseModelStudioResponses(accountJSON, seatJSON, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snapshot.Provider != "modelstudio" {
		t.Errorf("expected provider 'modelstudio', got %s", snapshot.Provider)
	}

	if len(snapshot.Periods) != 3 {
		t.Fatalf("expected 3 periods (1 plan + 2 seats), got %d", len(snapshot.Periods))
	}

	if snapshot.Periods[0].Name != "Team Plan" {
		t.Errorf("expected primary period name 'Team Plan', got %s", snapshot.Periods[0].Name)
	}

	if snapshot.Periods[0].Utilization != 25 {
		t.Errorf("expected utilization 25%%, got %d", snapshot.Periods[0].Utilization)
	}
}

func TestParseSubscriptionSummary(t *testing.T) {
	summaryJSON := []byte(`{
		"Success": true,
		"Data": {
			"TotalCount": 1,
			"TotalValue": 100000000,
			"UsedValue": 25000000,
			"TotalSurplusValue": 75000000,
			"NearestExpireDate": "2026-09-01T00:00:00Z",
			"SubscriptionSummaryList": [
				{
					"TotalValue": 100000000,
					"UsedValue": 25000000,
					"TotalSurplusValue": 75000000,
					"NearestExpireDate": "2026-09-01T00:00:00Z"
				}
			]
		}
	}`)

	snapshot, err := parseModelStudioConsoleResponses(summaryJSON, nil, nil, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshot.Periods) != 1 {
		t.Fatalf("expected 1 period, got %d", len(snapshot.Periods))
	}

	if snapshot.Periods[0].Name != "Team Token Plan" {
		t.Errorf("expected 'Team Token Plan', got %s", snapshot.Periods[0].Name)
	}

	if snapshot.Periods[0].Utilization != 25 {
		t.Errorf("expected 25%% utilization, got %d", snapshot.Periods[0].Utilization)
	}
	wantReset := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	if snapshot.Periods[0].ResetsAt == nil || !snapshot.Periods[0].ResetsAt.Equal(wantReset) {
		t.Errorf("expected reset %s, got %v", wantReset, snapshot.Periods[0].ResetsAt)
	}
}

func TestParseNestedSubscriptionSummary(t *testing.T) {
	summaryJSON := []byte(`{
		"code": "200",
		"data": {
			"Code": "200",
			"Data": "{\"TotalCount\":1,\"TotalValue\":1000,\"TotalSurplusValue\":750,\"NearestExpireDate\":1788220800000}",
			"Message": "",
			"RequestId": "request-id",
			"Success": true
		},
		"httpStatusCode": "200",
		"requestId": "gateway-request-id",
		"successResponse": true
	}`)

	snapshot, err := parseModelStudioConsoleResponses(summaryJSON, nil, nil, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshot.Periods) != 1 {
		t.Fatalf("expected 1 period, got %d", len(snapshot.Periods))
	}
	period := snapshot.Periods[0]
	if period.Utilization != 25 {
		t.Errorf("expected 25%% utilization, got %d%%", period.Utilization)
	}
	if period.Used == nil || *period.Used != 250 {
		t.Errorf("expected 250 used, got %v", period.Used)
	}
	if period.Limit == nil || *period.Limit != 1000 {
		t.Errorf("expected 1000 limit, got %v", period.Limit)
	}
	wantReset := time.UnixMilli(1788220800000).UTC()
	if period.ResetsAt == nil || !period.ResetsAt.Equal(wantReset) {
		t.Errorf("expected reset %s, got %v", wantReset, period.ResetsAt)
	}
}

func TestParseSubscriptionSummaryLoginError(t *testing.T) {
	summaryJSON := []byte(`{
		"code": "ConsoleNeedLogin",
		"message": "You need to log in.",
		"successResponse": false
	}`)

	_, err := parseModelStudioConsoleResponses(summaryJSON, nil, nil, nil, "test")
	if err == nil {
		t.Fatal("expected login error")
	}
	if !errors.Is(err, errModelStudioSession) {
		t.Errorf("expected session error, got: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "ConsoleNeedLogin: You need to log in.") {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestParseSubscriptionSummaryNestedTokenError(t *testing.T) {
	summaryJSON := []byte(`{
		"code": "200",
		"data": {
			"Code": "PostOnlyOrTokenError",
			"Message": "The request has expired. Refresh the page.",
			"Success": false
		},
		"httpStatusCode": "200",
		"successResponse": false
	}`)

	_, err := parseModelStudioConsoleResponses(summaryJSON, nil, nil, nil, "test")
	if !errors.Is(err, errModelStudioSession) {
		t.Fatalf("expected session error, got: %v", err)
	}
}

func TestParseSubscriptionSummaryWorkspaceNotAuthorisedIsNotSessionError(t *testing.T) {
	summaryJSON := []byte(`{
		"code": "200",
		"data": {
			"Code": "BailianGateway.Workspace.NotAuthorised",
			"Message": "Workspace access denied",
			"Success": false
		},
		"httpStatusCode": "200",
		"successResponse": false
	}`)

	_, err := parseModelStudioConsoleResponses(summaryJSON, nil, nil, nil, "test")
	if err == nil {
		t.Fatal("expected API error")
	}
	if errors.Is(err, errModelStudioSession) {
		t.Fatalf("workspace permission error must not be classified as an expired session: %v", err)
	}
}

func TestParseSubscriptionSummaryLoginHTML(t *testing.T) {
	_, err := parseModelStudioConsoleResponses(
		[]byte(`<html><body>Please sign in to Alibaba Cloud</body></html>`),
		nil,
		nil,
		nil,
		"test",
	)
	if !errors.Is(err, errModelStudioSession) {
		t.Fatalf("expected session error, got: %v", err)
	}
}

func TestParsePersonalUsage(t *testing.T) {
	personalJSON := []byte(`{
		"code": "200",
		"success": true,
		"data": {
			"per5HourPercentage": 40,
			"per1WeekPercentage": 15,
			"per5HourResetTime": 1700000000000,
			"per1WeekResetTime": 1700000000000
		}
	}`)

	snapshot, err := parseModelStudioConsoleResponses(nil, nil, nil, personalJSON, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snapshot.Periods) != 2 {
		t.Fatalf("expected 2 periods, got %d", len(snapshot.Periods))
	}

	if snapshot.Periods[0].Name != "5-Hour Window" || snapshot.Periods[0].Utilization != 40 {
		t.Errorf("expected 5-Hour Window with 40%% utilization, got %s %d%%", snapshot.Periods[0].Name, snapshot.Periods[0].Utilization)
	}

	if snapshot.Periods[1].Name != "7-Day Window" || snapshot.Periods[1].Utilization != 15 {
		t.Errorf("expected 7-Day Window with 15%% utilization, got %s %d%%", snapshot.Periods[1].Name, snapshot.Periods[1].Utilization)
	}
}

func TestSignRPC(t *testing.T) {
	params := map[string]string{
		"Action":           "GetTokenPlanAccountDetail",
		"Version":          "2024-01-14",
		"Format":           "JSON",
		"AccessKeyId":      "test-key-id",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"Timestamp":        "2026-08-13T00:00:00Z",
		"SignatureNonce":   "test-nonce",
	}

	sig := signRPC("GET", params, "test-secret")
	if sig == "" {
		t.Error("expected non-empty signature")
	}
}
