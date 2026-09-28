package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// defaultUserType is what the Qoder CLI sends for personal accounts. The job
// token exchange never returns a userType, so there is nothing else to use.
const defaultUserType = "personal_standard"

// storedAuth is the on-disk shape of a Qoder credential.
//
// The PAT is the long-lived secret the user pastes; everything else is either
// generated once (the machine identity) or refreshed from the PAT (the job
// token, which expires roughly daily).
type storedAuth struct {
	Variant string `json:"variant"` // "cn" or "intl"

	// PersonalToken comes from a pasted PAT; DeviceToken comes from device
	// login. A credential has one or the other — each is enough to mint job
	// tokens, which is what actually signs requests.
	PersonalToken string `json:"personalToken,omitempty"`
	DeviceToken   string `json:"deviceToken,omitempty"`
	RefreshToken  string `json:"refreshToken,omitempty"`

	MachineID    string `json:"machineId"`
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`

	UID         string `json:"uid,omitempty"`
	Name        string `json:"name,omitempty"`
	AccessToken string `json:"accessToken,omitempty"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"` // unix ms
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if strings.TrimSpace(sa.PersonalToken) == "" && strings.TrimSpace(sa.DeviceToken) == "" {
		return nil, fmt.Errorf("parse_error: missing personalToken or deviceToken")
	}
	if sa.MachineID == "" {
		sa.MachineID = newUUID()
	}
	if sa.MachineToken == "" {
		sa.MachineToken = newMachineToken()
	}
	if sa.MachineType == "" {
		sa.MachineType = newMachineType()
	}
	return &sa, nil
}

// identity projects the stored credential onto the identity the session signs.
func (sa *storedAuth) identity() identity {
	return identity{
		Name:               sa.Name,
		Aid:                sa.UID,
		UID:                sa.UID,
		UserType:           defaultUserType,
		SecurityOauthToken: sa.AccessToken,
		RefreshToken:       "",
	}
}

// session builds signing material. It requires a job token, so a credential
// that has not been through auth.refresh yet cannot make API calls.
func (sa *storedAuth) session() (*session, error) {
	if sa.AccessToken == "" {
		return nil, fmt.Errorf("no job token: run auth refresh first")
	}
	return newSession(sa.identity(), sa.MachineID, sa.MachineToken, sa.MachineType)
}

// jobTokenRequest is the cosy-encoded envelope around the exchange payload.
type jobTokenRequest struct {
	Payload       string `json:"payload"`
	EncodeVersion string `json:"encodeVersion"`
}

type jobTokenInner struct {
	PersonalToken      string         `json:"personalToken"`
	SecurityOauthToken string         `json:"securityOauthToken"`
	RefreshToken       string         `json:"refreshToken"`
	NeedRefresh        bool           `json:"needRefresh"`
	AuthInfo           map[string]any `json:"authInfo"`
}

