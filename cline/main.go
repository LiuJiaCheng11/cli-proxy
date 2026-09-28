// Package main implements the cline CLIProxyAPI dynamic plugin.
//
// cline wraps Cline (api.cline.bot) as a cliproxy provider: it performs the
// WorkOS device-code login flow, exchanges the WorkOS token for a Cline
// credential, refreshes access tokens, and forwards OpenAI-compatible chat
// completion requests to the upstream /chat/completions endpoint.
//
// The upstream protocol was taken from the Cline Go proxy
// (https://github.com/lovingfish/cline-proxy); this file is a from-scratch
// reimplementation of its core (login, refresh, model discovery, forwarding)
// shaped for the cliproxy plugin ABI. Built with -buildmode=c-shared and
// exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int cl_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void cl_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName = "cline"

	clineAPIBase = "https://api.cline.bot/api/v1"

	// WorkOS device-code flow that issues the identity Cline accepts.
	workosClientID        = "client_01K3A541FN8TA3EPPHTD2325AR"
	workosDeviceAuthURL   = "https://api.workos.com/user_management/authorize/device"
	workosAuthenticateURL = "https://api.workos.com/user_management/authenticate"

	endpointRegister = clineAPIBase + "/auth/register"
	endpointRefresh  = clineAPIBase + "/auth/refresh"
	endpointChat     = clineAPIBase + "/chat/completions"
	endpointModels   = clineAPIBase + "/ai/cline/recommended-models"

	defaultMaxTokens       = 128000
	defaultReasoningEffort = "high"

	// Refresh this far ahead of the advertised expiry so a request never races
	// the token's own deadline.
	expirySkew = 60 * time.Second

	// The host asks for models on every discovery pass; the catalog barely
	// moves, so serve it from cache rather than per call.
	modelCacheTTL = 10 * time.Minute

	// Cline's upstream accepts the bearer verbatim only when it carries the
	// WorkOS prefix; a bare token is rejected with 401.
	tokenPrefix = "workos:"
)

// passThroughKeys are OpenAI request fields forwarded to Cline untouched.
var passThroughKeys = []string{
	"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
	"temperature", "top_p", "top_k", "stop", "presence_penalty", "frequency_penalty",
	"response_format", "user", "n", "logit_bias", "seed", "logprobs", "top_logprobs",
	"stream_options", "metadata",
}

// modelGroupOrder is the order groups appear in /v1/models, free first. Groups
// the upstream adds later are appended after these, sorted by name.
var modelGroupOrder = []string{"free", "recommended", "clinePass", "clineCloud"}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// -----------------------------------------------------------------------------
// Host calls (async streaming)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured at
// init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.cl_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.cl_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// -----------------------------------------------------------------------------
// RPC dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(clRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: clModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin()
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func clRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          "0.1.0",
			Author:           "lovingfish",
			GitHubRepository: "https://github.com/lovingfish/cline-proxy",
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

// -----------------------------------------------------------------------------
// Model discovery
// -----------------------------------------------------------------------------

// clineModel is one entry of the upstream catalog.
type clineModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Group       string   `json:"-"` // free / recommended / clinePass / clineCloud
}

// ownedBy maps an upstream group onto OpenAI's owned_by so clients can tell
// which models need a subscription.
func ownedBy(group string) string {
	switch group {
	case "free":
		return "cline-free"
	case "recommended":
		return "cline-recommended"
	case "clinePass":
		return "cline-pass"
	case "clineCloud":
		return "cline-cloud"
	}
	return "cline"
}

var (
	modelCacheMu sync.Mutex
	modelCache   []pluginapi.ModelInfo
	modelCacheAt time.Time
)

