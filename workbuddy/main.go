// Package main implements the workbuddy CLIProxyAPI dynamic plugin.
//
// workbuddy wraps Tencent CodeBuddy (copilot.tencent.com) as a cliproxy
// provider: it performs the CodeBuddy web login flow, refreshes access
// tokens, and forwards OpenAI-compatible chat completion requests to the
// upstream /v2/chat/completions endpoint.
//
// This file is a clean-room reimplementation reconstructed from the public
// workbuddy.so binary (symbol table, string constants and RPC shape) published
// by Sliverkiss. Original credit for the workbuddy plugin goes to Sliverkiss;
// see https://github.com/Sliverkiss/cpa-plugin. Built with -buildmode=c-shared
// and exports the cliproxy C ABI entry points.
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
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
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
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/LiuJiaCheng11/cli-proxy/internal/channelgate"
	"github.com/LiuJiaCheng11/cli-proxy/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName  = "workbuddy"
	authFileName  = "workbuddy.json"
	upstreamBase  = "https://copilot.tencent.com"
	clientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer = "https://www.codebuddy.cn"

	endpointAuthState    = upstreamBase + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct    = upstreamBase + "/v2/plugin/login/account?state="
	endpointAuthToken    = upstreamBase + "/v2/plugin/auth/token?state="
	endpointTokenRefresh = upstreamBase + "/v2/plugin/auth/token/refresh"
	endpointChat         = upstreamBase + "/v2/chat/completions"
	endpointConfig       = upstreamBase + "/v3/config"

	loginTTL = 5 * time.Minute

	// The gateway must see Origin/Referer or it answers /v3/config with 400.
	// X-User-Id switches /v3/config from its anonymous shape (models:null) to
	// the full catalog, and its value is never validated: a random UUID yields a
	// catalog identical to a logged-in account's. So the catalog is account
	// independent and can be fetched before login, with no credential.
	configProbeUID = "00000000-0000-0000-0000-000000000000"

	// The host asks for models on every discovery pass; the catalog barely
	// moves, so serve it from cache rather than per call.
	modelCacheTTL = 10 * time.Minute
)

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	client  *http.Client
	expires time.Time
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
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

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
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
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
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
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: wbModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
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
// Registration & models
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

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          "0.1.0",
			Author:           "lovingfish (clean-room rebuild; original workbuddy by Sliverkiss)",
			GitHubRepository: "https://github.com/lovingfish/workbuddy-cliproxy",
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

// cloudModel is the subset of a /v3/config model entry we map to ModelInfo.
type cloudModel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	MaxInputTokens    int64  `json:"maxInputTokens"`
	MaxOutputTokens   int64  `json:"maxOutputTokens"`
	SupportsImages    bool   `json:"supportsImages"`
	SupportsReasoning bool   `json:"supportsReasoning"`
}

var (
	modelCacheMu sync.Mutex
	modelCache   []pluginapi.ModelInfo
	modelCacheAt time.Time
)