// exchangeJobToken trades the PAT for a short-lived job token at the auth
// center. Unlike the API host, the center is authenticated by a static md5
// signature rather than a COSY bearer.
func exchangeJobToken(sa *storedAuth) (*storedAuth, error) {
	inner, err := json.Marshal(jobTokenInner{
		PersonalToken: sa.PersonalToken,
		AuthInfo:      map[string]any{},
	})
	if err != nil {
		return nil, err
	}
	outer, err := json.Marshal(jobTokenRequest{Payload: string(inner), EncodeVersion: "1"})
	if err != nil {
		return nil, err
	}

	date := rfc1123Now()
	req, err := http.NewRequest(http.MethodPost,
		endpointsFor(sa.Variant).center+pathJobToken,
		strings.NewReader(cosyEncode(outer)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("cosy-machinetoken", sa.MachineToken)
	req.Header.Set("cosy-machinetype", sa.MachineType)
	req.Header.Set("login-version", loginVersion)
	req.Header.Set("appcode", appCode)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("cosy-version", cosyVersion)
	req.Header.Set("cosy-clienttype", clientType)
	req.Header.Set("date", date)
	req.Header.Set("signature", centerSign(date))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-machineid", sa.MachineID)
	req.Header.Set("user-agent", upstreamUA)

	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("job token: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("job token rejected: HTTP %d %s", resp.StatusCode, truncate(string(payload), 200))
	}

	var out struct {
		ID                 string `json:"id"`
		Name               string `json:"name"`
		Plan               string `json:"plan"`
		SecurityOauthToken string `json:"securityOauthToken"`
		RefreshToken       string `json:"refreshToken"`
		ExpireTime         any    `json:"expireTime"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("job token decode: %w", err)
	}
	if out.SecurityOauthToken == "" {
		return nil, fmt.Errorf("job token: response carried no securityOauthToken")
	}

	updated := *sa
	updated.UID = out.ID
	updated.Name = out.Name
	updated.AccessToken = out.SecurityOauthToken
	updated.ExpiresAt = parseExpiry(out.ExpireTime)
	return &updated, nil
}

// parseExpiry reads the auth center's expireTime, which arrives either as unix
// milliseconds or as an RFC3339 timestamp.
func parseExpiry(exp any) int64 {
	switch v := exp.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a Qoder credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: toAuthData(sa)})
}

// authLabel tells two credentials of the same provider apart in the host UI.
// The account name is what identifies an account to a human, but it is not
// always available — a freshly parsed file may have nothing but an id — so the
// id is shortened and used as the fallback.
func authLabel(provider, name, id string) string {
	if n := strings.TrimSpace(name); n != "" {
		return provider + " (" + n + ")"
	}
	if i := strings.TrimSpace(id); i != "" {
		return provider + " (" + shortID(i) + ")"
	}
	return provider
}

// shortID trims opaque ids so the label stays readable.
func shortID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:8]
}

// authSlug turns an account identity into something safe to drop into a file
// name and stable across refreshes. CPA names auth entries after FileName and
// falls back to ID, so two credentials of the same provider must not share
// either — otherwise the second one just replaces the first in the UI.
func authSlug(name, id string) string {
	if s := sanitizeSlug(name); s != "" {
		return truncateRunes(s, 24)
	}
	if s := sanitizeSlug(shortID(id)); s != "" {
		return s
	}
	return "account"
}

// sanitizeSlug drops only what is hostile to a file name; letters of any
// script are kept so non-Latin account names stay readable.
func sanitizeSlug(s string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r < 0x20, r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			sb.WriteRune('-')
		case unicode.IsSpace(r):
			sb.WriteRune('-')
		default:
			sb.WriteRune(r)
		}
	}
	return strings.Trim(sb.String(), "-")
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func toAuthData(sa *storedAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	slug := authSlug(sa.Name, sa.UID)
	metadata := map[string]any{"type": providerName, "variant": sa.Variant}
	// Metadata["email"] is CPA's account-identity channel: it feeds both the
	// "email" column and AccountInfo()'s "account". The account list shows that;
	// without it the list falls back to FileName and inherits the provider
	// prefix that belongs in the file detail view instead.
	if identity := firstNonEmpty(sa.Name, shortID(sa.UID)); identity != "" {
		metadata["email"] = identity
	}
	return pluginapi.AuthData{
		Provider: providerName,
		// FileName doubles as the on-disk name (resolveAuthPath) and the name the
		// management UI lists, so it carries the provider prefix the same way the
		// built-in providers do (claude-<email>.json, kimi-<ts>.json). The two
		// cannot be separated — one field, both uses.
		ID:          providerName + "-" + slug,
		FileName:    providerName + "-" + slug + ".json",
		Label:       authLabel("Qoder", sa.Name, sa.UID),
		StorageJSON: storage,
		Metadata:    metadata,
	}
}

// handleRefreshAuth mints a fresh job token. The job token only lives about a
// day, so this is what keeps a credential usable; the host calls it based on
// NextRefreshAfter.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	var updated *storedAuth
	if strings.TrimSpace(sa.PersonalToken) != "" {
		updated, err = exchangeJobToken(sa)
	} else {
		updated, err = refreshDeviceToken(sa)
	}
	if err != nil {
		return nil, err
	}
	next := time.Now().Add(6 * time.Hour)
	if updated.ExpiresAt > 0 {
		next = time.UnixMilli(updated.ExpiresAt).Add(-10 * time.Minute)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             toAuthData(updated),
		NextRefreshAfter: next,
	})
}