// clModels returns the live Cline catalog. The upstream endpoint is public —
// it needs no credential and returns the same list for every caller — so it can
// be fetched at registration time, before any account exists. On a failed
// refresh the previous snapshot is served so a transient blip doesn't empty
// /v1/models; before the first successful fetch the list stays empty.
func clModels() []pluginapi.ModelInfo {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	if len(modelCache) > 0 && time.Since(modelCacheAt) < modelCacheTTL {
		return modelCache
	}
	models, err := fetchCatalog()
	if err != nil {
		return modelCache
	}
	modelCache, modelCacheAt = models, time.Now()
	return modelCache
}

// fetchCatalog reads GET /ai/cline/recommended-models and maps each entry to the
// host's ModelInfo. The response is a map of group name to model list.
func fetchCatalog() ([]pluginapi.ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, endpointModels, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("models upstream %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var raw map[string][]clineModel
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("models decode: %w", err)
	}

	// Known groups first in modelGroupOrder; anything the upstream adds later
	// goes after them, sorted so the order stays stable across fetches.
	groups := make([]string, 0, len(raw))
	groups = append(groups, modelGroupOrder...)
	var extra []string
	for g := range raw {
		known := false
		for _, k := range modelGroupOrder {
			if g == k {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, g)
		}
	}
	sort.Strings(extra)
	groups = append(groups, extra...)

	models := make([]pluginapi.ModelInfo, 0, 32)
	seen := make(map[string]bool)
	for _, g := range groups {
		for _, m := range raw[g] {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			models = append(models, pluginapi.ModelInfo{
				ID:                         m.ID,
				Object:                     "model",
				OwnedBy:                    ownedBy(g),
				DisplayName:                firstNonEmpty(m.Name, m.ID),
				Name:                       m.ID,
				Description:                m.Description,
				SupportedGenerationMethods: []string{"chat"},
				SupportedInputModalities:   []string{"text"},
				UserDefined:                true,
			})
		}
	}
	// Upstream publishes no context limits, so ContextLength stays zero rather
	// than an invented number.
	return models, nil
}

// -----------------------------------------------------------------------------
// Auth data shapes (matches persisted cline.json)
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a Cline credential.
type storedAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // unix ms
	Email        string `json:"email,omitempty"`
	UID          string `json:"uid,omitempty"`
}

type deviceAuthResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type workosTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type registerResp struct {
	Data struct {
		ID           string `json:"id"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
		UserInfo     *struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	} `json:"data"`
}

type refreshResp struct {
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
	} `json:"data"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if sa.RefreshToken == "" {
		return nil, fmt.Errorf("parse_error: missing refreshToken")
	}
	return &sa, nil
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
	slug := authSlug(sa.Email, sa.UID)
	metadata := map[string]any{"type": providerName}
	// CPA renders whatever sits under "email" as the account's identity column.
	if sa.Email != "" {
		metadata["email"] = sa.Email
	}
	return pluginapi.AuthData{
		Provider: providerName,
		// FileName doubles as the on-disk name (resolveAuthPath) and the name the
		// management UI lists, so it carries the provider prefix the same way the
		// built-in providers do (claude-<email>.json, kimi-<ts>.json). The two
		// cannot be separated — one field, both uses.
		ID:          providerName + "-" + slug,
		FileName:    providerName + "-" + slug + ".json",
		Label:       authLabel("Cline", sa.Email, sa.UID),
		StorageJSON: storage,
		Metadata:    metadata,
	}
}

// parseExpiry reads Cline's expiresAt, which arrives either as a unix
// millisecond number or as an RFC3339 timestamp depending on the endpoint.
func parseExpiry(exp any) int64 {
	switch v := exp.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UnixMilli()
		}
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
		}
	})
	return sharedClient
}

// postForm sends an application/x-www-form-urlencoded request and returns the
// raw response. Used by the WorkOS endpoints, which are not JSON APIs.
func postForm(fullURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return sharedHTTPClient().Do(req)
}

func postJSON(fullURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return sharedHTTPClient().Do(req)
}

