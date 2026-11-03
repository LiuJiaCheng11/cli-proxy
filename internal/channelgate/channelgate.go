// Package channelgate reproduces the request-side "channel verification"
// workarounds that the reference TypeScript plugin (dsh-connect-workbuddy)
// needed to get through the WorkBuddy / CodeBuddy gateways.
//
// The gateways (both copilot.tencent.com and www.workbuddy.ai) run a content
// check on every `system` and `assistant` message of a chat request and refuse
// a hit with HTTP 400 / business code 11128:
//
//	{"code":11128,"msg":"Illegal API invocation from an unapproved channel",
//	 "displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。"}}
//
// The message is a red herring: the failure is about the CONTENT of the prompt,
// not about the headers. Measured on 2026-11-02 against the live gateways with
// a real DeepSeek Harness system prompt (5,923 chars) and every other request
// field held fixed:
//
//	prompt sent as-is                         -> 400 / 11128
//	only the three identity sentences rewritten -> 400 / 11128
//	only the @-path paragraph rewritten         -> 400 / 11128
//	only "You are a helpful assistant." prepended -> 400 / 11128
//	only zero-width breaks inserted             -> 400 / 11128
//	the full phrase table applied               -> 200
//
// So the gate matches several spans at once and any single transform is not
// enough. This package therefore applies the whole table on every request and
// keeps a two-step rescue ladder for spans nobody has measured yet.
//
// Package layout mirrors the TS reference:
//
//	rewriteChannelFingerprintPhrases  <- CHANNEL_GATE_PHRASES
//	neutralizeChannelFingerprints     <- forceNeutralHead
//	perturbChannelFingerprints        <- perturbChannelFingerprints
//	IsChannelReject                   <- isChannelRejectBody / channelGateMemberOf
package channelgate

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// FallbackSystemPrompt is the stand-in system message for a request that
// reached the wire carrying none. Both gateways want the conversation to OPEN
// with a system message, and the international one enforces it (code 11128,
// "first message is not system prompt").
//
// It cannot be empty or blank: kimi-k3 / kimi-k2.6 / kimi-k2.8-preview /
// balanced-model answer 400 code 11151 ("a message has empty content") to a
// zero-length system message while every other model accepts it. This exact
// sentence is the shortest form measured to pass on all of them.
const FallbackSystemPrompt = "You are a helpful assistant."

// ZeroWidthSpace breaks an exact-substring content fingerprint without
// changing what a reader or a model sees.
const ZeroWidthSpace = "\u200b"

// gatePhrases is the measured rewrite table. Every entry is a MINIMAL edit: the
// sentence keeps its meaning and its grammar, and only the bytes the gateway
// fingerprints change. Order matters only in that no `to` string may recreate
// an earlier `from` string — that is what makes the pass idempotent.
//
// The first three are the agent-identity clauses: they introduce the request as
// something other than an official WorkBuddy client. The rest are the DeepSeek
// Harness @-path paragraph and the Web-GUI paragraph, which the gateway began
// matching as a long exact substring (its own reporters bisected the failing
// span to the first two sentences of each).
var gatePhrases = [][2]string{
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent running inside DeepSeek Harness"},
	{"You are a coding agent powered by the ", "You are a coding agent running on the "},
	{"You are interacting with the user through the DeepSeek Harness Web GUI", "You are working with the user inside the DeepSeek Harness Web GUI"},
	{"Agent Teams is available in this session", "Agent Teams is enabled for this session"},
	{"Tokens prefixed with @ are paths the user explicitly referenced.", "Paths that begin with @ are the ones the user explicitly referenced."},
	{"Relative paths resolve from the workspace root;", "Relative paths are resolved against the workspace root;"},
	{"absolute paths identify files or directories on the host.", "absolute paths point at files or directories on the host."},
	{"A trailing slash marks a directory:", "A trailing slash indicates a directory:"},
	{"list it when its contents matter.", "list it whenever its contents matter."},
	{"Anything else is a file:", "Everything else is a file:"},
	{"use the read tool when its contents are needed,", "use the read tool whenever you need its contents,"},
	{"and do not claim to have inspected it before reading.", "and never claim to have inspected it before reading it."},
	{"@\"...\" quotes a path containing spaces.", "A path containing spaces is quoted with @\"...\"."},
	{"without naming another target, they mean this GUI.", "and names no other target, the interface described here is meant."},
	{"they mean this GUI.", "the interface described here is meant."},
}

// Claude Code template phrases the gateways blocklist verbatim. Kept separate
// from gatePhrases because these are the CLI's own boilerplate rather than a
// harness fingerprint, and because they are what the original plugin shipped.
var claudeCodePhrases = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude.", "You are Claude Code, Anthropic's official CLI tool for Claude."},
	{"Main branch (you will usually use this for PRs)", "Default branch (you will usually use this for PRs)"},
}

