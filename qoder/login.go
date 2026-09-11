package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Qoder's real login is a PKCE device flow: mint a machine id, a nonce and a
// verifier, send the user to a page carrying the verifier's SHA-256 challenge,
// then poll with the verifier itself until the signed-in account's token lands.

// deviceState is packed into the login state the host echoes back on every
// poll, so no server-side session is needed. The host rejects states longer
// than 128 characters; this one lands around 100.
type deviceState struct {
	MachineID string
	Verifier  string
	Nonce     string
}

func (d deviceState) encode() string {
	return strings.Join([]string{d.MachineID, d.Verifier, d.Nonce}, ".")
}

func decodeDeviceState(state string) (deviceState, bool) {
	parts := strings.Split(strings.TrimSpace(state), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return deviceState{}, false
	}
	return deviceState{MachineID: parts[0], Verifier: parts[1], Nonce: parts[2]}, true
}

func randomURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("qoder: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// pkceChallenge is base64url(SHA256(verifier)), the standard S256 transform.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func handleStartLogin() ([]byte, error) {
	ep := endpointsFor(defaultLoginVariant)
	state := deviceState{
		MachineID: newUUID(),
		Verifier:  randomURLSafe(32),
		Nonce:     randomURLSafe(16),
	}
	query := url.Values{}
	query.Set("machine_id", state.MachineID)
	query.Set("challenge", pkceChallenge(state.Verifier))
	query.Set("challenge_method", "S256")
	query.Set("nonce", state.Nonce)

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       ep.account + pathDeviceSelect + "?" + query.Encode(),
		State:     state.encode(),
		ExpiresAt: time.Now().Add(deviceLoginTTL).UTC(),
		Metadata:  map[string]any{"variant": defaultLoginVariant},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state, ok := decodeDeviceState(req.State)
	if !ok {
		return nil, fmt.Errorf("poll: malformed device state")
	}
	ep := endpointsFor(defaultLoginVariant)

	query := url.Values{}
	query.Set("machine_id", state.MachineID)
	query.Set("verifier", state.Verifier)
	query.Set("nonce", state.Nonce)
	pollURL := ep.openapi + pathDevicePoll + "?" + query.Encode()

	payload, status, err := getPlain(pollURL)
	if err != nil {
		return nil, err
	}
	// While the user has not finished signing in the upstream simply has no
	// token for this device yet.
	if status == http.StatusNotFound || status == http.StatusAccepted || status == http.StatusNoContent {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待浏览器中完成登录",
		})
	}
	if status != http.StatusOK {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("deviceToken/poll HTTP %d: %s", status, truncate(string(payload), 300)),
		})
	}

	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("deviceToken/poll decode: %w", err)
	}
	// The device token is the credential. Qoder only honours dt- tokens for
	// real API calls, and every jobToken/exchange shape tried against it
	// returns 400 — so there is nothing to exchange it for, it signs requests
	// directly (verified: model list returns 14 chat models).
	deviceToken := firstString(out, "token", "device_token", "deviceToken")
	if !strings.HasPrefix(deviceToken, "dt-") {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: fmt.Sprintf("deviceToken/poll 未返回 dt- 设备令牌，响应=%s", summarizeJSON(out)),
		})
	}

	sa := &storedAuth{
		Variant:      defaultLoginVariant,
		MachineID:    state.MachineID,
		MachineToken: newMachineToken(),
		MachineType:  newMachineType(),
		DeviceToken:  deviceToken,
		AccessToken:  deviceToken,
	}
	adoptTokenFields(sa, out)
	// The poll carries user_id, so the uid is known; the display name is not.
	// Non-fatal when it fails — the label is cosmetic.
	if sa.Name == "" {
		_ = fetchUserInfo(sa)
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

// fetchUserInfo fills in uid/name for a freshly minted access token. Both the
// device token (dt-) and the job token (jt-) are accepted as a bearer here.
func fetchUserInfo(sa *storedAuth) error {
	req, err := http.NewRequest(http.MethodGet, endpointsFor(sa.Variant).openapi+pathUserInfo, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+sa.AccessToken)
	req.Header.Set("accept", "application/json")
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("userinfo HTTP %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return fmt.Errorf("userinfo decode: %w", err)
	}
	sa.UID = firstString(out, "id", "uid", "userId")
	sa.Name = firstString(out, "name", "userName", "nickname")
	if sa.UID == "" {
		return fmt.Errorf("userinfo 未返回 id，响应=%s", summarizeJSON(out))
	}
	return nil
}

// adoptTokenFields copies whichever identity/refresh fields the upstream sent.
// Two different endpoints return these and the exact key names differ between
// them, so the known spellings are tried in order.
func adoptTokenFields(sa *storedAuth, out map[string]any) {
	if v := firstString(out, "refreshToken", "refresh_token"); v != "" {
		sa.RefreshToken = v
	}
	// user_id first: a device poll response also carries an "id", but that is
	// the device-session id, not the account.
	if v := firstString(out, "user_id", "uid", "userId", "id"); v != "" {
		sa.UID = v
	}
	if v := firstString(out, "name", "userName", "nickname"); v != "" {
		sa.Name = v
	}
	if sa.ExpiresAt == 0 {
		// Absolute timestamps only. The exchange response also carries
		// expires_in, but that is a duration in milliseconds — reading it as an
		// epoch would put the expiry in 1970.
		if v, ok := numericField(out, "expireTime", "expiresAt", "expires_at"); ok {
			sa.ExpiresAt = v
		}
	}
}

// refreshDeviceToken renews a credential that came from device login. The
// upstream rotates both tokens on every call, so the new pair replaces the old
// one wholesale — the previous drt- stops working immediately.
func refreshDeviceToken(sa *storedAuth) (*storedAuth, error) {
	if sa.RefreshToken == "" {
		return nil, fmt.Errorf("refresh: device credential has no refresh token, re-login required")
	}
	payload, status, err := postJSONPlain(endpointsFor(sa.Variant).openapi+pathDeviceRefresh,
		map[string]string{"refresh_token": sa.RefreshToken})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("deviceToken/refresh HTTP %d: %s", status, truncate(string(payload), 300))
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("deviceToken/refresh decode: %w", err)
	}
	deviceToken := firstString(out, "device_token", "deviceToken", "token")
	if deviceToken == "" {
		return nil, fmt.Errorf("deviceToken/refresh 未返回 device token，响应=%s", summarizeJSON(out))
	}
	updated := *sa
	updated.DeviceToken = deviceToken
	updated.AccessToken = deviceToken
	adoptTokenFields(&updated, out)
	return &updated, nil
}

