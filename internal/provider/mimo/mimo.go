package mimo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

type Mimo struct{}

func (m Mimo) Meta() provider.Metadata {
	return provider.Metadata{
		ID:           "mimo",
		Name:         "Xiaomi MiMo",
		Description:  "Xiaomi MiMo AI platform balance and token plans",
		Homepage:     "https://platform.xiaomimimo.com",
		DashboardURL: "https://platform.xiaomimimo.com/#/console/balance",
	}
}

func (m Mimo) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{
		CheckStrategy: true,
		EnvVars:       []string{"MIMO_COOKIE"},
	}
}

func (m Mimo) FetchStrategies() []fetch.Strategy {
	timeout := config.Get().Fetch.Timeout
	return []fetch.Strategy{
		&WebConsoleStrategy{HTTPTimeout: timeout},
	}
}

func (m Mimo) FetchStatus(_ context.Context) models.ProviderStatus {
	return models.ProviderStatus{Level: models.StatusUnknown}
}

func validateMimoCookie(cookie string) error {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return errors.New("cookie cannot be empty")
	}
	// Check for expected cookies or at least non-empty string
	if !strings.Contains(cookie, "api-platform_serviceToken") && !strings.Contains(cookie, "userId") && !strings.Contains(cookie, "=") {
		return errors.New("cookie must contain 'api-platform_serviceToken' and 'userId' or valid cookie key-value pairs")
	}
	return nil
}

// Auth returns the manual cookie flow for Xiaomi MiMo.
func (m Mimo) Auth() provider.AuthFlow {
	return provider.ManualKeyAuthFlow{
		Instructions: "Authenticate Xiaomi MiMo using your web console cookie:\n" +
			"  1. Log in to https://platform.xiaomimimo.com/#/console/balance in your browser\n" +
			"  2. Open Developer Tools (F12 or Inspect) -> Network tab\n" +
			"  3. Refresh the page or click 'Balance'\n" +
			"  4. Select any request to platform.xiaomimimo.com and copy the 'Cookie:' request header value\n" +
			"     (must include 'api-platform_serviceToken' and 'userId')",
		Placeholder: "api-platform_serviceToken=...; userId=...",
		Validate:    validateMimoCookie,
		ProviderID:  "mimo",
		CredType:    "session",
		JSONKey:     "cookie",
		Save: func(value string) error {
			return m.AcceptCredential(value)
		},
	}
}

// AcceptCredential implements provider.CredentialAcceptor.
func (m Mimo) AcceptCredential(credential string) error {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("credential cannot be empty")
	}

	cookieVal := credential
	if strings.HasPrefix(credential, "{") {
		var payload struct {
			Cookie string `json:"cookie"`
		}
		if err := json.Unmarshal([]byte(credential), &payload); err == nil && payload.Cookie != "" {
			cookieVal = strings.TrimSpace(payload.Cookie)
		}
	}

	if err := validateMimoCookie(cookieVal); err != nil {
		return err
	}

	data, err := json.Marshal(map[string]string{
		"cookie": cookieVal,
	})
	if err != nil {
		return fmt.Errorf("marshal mimo cookie credential: %w", err)
	}

	return config.WriteCredential("mimo", "session", data)
}

func init() {
	provider.Register(Mimo{})
}
