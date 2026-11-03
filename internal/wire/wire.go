// Package wire builds the outbound identity a WorkBuddy / CodeBuddy chat
// request must present.
//
// WHY THIS PACKAGE EXISTS. Both gateways run a "channel verification" pass over
// chat requests. The reference TypeScript plugin (dsh-connect-workbuddy) had to
// reverse-engineer three separate families of headers before its traffic looked
// like an official client:
//
//  1. the conversation-identity triple — X-Conversation-ID plus the
//     request/message ids. Without them a SAMPLED request is refused with
//     HTTP 400 / code 11128 "conversation id missing", whose displayTips blame
//     "渠道校验 / 不是官方客户端发出的". The failure is intermittent, which is
//     why it reads as "that account stopped working".
//  2. the attribution headers — X-Agent-Purpose / X-IDE-* / X-Product. An
//     official client says the same product name in all of them; a proxy that
//     says `X-Product: SaaS` while `X-IDE-Type: CLI` is a shape no official
//     build ever emits.
//  3. the device pair — X-Machine-ID / X-Session-ID, derived per account so
//     two accounts never look like one machine.
//
// Measured 2026-11-02 against both live gateways: with a body the gate already
// accepts, the older header set answers 200 as well, so none of this is a hard
// gate today. It is kept because (a) the conversation triple IS a hard gate on
// sampled international traffic, and (b) an outbound shape that contradicts
// itself is the cheapest thing for a static rule to pick out.
package wire

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// ProductName is the client name an official build claims in X-IDE-Name,
// X-IDE-Type and X-Product. All three carry the SAME value on purpose: the
// official clients never say "WorkBuddy" in one header and "CLI" in another.
const ProductName = "WorkBuddy"

// Region identifies which gateway a credential belongs to. It selects the
// Origin/Referer pair and the Accept-Language the official client would send.
type Region int

const (
	// RegionCN is copilot.tencent.com (domestic).
	RegionCN Region = iota
	// RegionGlobal is www.workbuddy.ai / www.codebuddy.ai (international).
	RegionGlobal
)

// Identity is the per-credential input to the header builders.
type Identity struct {
	Region     Region
	UID        string
	AppVersion string
}

// AcceptLanguage is the language an official client sends for the account's
// region. A Chinese account sending en-US reads as "this machine changed
// language", which is one of the cheapest signals to cluster on.
func (id Identity) AcceptLanguage() string {
	if id.Region == RegionGlobal {
		return "en-US"
	}
	return "zh-CN"
}

// Origin is the web origin the official clients send. The CN product lives on
// codebuddy.cn; the global one on its own gateway.
func (id Identity) Origin(globalBase string) string {
	if id.Region == RegionGlobal {
		return globalBase
	}
	return "https://www.codebuddy.cn"
}

// DeriveDeviceID returns a stable 36-hex id for one (account, purpose) pair.
//
// Salts are derived from the account uid, so the same account always presents
// the same device while two accounts on one machine never share one — the
// cross-account correlation the gateways would otherwise be able to do.
//
// The uid is hashed, never embedded: these values travel on the wire, and a
// header that is literally the account id is a different (and much worse) thing
// than a pseudonym.
func DeriveDeviceID(uid, purpose string) string {
	if strings.TrimSpace(uid) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("dsh-cpa/device/v1\x00" + purpose + "\x00" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars, the official shape
}

// hexID returns 32 lowercase hex characters, the shape the official client uses
// for X-Request-ID and the trace ids.
func hexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.Repeat("0", 32)
	}
	return hex.EncodeToString(b[:])
}

// uuid4 returns a dashed (8-4-4-4-12) UUIDv4, the shape of X-Conversation-ID.
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ApplyConversationHeaders adds the conversation-identity family.
//
// The ids are freshly generated per request, as the official client generates
// its own per call: reusing one value across requests would forge a conversation
// identity the plugin does not actually have, and the first thing that breaks is
// the upstream's own correlation of a session it is told about twice under one
// id.
func ApplyConversationHeaders(req *http.Request) {
	requestID := hexID()
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Conversation-ID", uuid4())
	req.Header.Set("X-Conversation-Request-ID", requestID)
	req.Header.Set("X-Root-Request-ID", requestID)
	req.Header.Set("X-Conversation-Message-ID", requestID)
}

// ApplyAttributionHeaders makes the request describe one coherent client.
func ApplyAttributionHeaders(req *http.Request, id Identity) {
	version := id.AppVersion
	if version == "" {
		version = "5.8.3"
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", ProductName)
	req.Header.Set("X-IDE-Type", ProductName)
	req.Header.Set("X-IDE-Version", version)
	req.Header.Set("X-Product", ProductName)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", id.AcceptLanguage())
}

// ApplyDeviceHeaders adds the derived device pair. Both or neither: a real
// client never sends a machine id without a session id.
func ApplyDeviceHeaders(req *http.Request, id Identity) {
	machine := DeriveDeviceID(id.UID, "machine")
	session := DeriveDeviceID(id.UID, "session")
	if machine == "" || session == "" {
		return
	}
	req.Header.Set("X-Machine-ID", machine)
	req.Header.Set("X-Session-ID", session)
}