// -----------------------------------------------------------------------------
// Plain (unsigned) HTTP — the device endpoints are not COSY-signed
// -----------------------------------------------------------------------------

func getPlain(fullURL string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return payload, resp.StatusCode, nil
}

func postJSONPlain(fullURL string, body any) ([]byte, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, fullURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return payload, resp.StatusCode, nil
}

func firstString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func numericField(obj map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case float64:
			return int64(v), true
		case string:
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.UnixMilli(), true
			}
		}
	}
	return 0, false
}

// sortedKeys lists a response's fields so an unexpected shape is diagnosable
// from the message the user sees rather than only from server logs.
func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// summarizeJSON renders a response as "key=shape" pairs. Values are masked to
// lengths and prefixes so the message can be pasted into an issue or a chat
// without leaking tokens.
func summarizeJSON(obj map[string]any) string {
	parts := make([]string, 0, len(obj))
	for _, k := range sortedKeys(obj) {
		parts = append(parts, k+"="+describeJSON(obj[k]))
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " ")
}

func describeJSON(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		if len(x) > 8 {
			return fmt.Sprintf("str(len=%d,prefix=%q)", len(x), x[:3])
		}
		return fmt.Sprintf("str(len=%d)", len(x))
	case float64:
		return fmt.Sprintf("num(%v)", x)
	case bool:
		return fmt.Sprintf("bool(%v)", x)
	case []any:
		return fmt.Sprintf("array(%d)", len(x))
	case map[string]any:
		return "object{" + strings.Join(sortedKeys(x), ",") + "}"
	}
	return fmt.Sprintf("%T", v)
}

func stringMapToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