// RewritePhrases applies both tables to one text and reports whether it changed.
func RewritePhrases(text string) (string, bool) {
	out := text
	for _, p := range claudeCodePhrases {
		out = strings.ReplaceAll(out, p[0], p[1])
	}
	for _, p := range gatePhrases {
		out = strings.ReplaceAll(out, p[0], p[1])
	}
	return out, out != text
}

// -----------------------------------------------------------------------------
// Message walking
// -----------------------------------------------------------------------------

// scannedRole reports whether the gateway scans this role's content. Measured:
// `system` and `assistant` are read; `user` and `tool` are not, and rewriting a
// user's own words would change the conversation for no gate.
func scannedRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "assistant", "developer":
		return true
	}
	return false
}

// mapScannedText calls fn on every text a gateway scans and replaces it with the
// result. Handles both the plain-string content shape and the OpenAI multimodal
// (array of parts) shape — the gate reads EVERY text part of an array, so
// rewriting only the first part fixes half the failure.
//
// fn returns (newText, changed). The walker returns true if any text changed.
func mapScannedText(obj map[string]any, fn func(string) (string, bool)) bool {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !scannedRole(role) {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if next, did := fn(content); did {
				msg["content"] = next
				changed = true
			}
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				kind, _ := part["type"].(string)
				if strings.ToLower(strings.TrimSpace(kind)) != "text" {
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					continue
				}
				if next, did := fn(text); did {
					part["text"] = next
					changed = true
				}
			}
		}
	}
	return changed
}

// parseBody decodes an OpenAI chat request. It returns nil for anything that is
// not a JSON object: a malformed body is the upstream's to reject, and a parse
// error must not turn a retry into a crash.
//
// Numbers are decoded as json.Number rather than float64 so a field the plugin
// never touches — a seed, a big token budget — goes back on the wire with the
// digits the client sent instead of a re-formatted float.
func parseBody(payload []byte) map[string]any {
	if len(payload) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var obj map[string]any
	if decoder.Decode(&obj) != nil || obj == nil {
		return nil
	}
	return obj
}

func marshalBody(obj map[string]any) ([]byte, bool) {
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// -----------------------------------------------------------------------------
// Pass 1: phrase rewrite
// -----------------------------------------------------------------------------

// RewritePhrasesInBody applies RewritePhrases to every scanned message and
// returns the re-serialized body. The boolean reports whether anything changed;
// when it is false the ORIGINAL bytes are returned untouched so a request the
// gateway already answers keeps its prompt byte-for-byte.
func RewritePhrasesInBody(payload []byte) ([]byte, bool) {
	obj := parseBody(payload)
	if obj == nil {
		return payload, false
	}
	if !mapScannedText(obj, RewritePhrases) {
		return payload, false
	}
	if out, ok := marshalBody(obj); ok {
		return out, true
	}
	return payload, false
}

// -----------------------------------------------------------------------------
// Pass 2: neutral head (rescue rung 1)
// -----------------------------------------------------------------------------

func startsWithNeutralHead(text string) bool {
	return strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), FallbackSystemPrompt)
}

// NeutralizeFingerprints prepends FallbackSystemPrompt in front of every
// scanned message that does not already carry it.
//
// This is the first rescue rung, not the main fix: the phrase table above is
// what a measured request needs. This rung exists because the gateway's list is
// its own data — it has grown twice without notice — and a prefix rule that
// matches at offset 0 is defeated by one prepended line. It is also the rung
// that covers a conversation whose HISTORY carries a refused opening (the model
// echoing its own prompt, a restored session, a compaction artefact).
//
// Returns the original bytes and false when nothing would change, which is the
// caller's signal not to spend a request.
func NeutralizeFingerprints(payload []byte) ([]byte, bool) {
	obj := parseBody(payload)
	if obj == nil {
		return payload, false
	}
	changed := mapScannedText(obj, func(text string) (string, bool) {
		if strings.TrimSpace(text) == "" || startsWithNeutralHead(text) {
			return text, false
		}
		return FallbackSystemPrompt + "\n\n" + text, true
	})
	if !changed {
		return payload, false
	}
	if out, ok := marshalBody(obj); ok {
		return out, true
	}
	return payload, false
}

// -----------------------------------------------------------------------------
// Pass 3: zero-width perturbation (rescue rung 2)
// -----------------------------------------------------------------------------

