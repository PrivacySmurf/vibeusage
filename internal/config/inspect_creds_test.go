package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCredsScript_RedactsSecretsAndPreservesMetadata(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "inspect-creds.py"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}

	tmpDir := t.TempDir()
	credFile := filepath.Join(tmpDir, "credentials.json")
	sample := `{
  "antigravity": {
    "oauth": {
      "access_token": "ya29.super-secret-access-token-value",
      "refresh_token": "1//super-secret-refresh-token-value",
      "expires_at": "2026-11-03T00:00:00Z",
      "vibeusage_owned": true
    }
  },
  "venice": {
    "apikey": {
      "api_key": "VENICE_INFERENCE_KEY_secret1234567890"
    }
  },
  "refs": {
    "VENICE_AI_API_KEY": "VENICE_INFERENCE_KEY_secret1234567890"
  }
}`
	if err := os.WriteFile(credFile, []byte(sample), 0o600); err != nil {
		t.Fatalf("write sample credentials: %v", err)
	}

	out, err := exec.Command(python, scriptPath, credFile).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect-creds.py failed: %v\n%s", err, string(out))
	}
	text := string(out)

	for _, secret := range []string{
		"ya29.super-secret-access-token-value",
		"1//super-secret-refresh-token-value",
		"VENICE_INFERENCE_KEY_secret1234567890",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("output leaked secret %q:\n%s", secret, text)
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal script output: %v\n%s", err, text)
	}

	ag, _ := parsed["antigravity"].(map[string]any)
	oauthMap, _ := ag["oauth"].(map[string]any)
	if got := oauthMap["expires_at"]; got != "2026-11-03T00:00:00Z" {
		t.Errorf("expires_at = %v, want 2026-11-03T00:00:00Z", got)
	}
	if got := oauthMap["access_token"]; got != "<redacted str len=36>" {
		t.Errorf("access_token = %v, want <redacted str len=36>", got)
	}

	refsMap, _ := parsed["refs"].(map[string]any)
	if got := refsMap["VENICE_AI_API_KEY"]; got != "<redacted str len=37>" {
		t.Errorf("refs.VENICE_AI_API_KEY = %v, want <redacted str len=37>", got)
	}
}
