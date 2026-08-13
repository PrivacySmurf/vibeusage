package modelstudio

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
)

type OpenAPIStrategy struct {
	HTTPTimeout float64
}

type Credentials struct {
	AccessKeyID     string
	AccessKeySecret string
}

func LoadCredentials() (Credentials, string) {
	// 1. Stored credentials in vibeusage config
	if data, _ := config.ReadCredential("modelstudio", "openapi"); len(data) > 0 {
		var creds struct {
			AccessKeyID     string `json:"access_key_id"`
			AccessKeySecret string `json:"access_key_secret"`
		}
		if err := json.Unmarshal(data, &creds); err == nil && creds.AccessKeyID != "" && creds.AccessKeySecret != "" {
			return Credentials{AccessKeyID: creds.AccessKeyID, AccessKeySecret: creds.AccessKeySecret}, "vibeusage"
		}
	}

	// 2. Check environment variables
	idEnvs := []string{"ALIBABA_CLOUD_ACCESS_KEY_ID", "DASHSCOPE_ACCESS_KEY_ID", "MODELSTUDIO_ACCESS_KEY_ID"}
	secEnvs := []string{"ALIBABA_CLOUD_ACCESS_KEY_SECRET", "DASHSCOPE_ACCESS_KEY_SECRET", "MODELSTUDIO_ACCESS_KEY_SECRET"}

	for i := range idEnvs {
		id := strings.TrimSpace(os.Getenv(idEnvs[i]))
		sec := strings.TrimSpace(os.Getenv(secEnvs[i]))
		if id != "" && sec != "" {
			return Credentials{AccessKeyID: id, AccessKeySecret: sec}, "env"
		}
	}

	// 3. Single env var formatted as ID:Secret
	for _, envName := range []string{"ALIBABA_CLOUD_API_KEY", "DASHSCOPE_API_KEY"} {
		if val := strings.TrimSpace(os.Getenv(envName)); val != "" && strings.Contains(val, ":") {
			parts := strings.SplitN(val, ":", 2)
			return Credentials{AccessKeyID: strings.TrimSpace(parts[0]), AccessKeySecret: strings.TrimSpace(parts[1])}, "env"
		}
	}

	return Credentials{}, ""
}

func (s *OpenAPIStrategy) IsAvailable() bool {
	creds, _ := LoadCredentials()
	return creds.AccessKeyID != "" && creds.AccessKeySecret != ""
}

func getEndpoint() string {
	if ep := os.Getenv("MODELSTUDIO_ENDPOINT"); ep != "" {
		return strings.TrimRight(ep, "/")
	}
	if ep := os.Getenv("ALIBABA_CLOUD_ENDPOINT"); ep != "" {
		return strings.TrimRight(ep, "/")
	}
	region := os.Getenv("ALIBABA_CLOUD_REGION_ID")
	if region == "" {
		region = os.Getenv("ALIBABA_REGION_ID")
	}
	if region != "" && region != "ap-southeast-1" {
		return fmt.Sprintf("https://modelstudio.%s.aliyuncs.com", region)
	}
	return "https://modelstudio.ap-southeast-1.aliyuncs.com"
}

func (s *OpenAPIStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	creds, source := LoadCredentials()
	if creds.AccessKeyID == "" || creds.AccessKeySecret == "" {
		return fetch.ResultFail("Model Studio: missing AccessKeyID or AccessKeySecret"), nil
	}

	client := httpclient.NewFromConfig(s.HTTPTimeout)
	endpoint := getEndpoint()

	// 1. Fetch Account Details (Token Plan quota)
	accountData, accountErr := s.callRPC(ctx, client, endpoint, "GetTokenPlanAccountDetail", creds)

	// 2. Fetch Seat Details
	seatData, _ := s.callRPC(ctx, client, endpoint, "GetSubscriptionSeatDetails", creds)

	if accountErr != nil && len(seatData) == 0 {
		return fetch.ResultFail(fmt.Sprintf("Model Studio API failed: %v", accountErr)), nil
	}

	snapshot, parseErr := parseModelStudioResponses(accountData, seatData, source)
	if parseErr != nil {
		return fetch.ResultFail(fmt.Sprintf("Model Studio parse error: %v", parseErr)), nil
	}

	return fetch.ResultOK(*snapshot), nil
}

func (s *OpenAPIStrategy) callRPC(ctx context.Context, client *httpclient.Client, endpoint, action string, creds Credentials) ([]byte, error) {
	nonce := generateNonce()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	params := map[string]string{
		"Action":           action,
		"Version":          "2026-02-10",
		"Format":           "JSON",
		"AccessKeyId":      creds.AccessKeyID,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"Timestamp":        now,
		"SignatureNonce":   nonce,
	}

	signature := signRPC("GET", params, creds.AccessKeySecret)
	params["Signature"] = signature

	queryParams := url.Values{}
	for k, v := range params {
		queryParams.Set(k, v)
	}

	reqURL := endpoint + "/?" + queryParams.Encode()
	resp, err := client.GetJSONCtx(ctx, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("HTTP %d: unauthorized / invalid credentials", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(resp.Body))
	}

	return resp.Body, nil
}

func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func percentEncode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

func signRPC(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonicalParts []string
	for _, k := range keys {
		canonicalParts = append(canonicalParts, percentEncode(k)+"="+percentEncode(params[k]))
	}
	canonicalQuery := strings.Join(canonicalParts, "&")

	stringToSign := strings.ToUpper(method) + "&" + percentEncode("/") + "&" + percentEncode(canonicalQuery)

	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