// upstreamHeaders builds the headers Cline expects on a chat request. The
// bearer must carry the WorkOS prefix or the upstream answers 401.
func upstreamHeaders(req *http.Request, accessToken, sessionID string) {
	req.Header.Set("Authorization", "Bearer "+tokenPrefix+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Task-ID", sessionID)
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
		// Not a Cline credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    toAuthData(sa),
	})
}

// handleStartLogin opens a WorkOS device authorization. The state handed back
// is the device code: the host echoes it on every poll, so no server-side
// session is needed. ExpiresAt comes from WorkOS and the host enforces it, so
// the plugin does not need its own deadline.
func handleStartLogin() ([]byte, error) {
	resp, err := postForm(workosDeviceAuthURL, url.Values{"client_id": {workosClientID}})
	if err != nil {
		return nil, fmt.Errorf("workos device auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("workos device auth failed: %d %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var d deviceAuthResp
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("workos device auth decode: %w", err)
	}
	// verification_uri_complete already carries the user code, so the user only
	// has to open it and approve — no manual code entry.
	authURL := firstNonEmpty(d.VerificationURIComplete, d.VerificationURI)
	if authURL == "" || d.DeviceCode == "" {
		return nil, fmt.Errorf("workos device auth: missing device_code or verification uri")
	}
	expiresIn := d.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       authURL,
		State:     d.DeviceCode,
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).UTC(),
	})
}

// handlePollLogin performs a single device-code probe. The host drives the
// polling cadence, so one RPC does exactly one attempt and reports back.
func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	deviceCode := strings.TrimSpace(req.State)
	if deviceCode == "" {
		return nil, fmt.Errorf("poll: empty device code")
	}

	resp, err := postForm(workosAuthenticateURL, url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {workosClientID},
	})
	if err != nil {
		return nil, fmt.Errorf("workos poll: %w", err)
	}
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var tok workosTokenResp
	_ = json.Unmarshal(payload, &tok)

	if resp.StatusCode != http.StatusOK {
		// authorization_pending / slow_down mean "not yet"; anything else is fatal.
		if tok.Error == "authorization_pending" || tok.Error == "slow_down" {
			return okEnvelope(pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "waiting for authorization",
			})
		}
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: firstNonEmpty(tok.ErrorDesc, tok.Error, truncate(string(payload), 200)),
		})
	}
	if tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for authorization",
		})
	}

	// Trade the WorkOS identity for a Cline credential. It is the Cline refresh
	// token that gets persisted — the WorkOS one is spent here.
	sa, err := registerWithCline(tok.AccessToken, tok.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cline register: %w", err)
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func registerWithCline(workosAccess, workosRefresh string) (*storedAuth, error) {
	resp, err := postJSON(endpointRegister, map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register failed: %d %s", resp.StatusCode, truncate(string(payload), 200))
	}
	var r registerResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("register decode: %w", err)
	}
	if r.Data.RefreshToken == "" {
		return nil, fmt.Errorf("register: missing refreshToken")
	}
	sa := &storedAuth{
		AccessToken:  r.Data.AccessToken,
		RefreshToken: r.Data.RefreshToken,
		ExpiresAt:    parseExpiry(r.Data.ExpiresAt),
		UID:          r.Data.ID,
	}
	if r.Data.UserInfo != nil {
		sa.Email = r.Data.UserInfo.Email
	}
	return sa, nil
}

