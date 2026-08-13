package modelstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

type ModelStudio struct{}

func (m ModelStudio) Meta() provider.Metadata {
	return provider.Metadata{
		ID:           "modelstudio",
		Name:         "Model Studio (Bailian)",
		Description:  "Alibaba Cloud Model Studio (Bailian) Team/Enterprise Token Plan",
		Homepage:     "https://modelstudio.console.alibabacloud.com",
		DashboardURL: modelStudioDashboardURL,
	}
}

func (m ModelStudio) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{
		CheckStrategy: true,
		EnvVars: []string{
			"ALIBABA_CLOUD_ACCESS_KEY_ID", "ALIBABA_CLOUD_ACCESS_KEY_SECRET",
			"DASHSCOPE_ACCESS_KEY_ID", "DASHSCOPE_ACCESS_KEY_SECRET",
			"MODELSTUDIO_ACCESS_KEY_ID", "MODELSTUDIO_ACCESS_KEY_SECRET",
			"MODELSTUDIO_COOKIE",
		},
	}
}

func (m ModelStudio) FetchStrategies() []fetch.Strategy {
	timeout := config.Get().Fetch.Timeout
	return []fetch.Strategy{
		&OpenAPIStrategy{HTTPTimeout: timeout},
		&WebConsoleStrategy{HTTPTimeout: timeout},
	}
}

func (m ModelStudio) FetchStatus(_ context.Context) models.ProviderStatus {
	return models.ProviderStatus{Level: models.StatusUnknown}
}

func (m ModelStudio) AcceptCredential(credential string) error {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("credential cannot be empty")
	}

	// 1. Browser Cookie format (contains = or sec_token= or login_aliyunid_ticket=)
	if strings.Contains(credential, "sec_token") || strings.Contains(credential, "login_aliyunid_ticket") || strings.Contains(credential, ";") {
		data, err := json.Marshal(map[string]string{
			"cookie": credential,
			"source": sessionSourceManual,
		})
		if err != nil {
			return fmt.Errorf("marshal modelstudio cookie credential: %w", err)
		}
		return config.WriteCredential("modelstudio", "session", data)
	}

	// 2. AccessKeyID:AccessKeySecret or JSON
	var keyID, keySecret string
	if strings.HasPrefix(credential, "{") {
		var creds struct {
			AccessKeyID     string `json:"access_key_id"`
			AccessKeySecret string `json:"access_key_secret"`
			KeyID           string `json:"key_id"`
			KeySecret       string `json:"key_secret"`
			Cookie          string `json:"cookie"`
		}
		if err := json.Unmarshal([]byte(credential), &creds); err == nil {
			if creds.Cookie != "" {
				data, err := json.Marshal(map[string]string{
					"cookie": creds.Cookie,
					"source": sessionSourceManual,
				})
				if err != nil {
					return fmt.Errorf("marshal modelstudio cookie credential: %w", err)
				}
				return config.WriteCredential("modelstudio", "session", data)
			}
			if creds.AccessKeyID != "" {
				keyID = creds.AccessKeyID
			} else {
				keyID = creds.KeyID
			}
			if creds.AccessKeySecret != "" {
				keySecret = creds.AccessKeySecret
			} else {
				keySecret = creds.KeySecret
			}
		}
	} else if strings.Contains(credential, ":") {
		parts := strings.SplitN(credential, ":", 2)
		keyID = strings.TrimSpace(parts[0])
		keySecret = strings.TrimSpace(parts[1])
	} else if strings.Contains(credential, ",") {
		parts := strings.SplitN(credential, ",", 2)
		keyID = strings.TrimSpace(parts[0])
		keySecret = strings.TrimSpace(parts[1])
	}

	if keyID != "" && keySecret != "" {
		data, err := json.Marshal(map[string]string{
			"access_key_id":     keyID,
			"access_key_secret": keySecret,
		})
		if err != nil {
			return fmt.Errorf("marshal modelstudio credentials: %w", err)
		}
		return config.WriteCredential("modelstudio", "openapi", data)
	}

	// Fallback to storing as cookie/session
	data, err := json.Marshal(map[string]string{
		"cookie": credential,
		"source": sessionSourceManual,
	})
	if err != nil {
		return fmt.Errorf("marshal modelstudio credential: %w", err)
	}
	return config.WriteCredential("modelstudio", "session", data)
}

func (m ModelStudio) Auth() provider.AuthFlow {
	return provider.ManualKeyAuthFlow{
		Instructions: "Model Studio automatically imports and refreshes Singapore Team cookies from signed-in Chrome, Chromium, or Codex in-app browser profiles.\n" +
			"If automatic import is unavailable, paste a Cookie header manually:\n\n" +
			"Option 1: Web Console Cookie (For Team/Enterprise plans without RAM access)\n" +
			"  1. Open the Singapore Team Token Plan page and log in\n" +
			"  2. Open DevTools (F12 or Cmd+Opt+I) -> Network, then reload the page\n" +
			"  3. Filter for api.json and select the GetSubscriptionSummary request\n" +
			"  4. Copy the complete Request Headers -> Cookie value (without the Cookie: label)\n" +
			"  5. Paste that full value below\n\n" +
			"Option 2: OpenAPI Access Key\n" +
			"  Format: <AccessKeyID>:<AccessKeySecret>",
		Placeholder: "paste complete Cookie header value or AccessKeyID:AccessKeySecret",
		Validate:    provider.ValidateNotEmpty,
		ProviderID:  "modelstudio",
		CredType:    "session",
		Save: func(credential string) error {
			return m.AcceptCredential(credential)
		},
	}
}

func init() {
	provider.Register(ModelStudio{})
}