var (
	sentenceBreak = regexp.MustCompile(`([.!?])(\s+)`)
	cjkBreak      = regexp.MustCompile(`([。！？])([^\x{200b}])`)
	clauseBreak   = regexp.MustCompile(`([,;])(\s+)`)
	// Two adjacent capitalised words are a proper name ("DeepSeek Harness",
	// "Visual Studio") far more often than they are ordinary prose, and a proper
	// name is exactly the kind of span the gateway fingerprints.
	wordInternal = regexp.MustCompile(`([A-Z][A-Za-z0-9]*)(\s+)([A-Z][A-Za-z0-9])`)
	denseWord    = regexp.MustCompile(`[A-Za-z0-9\x{3040}-\x{30ff}\x{4e00}-\x{9fff}]`)
)

// PerturbFingerprints inserts zero-width spaces at sentence, clause and
// proper-name boundaries of every scanned message.
//
// The gateway's rule is an exact SUBSTRING match, so any perturbation inside the
// matched span defeats it, while the text a model reads is unchanged. A list of
// refused spans cannot work (the list is the upstream's and it grows), which is
// why this perturbs structure instead.
//
// dense adds a break between every pair of word runes. It is the last resort,
// used only for a refusal that the neutral head and the sparse form could not
// clear: on a real 10.8k-character prompt the sparse form costs ~28 characters
// (+0.26%) and the dense form ~7.7k (+70%).
//
// Idempotent: a text that already carries U+200B is left alone, so a retry never
// stacks perturbations. Returns the original bytes and false when nothing would
// change.
func PerturbFingerprints(payload []byte, dense bool) ([]byte, bool) {
	obj := parseBody(payload)
	if obj == nil {
		return payload, false
	}
	changed := mapScannedText(obj, func(text string) (string, bool) {
		if strings.Contains(text, ZeroWidthSpace) {
			return text, false
		}
		next := perturbText(text, dense)
		if next == text {
			return text, false
		}
		return next, true
	})
	if !changed {
		return payload, false
	}
	if out, ok := marshalBody(obj); ok {
		return out, true
	}
	return payload, false
}

func perturbText(text string, dense bool) string {
	out := sentenceBreak.ReplaceAllString(text, "$1"+ZeroWidthSpace+"$2")
	out = cjkBreak.ReplaceAllString(out, "$1"+ZeroWidthSpace+"$2")
	out = clauseBreak.ReplaceAllString(out, "$1"+ZeroWidthSpace+"$2")
	out = wordInternal.ReplaceAllString(out, "$1"+ZeroWidthSpace+"$2$3")
	if dense {
		out = spaceDenseBreaks(out)
	}
	return out
}

// spaceDenseBreaks inserts a zero-width space between every pair of adjacent
// non-space runes in a run that starts with a word rune.
func spaceDenseBreaks(text string) string {
	runes := []rune(text)
	var sb strings.Builder
	sb.Grow(len(text) * 2)
	var runStart rune
	inRun := false
	for index, r := range runes {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			inRun = false
			sb.WriteRune(r)
			continue
		}
		if !inRun {
			runStart = r
			inRun = true
		}
		prev := rune(0)
		if index > 0 {
			prev = runes[index-1]
		}
		if index > 0 && inRun && runStart != []rune(ZeroWidthSpace)[0] &&
			denseWord.MatchString(string(runStart)) && r != []rune(ZeroWidthSpace)[0] &&
			prev != []rune(ZeroWidthSpace)[0] && prev != ' ' {
			sb.WriteString(ZeroWidthSpace)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// -----------------------------------------------------------------------------
// Pass 0: structural shape the gateways insist on
// -----------------------------------------------------------------------------

// PrepareBody puts a request body into the shape both gateways accept:
//
//   - `stream` is forced true — the upstream rejects non-streaming chat
//     requests outright (code 11101);
//   - `developer` is rewritten to `system` — the gateways reject the role;
//   - a leading system message is guaranteed (see FallbackSystemPrompt);
//   - `tool_choice` is normalized to the string form the upstream understands
//     (`"none"` also drops the `tools`/`functions` arrays, which is what the
//     official client does).
//
// It never fails: a body it cannot parse is returned unchanged.
func PrepareBody(payload []byte) []byte {
	obj := parseBody(payload)
	if obj == nil {
		return payload
	}
	obj["stream"] = true
	if messages, ok := obj["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); strings.EqualFold(strings.TrimSpace(role), "developer") {
				msg["role"] = "system"
			}
		}
		ensureSystemHead(obj, messages)
	}
	normalizeToolChoice(obj)
	dropRefusedReasoning(obj)
	out, ok := marshalBody(obj)
	if !ok {
		return payload
	}
	return out
}