// wbModels returns the live CodeBuddy model catalog from /v3/config. Nothing is
// hardcoded here: what the client sees at GET /v1/models is exactly what the
// upstream publishes. On a failed refresh the previous snapshot is served so a
// transient upstream blip doesn't empty the catalog; before the first
// successful fetch there is nothing to serve and the list stays empty.
func wbModels() []pluginapi.ModelInfo {
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

// fetchCatalog reads GET /v3/config and maps each entry to the host's ModelInfo.
func fetchCatalog() ([]pluginapi.ModelInfo, error) {
	headers := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("X-User-Id", configProbeUID)
		r.Header.Set("X-Product", "SaaS")
	}
	data, _, err := doJSON(sharedHTTPClient(), http.MethodGet, endpointConfig, headers, nil)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Models []cloudModel `json:"models"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	models := make([]pluginapi.ModelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		// Image-generation entries advertise zero context and cannot serve a
		// chat completion, so they are not published as chat models.
		if m.ID == "" || m.MaxInputTokens <= 0 {
			continue
		}
		modalities := []string{"text"}
		if m.SupportsImages {
			modalities = append(modalities, "image")
		}
		models = append(models, pluginapi.ModelInfo{
			ID:                         m.ID,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                firstNonEmpty(m.Name, m.ID),
			Name:                       m.ID,
			SupportedGenerationMethods: []string{"chat"},
			SupportedInputModalities:   modalities,
			ContextLength:              m.MaxInputTokens,
			InputTokenLimit:            m.MaxInputTokens,
			MaxCompletionTokens:        m.MaxOutputTokens,
			OutputTokenLimit:           m.MaxOutputTokens,
			UserDefined:                true,
		})
	}
	return models, nil
}

// -----------------------------------------------------------------------------
// Auth data shapes (matches persisted workbuddy.json)
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a workbuddy credential.
type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by every CodeBuddy API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &sa, nil
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
			Jar: jar,
		}
	})
	return sharedClient
}

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: sharedHTTPClient().Transport,
		Jar:       jar,
	}
}

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// identityOf describes the outbound client this credential should present.
func identityOf(sa *storedAuth) wire.Identity {
	return wire.Identity{
		Region:     wire.RegionCN,
		UID:        sa.Account.UID,
		AppVersion: "5.8.3",
	}
}

// backendHeaders applies auth-derived headers to a chat completion request.
// Empty fields are signalled via the X-No-* convention used by CodeBuddy.
//
// The attribution and device headers exist because an official client describes
// ITSELF the same way on every request; a proxy that says `X-Product: SaaS` in
// one header and nothing at all in the rest is the cheapest shape for a static
// rule to pick out. Measured 2026-11-02: the domestic gateway answers 200 either
// way for a body it already accepts, so this is hardening, not a gate.
func backendHeaders(req *http.Request, sa *storedAuth) {
	commonHeaders(req)
	identity := identityOf(sa)
	wire.ApplyAttributionHeaders(req, identity)
	wire.ApplyDeviceHeaders(req, identity)
	wire.ApplyConversationHeaders(req)
	if sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if sa.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
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
		// Not a workbuddy credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    toAuthData(sa),
	})
}

// authLabel tells two credentials of the same provider apart in the host UI.
// The account name is what identifies an account to a human, but it is not
// always available — the account lookup during login can fail — so the id is
// shortened and used as the fallback.
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
	slug := authSlug(sa.Account.Nickname, sa.Account.UID)
	metadata := map[string]any{"type": providerName}
	// Metadata["email"] is CPA's account-identity channel: it feeds both the
	// "email" column and AccountInfo()'s "account". The account list shows that;
	// without it the list falls back to FileName and inherits the provider
	// prefix that belongs in the file detail view instead.
	if identity := firstNonEmpty(sa.Account.Nickname, shortID(sa.Account.UID)); identity != "" {
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
		Label:       authLabel("WorkBuddy", sa.Account.Nickname, sa.Account.UID),
		StorageJSON: storage,
		Metadata:    metadata,
	}
}

func handleStartLogin(raw []byte) ([]byte, error) {
	client := newLoginClient()
	data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	loginStates.Store(st.State, &loginCtx{client: client, expires: time.Now().Add(loginTTL)})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns code 11217 ("login ing") while pending, and code 0 with the
	// token bundle once complete. login/account sits behind the openresty gateway
	// and is rejected (401) until login finishes, so probe token first and only
	// fetch account once we hold a bearer.
	tokRaw, _, errTok := doJSON(lc.client, http.MethodGet, endpointAuthToken+state, nil, nil)
	if errTok != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	var acct accountData
	acctHeaders := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(lc.client, http.MethodGet, endpointLoginAcct+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       tok.Domain,
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	headers := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", providerName)
	}
	data, status, err := doJSON(sharedHTTPClient(), http.MethodPost, endpointTokenRefresh, headers, nil)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthData(sa)})
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// Request preparation and the channel-gate retry ladder
// -----------------------------------------------------------------------------

// prepareBody turns the host's payload into the shape the gateway accepts and
// neutralises the content fingerprints it refuses.
//
// The order is load-bearing. PrepareBody fixes the STRUCTURE (stream flag,
// developer role, system head, tool_choice) — a request with the wrong shape is
// refused before its content is ever read. RewritePhrasesInBody then applies the
// measured phrase table, which is the fix for a real DeepSeek Harness prompt:
// measured 2026-11-02 on copilot.tencent.com, the same body with every other
// field held fixed answers 400 / 11128 until the WHOLE table is applied — the
// three identity sentences alone, the @-path paragraph alone, a neutral head
// alone and a zero-width perturbation alone all still answer 400.
func prepareBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	prepared := channelgate.PrepareBody(src)
	rewritten, _ := channelgate.RewritePhrasesInBody(prepared)
	return applyModelTweaks(rewritten)
}

// gateRung is one rescue step for a request the gateway refused with 11128
// after the phrase table had already been applied.
type gateRung struct {
	label string
	apply func([]byte) ([]byte, bool)
}

// gateLadder is the rescue ladder. It does NOT guess the offending span — it
// makes the request structurally different instead, in increasing order of cost:
//
//  1. force a neutral head in front of every scanned message;
//  2. insert zero-width breaks at sentence/clause/proper-name boundaries;
//  3. the same, between every pair of word runes (the dense form).
//
// Each rung is only spent if the previous one was refused, because rung 3 grows
// a 10.8k-character prompt by ~70%.
func gateLadder() []gateRung {
	return []gateRung{
		{"neutral head", channelgate.NeutralizeFingerprints},
		{"sparse zero-width", func(b []byte) ([]byte, bool) { return channelgate.PerturbFingerprints(b, false) }},
		{"dense zero-width", func(b []byte) ([]byte, bool) { return channelgate.PerturbFingerprints(b, true) }},
	}
}

// chatResult is one upstream answer: either a live 2xx response (the caller owns
// the body) or the failure status and body text.
type chatResult struct {
	resp   *http.Response
	status int
	body   []byte
	// tried lists the rescue rungs that were spent before giving up, so a
	// refusal that survives the whole ladder can be told apart from one that was
	// never retried.
	tried []string
}

// describe renders a failure for the client. The ladder history is appended
// because "upstream 400: 11128" alone cannot distinguish a request that was
// retried three times from one that was not retried at all.
func (r chatResult) describe() string {
	message := fmt.Sprintf("upstream %d: %s", r.status, truncate(string(r.body), 400))
	if len(r.tried) > 0 {
		message += fmt.Sprintf(" [channel-gate ladder exhausted: %s]", strings.Join(r.tried, " -> "))
	}
	return message
}

// postChat sends one chat request and returns the raw answer without consuming
// the body of a successful response.
func postChat(body []byte, sa *storedAuth) (chatResult, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))
	if err != nil {
		return chatResult{}, err
	}
	backendHeaders(httpReq, sa)
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return chatResult{}, fmt.Errorf("http_error: %w", err)
	}
	if resp.StatusCode < 400 {
		return chatResult{resp: resp, status: resp.StatusCode}, nil
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	return chatResult{status: resp.StatusCode, body: payload}, nil
}

// openChatStream posts a prepared body and walks the rescue ladder while the
// gateway keeps answering 11128. On success it returns a live response whose
// body the caller must close; otherwise it returns the last failure.
func openChatStream(body []byte, sa *storedAuth) (chatResult, error) {
	result, err := postChat(body, sa)
	if err != nil {
		return result, err
	}
	if result.resp != nil || !channelgate.IsChannelReject(result.body) {
		return result, nil
	}
	current := body
	var tried []string
	for _, rung := range gateLadder() {
		next, changed := rung.apply(current)
		if !changed {
			continue
		}
		current = next
		tried = append(tried, rung.label)
		result, err = postChat(current, sa)
		if err != nil {
			return result, err
		}
		if result.resp != nil || !channelgate.IsChannelReject(result.body) {
			return result, nil
		}
	}
	result.tried = tried
	return result, nil
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
	// CodeBuddy rejects non-stream requests (code 11101), so always stream
	// upstream and fold the chunks into a single chat.completion object.
	body := prepareBody(req.Payload, req.OriginalRequest)
	result, err := openChatStream(body, sa)
	if err != nil {
		return nil, err
	}
	if result.resp == nil {
		return nil, fmt.Errorf("%s", result.describe())
	}
	defer result.resp.Body.Close()
	completion, err := aggregateCompletion(result.resp.Body, req.Model)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
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
	body := prepareBody(req.Payload, req.OriginalRequest)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// The FIRST attempt is deliberately synchronous: the channel gate answers
	// 400 with a body, and a retry cannot be decided after the client has
	// already been handed a stream id.
	result, err := openChatStream(body, sa)
	if err != nil {
		if req.StreamID == "" {
			return nil, err
		}
		streamEmitError(req.StreamID, err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	if result.resp == nil {
		message := result.describe()
		if req.StreamID == "" {
			return nil, fmt.Errorf("%s", message)
		}
		streamEmitError(req.StreamID, message)
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}

	// No async stream id → drain the response and return the chunks inline.
	if req.StreamID == "" {
		defer result.resp.Body.Close()
		return okEnvelope(streamResponse{Headers: headers, Chunks: aggregateSSE(result.resp.Body, sseFramed)})
	}

	// Async: return immediately with empty chunks. A goroutine reads the already
	// open response and emits each chunk via host.stream.emit so the client sees
	// true streaming.
	go pumpUpstreamStream(result.resp, req.StreamID, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads an already-open upstream SSE response in the
// background and emits each cleaned chunk to the host stream. It closes the
// stream when done. An emit failure (client disconnected → host closed the
// stream) aborts the pump so we stop reading a dead upstream.
func pumpUpstreamStream(resp *http.Response, streamID string, sseFramed bool) {
	defer resp.Body.Close()
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
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	httpReq, err := http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
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

// applyModelTweaks pins reasoning_effort to "high" for Hunyuan-family models.
//
// Kept out of channelgate on purpose: this is a MODEL preference, not a gateway
// rule, and the two must be able to change independently.
func applyModelTweaks(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	if !forceMaxThinking(obj) {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// forceMaxThinking pins reasoning_effort to "high" for Hunyuan-family models
// (hy3 / hy4) so they always reason at maximum depth. CodeBuddy only honors
// "high" for deep thinking (medium/low/max/xhigh/ultra all fall back to no
// reasoning), so we override whatever the client sent. Returns true if changed.
func forceMaxThinking(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(model, "hy3") && !strings.HasPrefix(model, "hy4") {
		return false
	}
	if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
		return false
	}
	obj["reasoning_effort"] = "high"
	return true
}

// aggregateCompletion folds an SSE stream into a single non-streaming
// chat.completion object (used for non-stream client requests).
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	var toolCalls []map[string]any

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							toolCalls = append(toolCalls, call)
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	// SSE comment line (e.g. a ": heartbeat" keep-alive). Per the SSE spec a
	// line starting with ':' carries no event data; returning "" makes every
	// caller drop it via its existing empty-content check. The domestic
	// upstream does not currently send heartbeats, but the international one
	// does, and an unguarded comment leaks downstream as a chunk that clients
	// reject with `Unexpected token ':', ": heartbeat" is not valid JSON`.
	if strings.HasPrefix(s, ":") {
		return ""
	}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