// handleRefreshAuth exchanges the stored refresh token for a fresh pair.
// Cline rotates refresh tokens on every refresh and invalidates the old one
// immediately, so the new token must go back in StorageJSON — CPA persists
// what we return here.
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	resp, err := postJSON(endpointRefresh, map[string]string{
		"refreshToken": sa.RefreshToken,
		"grantType":    "refresh_token",
	})
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		status := resp.StatusCode
		// Only an explicit rejection means the credential is dead; a 5xx or 429
		// is transient and must not discard an account that could recover.
		permanent := status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden
		kind := "temporary"
		if permanent {
			kind = "permanent, re-login required"
		}
		return nil, fmt.Errorf("refresh rejected (%s) HTTP %d: %s", kind, status, truncate(string(payload), 200))
	}
	var r refreshResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("refresh decode: %w", err)
	}
	if r.Data.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.AccessToken = r.Data.AccessToken
	if r.Data.RefreshToken != "" {
		sa.RefreshToken = r.Data.RefreshToken
	}
	expiresAt := parseExpiry(r.Data.ExpiresAt)
	if expiresAt > 0 {
		sa.ExpiresAt = expiresAt
	}
	next := time.Now()
	if sa.ExpiresAt > 0 {
		next = time.UnixMilli(sa.ExpiresAt).Add(-expirySkew)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             toAuthData(sa),
		NextRefreshAfter: next,
	})
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

// buildUpstreamBody reshapes an OpenAI chat request into Cline's body. It also
// returns the session id it generated, which the upstream wants echoed in the
// X-Task-ID header.
func buildUpstreamBody(payload []byte, stream bool) ([]byte, string) {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload, ""
	}
	sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())

	maxTokens := int64(defaultMaxTokens)
	if v, ok := obj["max_tokens"].(float64); ok {
		maxTokens = int64(v)
	} else if v, ok := obj["max_completion_tokens"].(float64); ok {
		maxTokens = int64(v)
	}

	out := map[string]any{
		"max_tokens":       maxTokens,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
	}
	if model, ok := obj["model"].(string); ok && model != "" {
		out["model"] = model
	}
	if msgs, ok := obj["messages"]; ok {
		out["messages"] = msgs
	}
	if stream {
		out["stream"] = true
	}
	if re, ok := obj["reasoning_effort"].(string); ok && re != "" {
		out["reasoning_effort"] = re
	} else if re, ok := obj["reasoningEffort"].(string); ok && re != "" {
		out["reasoning_effort"] = re
	}
	for _, k := range passThroughKeys {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return payload, ""
	}
	return body, sessionID
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	source := req.Payload
	if len(source) == 0 {
		source = req.OriginalRequest
	}
	// Cline's upstream answers non-streaming requests directly, so unlike the
	// CodeBuddy path there is no need to force a stream and re-aggregate.
	body, sessionID := buildUpstreamBody(source, false)
	httpReq, err := http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstreamHeaders(httpReq, sa.AccessToken, sessionID)
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	body, sessionID := buildUpstreamBody(body, true)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	httpReq, err := http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))
	if err != nil {
		streamEmitError(req.StreamID, err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	upstreamHeaders(httpReq, sa.AccessToken, sessionID)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		chunks, errCollect := collectUpstreamStream(httpReq, sseFramed)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	go pumpUpstreamStream(httpReq, req.StreamID, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream.
func pumpUpstreamStream(httpReq *http.Request, streamID string, sseFramed bool) {
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		streamClose(streamID)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate(string(errPayload), 200)))
		streamClose(streamID)
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			break
		}
	}
	streamClose(streamID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice.
func collectUpstreamStream(httpReq *http.Request, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(errPayload), 200))
	}
	return aggregateSSE(resp.Body, sseFramed), nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSE reads an upstream SSE stream and emits one chunk per data event.
// Empty-valued delta fields are stripped and the trailing [DONE] is dropped
// (the host appends its own stream terminator). When sseFramed is true each
// payload is emitted as a "data: " line for cross-format translators; otherwise
// the payload is the raw JSON object and the host chat-completions writer adds
// the framing itself.
func aggregateSSE(r io.Reader, sseFramed bool) []pluginapi.ExecutorStreamChunk {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	return chunks
}

// cleanChunkJSON strips empty-valued fields (null/""/[]/{}) from choice deltas
// so strict clients don't trip on {"function_call":null,"tool_calls":[]}.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// -----------------------------------------------------------------------------
// envelope helpers
// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
