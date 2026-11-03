package wire_test

import (
	"net/http"
	"testing"

	"github.com/LiuJiaCheng11/cli-proxy/internal/wire"
)

func TestDeriveDeviceIDIsStableAndScoped(t *testing.T) {
	const uid = "96654dff-3b02-47a4-a20f-62052d3934ab"
	machine := wire.DeriveDeviceID(uid, "machine")
	session := wire.DeriveDeviceID(uid, "session")

	if machine == "" || session == "" {
		t.Fatal("empty device id")
	}
	if len(machine) != 36 {
		t.Errorf("device id is not the official 36-hex shape: %q", machine)
	}
	if machine == session {
		t.Error("machine and session ids must differ")
	}
	if again := wire.DeriveDeviceID(uid, "machine"); again != machine {
		t.Error("device id is not stable across calls")
	}
	// Two accounts on one machine must never look like one device: that is the
	// cross-account correlation the pair exists to prevent.
	other := wire.DeriveDeviceID("a9ec9fbf-0b5f-49c7-bca1-4598e24972a5", "machine")
	if other == machine {
		t.Error("two accounts derived the same machine id")
	}
	// The uid is hashed, never embedded: these values travel on the wire.
	if contains(uid, machine) {
		t.Error("the account uid is recoverable from the device id")
	}
	if wire.DeriveDeviceID("", "machine") != "" {
		t.Error("an empty uid must not derive an id")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestApplyHeadersDescribeOneClient(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	id := wire.Identity{Region: wire.RegionGlobal, UID: "uid-1", AppVersion: "5.8.3"}
	wire.ApplyAttributionHeaders(req, id)
	wire.ApplyDeviceHeaders(req, id)
	wire.ApplyConversationHeaders(req)

	// An official build says the same product name everywhere. A proxy that says
	// WorkBuddy in one header and CLI in another is a shape no official client
	// ever emits.
	if got := req.Header.Get("X-IDE-Name"); got != wire.ProductName {
		t.Errorf("X-IDE-Name = %q", got)
	}
	if got := req.Header.Get("X-IDE-Type"); got != wire.ProductName {
		t.Errorf("X-IDE-Type = %q", got)
	}
	if got := req.Header.Get("X-Product"); got != wire.ProductName {
		t.Errorf("X-Product = %q", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("global account must speak en-US, got %q", got)
	}
	if req.Header.Get("X-Machine-ID") == "" || req.Header.Get("X-Session-ID") == "" {
		t.Error("device pair missing")
	}
	// The three conversation ids must be distinct facts, and the request id must
	// be shared by the two names that mean the same thing.
	if req.Header.Get("X-Conversation-ID") == "" || req.Header.Get("X-Conversation-Message-ID") == "" {
		t.Error("conversation identity incomplete")
	}
	if req.Header.Get("X-Conversation-Request-ID") != req.Header.Get("X-Root-Request-ID") {
		t.Error("X-Root-Request-ID must equal X-Conversation-Request-ID")
	}
	if req.Header.Get("X-Conversation-ID") == req.Header.Get("X-Conversation-Request-ID") {
		t.Error("the conversation id must not be the request id")
	}
	if req.Header.Get("X-Request-ID") != req.Header.Get("X-Conversation-Message-ID") {
		t.Error("the official capture reuses one 32-hex value for request and message ids")
	}
}

func TestRegionSelectsOriginAndLanguage(t *testing.T) {
	cn := wire.Identity{Region: wire.RegionCN}
	if cn.AcceptLanguage() != "zh-CN" {
		t.Errorf("CN language = %q", cn.AcceptLanguage())
	}
	if cn.Origin("https://www.workbuddy.ai") != "https://www.codebuddy.cn" {
		t.Errorf("CN origin = %q", cn.Origin("https://www.workbuddy.ai"))
	}
	global := wire.Identity{Region: wire.RegionGlobal}
	if global.Origin("https://www.workbuddy.ai") != "https://www.workbuddy.ai" {
		t.Errorf("global origin = %q", global.Origin("https://www.workbuddy.ai"))
	}
}

func TestDevicePairIsAllOrNothing(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	wire.ApplyDeviceHeaders(req, wire.Identity{Region: wire.RegionCN, UID: ""})
	if req.Header.Get("X-Machine-ID") != "" || req.Header.Get("X-Session-ID") != "" {
		t.Error("a credential with no uid must send neither device header")
	}
}
