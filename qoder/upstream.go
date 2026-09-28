package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Qoder runs two separate deployments with different hosts: the international
// one and the China one. Everything except the base URLs is identical, so a
// credential only has to record which region it belongs to.
type endpoints struct {
	center   string // auth center: PAT → job token (qoder2api path)
	api      string // inference
	modelAPI string
	account  string // web host that serves the device login page
	openapi  string // device login + token exchange
}

var (
	variantIntl = endpoints{
		center:   "https://center.qoder.sh",
		api:      "https://api3.qoder.sh",
		modelAPI: "https://api1.qoder.sh",
		account:  "https://qoder.sh",
		openapi:  "https://openapi.qoder.sh",
	}
	variantCN = endpoints{
		center:   "https://gateway.qoder.com.cn",
		api:      "https://gateway.qoder.com.cn",
		modelAPI: "https://gateway.qoder.com.cn",
		account:  "https://qoder.com.cn",
		openapi:  "https://openapi.qoder.com.cn",
	}
)

// defaultLoginVariant picks the region used when nobody has told us which one
// to use — auth.login.start runs before any credential exists, so there is
// nothing to read it from.
const defaultLoginVariant = "cn"

// endpointsFor resolves a stored region. Unknown or empty regions fall back to
// the international deployment, which is the upstream's own default.
func endpointsFor(variant string) endpoints {
	if strings.EqualFold(variant, "cn") {
		return variantCN
	}
	return variantIntl
}

const (
	pathChat     = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	pathModels   = "/algo/api/v2/model/list?Encode=1"
	pathJobToken = "/algo/api/v3/user/jobToken?Encode=1"

	// Device login: the CLI opens a browser on pathDeviceSelect, the user signs
	// in and picks an account, then it polls pathDevicePoll with the PKCE
	// verifier until the token appears.
	pathDeviceSelect  = "/device/selectAccounts"
	pathDevicePoll    = "/api/v1/deviceToken/poll"
	pathDeviceRefresh = "/api/v1/deviceToken/refresh"
	pathUserInfo      = "/api/v1/userinfo"

	deviceLoginTTL = 10 * time.Minute
)

var (
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

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

// pathSig strips the /algo prefix that every endpoint shares; the signature is
// computed over what remains.
func pathSig(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(parsed.Path, "/algo"), nil
}

// signedHeaders builds the header set every authenticated Qoder call carries.
func (s *session) signedHeaders(bearer, date, accept string) http.Header {
	h := http.Header{}
	h.Set("cosy-data-policy", "AGREE")
	h.Set("content-type", "application/json")
	h.Set("cosy-machinetype", s.machineType)
	h.Set("cosy-clienttype", clientType)
	h.Set("cosy-date", date)
	h.Set("cosy-user", s.ident.UID)
	h.Set("cosy-key", s.cosyKey)
	h.Set("accept", accept)
	h.Set("cosy-clientip", "169.254.198.161")
	h.Set("authorization", bearer)
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", cosyVersion)
	h.Set("cosy-machineid", s.machineID)
	h.Set("cosy-machinetoken", s.machineToken)
	h.Set("login-version", loginVersion)
	h.Set("user-agent", upstreamUA)
	return h
}

// callJSON performs an authenticated request and decodes the JSON response.
// The request body is cosy-encoded, matching Encode=1 on every endpoint.
func (s *session) callJSON(method, fullURL string, bodyObj any, accept string) (map[string]any, error) {
	encoded := ""
	if bodyObj != nil {
		raw, err := json.Marshal(bodyObj)
		if err != nil {
			return nil, err
		}
		encoded = cosyEncode(raw)
	}
	sig, err := pathSig(fullURL)
	if err != nil {
		return nil, err
	}
	bearer, date, err := s.signRequest(encoded, sig, time.Now())
	if err != nil {
		return nil, err
	}

	var body io.Reader
	if encoded != "" {
		body = strings.NewReader(encoded)
	}
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header = s.signedHeaders(bearer, date, firstNonEmpty(accept, "application/json"))

	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// openStream performs an authenticated request and hands back the raw response
// so the caller can read the SSE body incrementally.
func (s *session) openStream(fullURL string, bodyObj any) (*http.Response, error) {
	raw, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, err
	}
	encoded := cosyEncode(raw)
	sig, err := pathSig(fullURL)
	if err != nil {
		return nil, err
	}
	bearer, date, err := s.signRequest(encoded, sig, time.Now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header = s.signedHeaders(bearer, date, "text/event-stream")

	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	return resp, nil
}