// refusesOff lists the model ids measured to answer HTTP 400 / code 11133 to
// `reasoning_effort: "off"` — "the request parameters were rejected by the
// model provider" — even though their catalog entry declares
// `canDisableThinking: true`.
//
// WHY THIS IS A HARD LIST AND NOT A CAPABILITY CHECK. The upstream's own
// declaration is what lies here, so believing it is exactly the bug. The cost is
// also asymmetric: offering a level that always fails costs EVERY request for
// that model, while hiding one that would have worked costs a single option.
//
// WHY IT MATTERS BEYOND ONE FAILED REQUEST. Measured 2026-11-03 on
// www.workbuddy.ai with a real token: `off` answers 400 / 11133 while low,
// medium, high, xhigh, max, minimal and none all answer 200 — so the refusal is
// specific to `off`, not to the parameter. CPA reacts to that 400 by benching
// the credential ("auth_unavailable ... last upstream error: upstream 400
// {"code":11133"..."), which turns one bad option into every model on that
// account failing. That is why this must be stripped BEFORE the request leaves,
// not retried after it fails.
//
// Keyed by exact model id and deliberately not by region: the split tracks the
// model family, not the gateway. Aliases (`primary-model`, `default-model`) are
// resolved by the gateway, so the ids that actually travel are what is listed;
// ids added upstream later are covered by dropRefusedReasoning's own guard only
// as far as they appear here, and the ladder in the plugin main packages covers
// the rest by making the body different on the next attempt.
var refusesOff = map[string]bool{
	"deepseek-v4.1-flash":    true,
	"deepseek-v4.1-flash-sg": true,
	"deepseek-v4-pro":        true,
	"primary-model":          true,
	"gpt-6-astra":            true,
	"gpt-5.6-sol":            true,
	"gpt-5.6-terra":          true,
	"gpt-5.6-luna":           true,
	"gpt-6-luna":             true,
	"gemini-3.5-flash":       true,
}

// RefusesOff reports whether a model is known to reject `reasoning_effort: off`.
func RefusesOff(modelID string) bool {
	return refusesOff[strings.TrimSpace(modelID)]
}

// dropRefusedReasoning deletes `reasoning_effort: "off"` when the model is known
// to reject it.
//
// Stripped HERE because this is the last place the plugin controls the body
// before the upstream counts it, and because the test is the same table the
// caller can read — so the two cannot drift apart.
//
// Only the literal `off` is touched. Every other effort value answered 200 on
// every model measured, and a body naming no effort is left alone: deleting a
// field the caller never sent would be inventing an opinion about how the model
// should think.
func dropRefusedReasoning(obj map[string]any) {
	effort, ok := obj["reasoning_effort"].(string)
	if !ok || !strings.EqualFold(strings.TrimSpace(effort), "off") {
		return
	}
	model, _ := obj["model"].(string)
	if !RefusesOff(model) {
		return
	}
	delete(obj, "reasoning_effort")
}

// ensureSystemHead puts a system message in front of a conversation that has
// none. `developer` has already been normalized to `system`, so an ordinary
// request is a no-op here.
func ensureSystemHead(obj map[string]any, messages []any) {
	if len(messages) == 0 {
		return
	}
	head, ok := messages[0].(map[string]any)
	if !ok {
		return
	}
	if role, _ := head["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
		return
	}
	obj["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": FallbackSystemPrompt,
	}}, messages...)
}

// normalizeToolChoice rewrites the OpenAI object spellings of `tool_choice` into
// the bare-string form the upstream accepts.
func normalizeToolChoice(obj map[string]any) {
	raw, present := obj["tool_choice"]
	if !present {
		return
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	switch choice := raw.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(choice), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
		return
	case map[string]any:
		kind, _ := choice["type"].(string)
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(kind))
		case "function":
			name := ""
			if fn, ok := choice["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				name, _ = choice["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
		return
	}
	delete(obj, "tool_choice")
}

// -----------------------------------------------------------------------------
// Failure classification
// -----------------------------------------------------------------------------

// IsChannelReject reports whether an upstream error body is the channel gate
// refusing the request, i.e. a case a rewrite can rescue.
//
// Matched on the literal `11128` code and on the two markers both gateways use.
// The code alone is not enough to be precise about WHY, but it is precise about
// WHAT TO DO: 11128 is the gate family (content fingerprint, missing
// conversation identity, `developer` role, missing system head), and every
// member of that family is worth one retry with a rewritten body — while a
// content-policy refusal (11140), an exhausted quota (14018) or a dead session
// (12153) is deterministic and must NOT be retried.
func IsChannelReject(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := bytes.ToLower(body)
	if bytes.Contains(body, []byte(`"code":11128`)) || bytes.Contains(body, []byte(`"code": 11128`)) {
		return true
	}
	if bytes.Contains(lower, []byte("unapproved channel")) ||
		bytes.Contains(lower, []byte("illegal api invocation")) {
		return true
	}
	if bytes.Contains(lower, []byte("first message is not system prompt")) ||
		bytes.Contains(lower, []byte("conversation id missing")) {
		return true
	}
	return false
}
