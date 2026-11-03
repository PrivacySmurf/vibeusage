package venice

import (
	"encoding/json"
	"testing"
)

func TestRateLimitsResponse_UnmarshalFullResponse(t *testing.T) {
	raw := `{
		"data": {
			"accessPermitted": true,
			"apiTier": {
				"id": "paid",
				"isCharged": true
			},
			"balances": {
				"USD": 9.12196523,
				"DIEM": 15.5,
				"BUNDLED_CREDITS": 2.25
			},
			"keyExpiration": null,
			"nextEpochBegins": "2026-11-04T00:00:00.000Z",
			"rateLimits": [
				{
					"apiModelId": "zai-org-glm-5-1",
					"rateLimits": [
						{"amount": 100, "type": "RPM"},
						{"amount": 2000000, "type": "TPM"}
					]
				}
			]
		}
	}`

	var resp RateLimitsResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if !resp.Data.AccessPermitted {
		t.Error("accessPermitted = false, want true")
	}
	if resp.Data.APITier.ID != "paid" || !resp.Data.APITier.IsCharged {
		t.Errorf("apiTier = %+v, want id=paid isCharged=true", resp.Data.APITier)
	}
	if resp.Data.Balances.USD != 9.12196523 {
		t.Errorf("balances.USD = %f, want 9.12196523", resp.Data.Balances.USD)
	}
	if resp.Data.Balances.DIEM != 15.5 {
		t.Errorf("balances.DIEM = %f, want 15.5", resp.Data.Balances.DIEM)
	}
	if resp.Data.Balances.BundledCredits != 2.25 {
		t.Errorf("balances.BUNDLED_CREDITS = %f, want 2.25", resp.Data.Balances.BundledCredits)
	}
	if resp.Data.KeyExpiration != nil {
		t.Errorf("keyExpiration = %v, want nil", resp.Data.KeyExpiration)
	}
	if resp.Data.NextEpochBegins == nil || *resp.Data.NextEpochBegins != "2026-11-04T00:00:00.000Z" {
		t.Errorf("nextEpochBegins = %v, want 2026-11-04T00:00:00.000Z", resp.Data.NextEpochBegins)
	}
	if len(resp.Data.RateLimits) != 1 || resp.Data.RateLimits[0].APIModelID != "zai-org-glm-5-1" {
		t.Errorf("rateLimits = %+v, want 1 item for zai-org-glm-5-1", resp.Data.RateLimits)
	}
}
