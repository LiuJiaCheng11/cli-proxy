// Package channelgate_test pins the behaviours the gateways were measured to
// demand. Each case names the failure it prevents.
package channelgate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LiuJiaCheng11/cli-proxy/internal/channelgate"
)

func body(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// The measured prompt: the same body answered 400 / 11128 until the WHOLE table
// was applied. Any single row is not enough, so this asserts all three identity
// clauses and a sample of the @-path paragraph are gone.
func TestRewritePhrasesRemovesEveryMeasuredFingerprint(t *testing.T) {
	prompt := strings.Join([]string{
		"You are an AI agent powered by DeepSeek Harness.",
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
		"You are interacting with the user through the DeepSeek Harness Web GUI at http://127.0.0.1:3080.",
		"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root;",
		"absolute paths identify files or directories on the host. A trailing slash marks a directory:",
		"list it when its contents matter. Anything else is a file: use the read tool when its contents are needed,",
		"and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces.",
	}, "\n\n")
	payload := body(t, map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "system", "content": prompt}},
	})

	out, changed := channelgate.RewritePhrasesInBody(payload)
	if !changed {
		t.Fatal("a prompt full of measured fingerprints was reported unchanged")
	}
	for _, banned := range []string{
		"You are an AI agent powered by DeepSeek Harness",
		"You are a coding agent powered by the ",
		"You are interacting with the user through the DeepSeek Harness Web GUI",
		"Tokens prefixed with @ are paths the user explicitly referenced.",
		"and do not claim to have inspected it before reading.",
	} {
		if strings.Contains(string(out), banned) {
			t.Errorf("fingerprint survived the rewrite: %q", banned)
		}
	}
}

// A body the gateway already answers must keep its bytes: re-serializing it
// would change a prompt that works today for no reason.
func TestRewritePhrasesLeavesCleanBodyUntouched(t *testing.T) {
	payload := body(t, map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "system", "content": "You are a helpful assistant."}},
	})
	out, changed := channelgate.RewritePhrasesInBody(payload)
	if changed {
		t.Fatal("a clean body was reported changed")
	}
	if string(out) != string(payload) {
		t.Fatal("a clean body was re-serialized")
	}
}

// `user` is not scanned by either gateway, and rewriting a user's own words
// would change the conversation for no gate.
func TestRewritePhrasesSkipsUserRole(t *testing.T) {
	const userText = "You are an AI agent powered by DeepSeek Harness"
	payload := body(t, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": userText}},
	})
	out, changed := channelgate.RewritePhrasesInBody(payload)
	if changed {
		t.Fatal("a user message was rewritten")
	}
	if !strings.Contains(string(out), userText) {
		t.Fatal("the user's own words were altered")
	}
}

// The gate reads EVERY text part of an array-content message, so rewriting only
// the first part fixes half the failure.
func TestRewritePhrasesWalksEveryTextPart(t *testing.T) {
	payload := body(t, map[string]any{
		"messages": []any{map[string]any{
			"role": "system",
			"content": []any{
				map[string]any{"type": "text", "text": "You are an AI agent powered by DeepSeek Harness"},
				map[string]any{"type": "text", "text": "You are a coding agent powered by the model."},
			},
		}},
	})
	out, changed := channelgate.RewritePhrasesInBody(payload)
	if !changed {
		t.Fatal("array-content fingerprints were not rewritten")
	}
	if strings.Contains(string(out), "powered by DeepSeek Harness") ||
		strings.Contains(string(out), "powered by the model") {
		t.Fatalf("a text part survived: %s", out)
	}
}

func TestNeutralizeFingerprintsPrependsHeadOnce(t *testing.T) {
	payload := body(t, map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "Original prompt."}},
	})
	first, changed := channelgate.NeutralizeFingerprints(payload)
	if !changed {
		t.Fatal("neutral head was not applied")
	}
	if !strings.HasPrefix(strings.TrimSpace(mustContent(t, first)), channelgate.FallbackSystemPrompt) {
		t.Fatalf("head missing: %s", first)
	}
	// Idempotent: a second pass must not stack a second head.
	second, changedAgain := channelgate.NeutralizeFingerprints(first)
	if changedAgain {
		t.Fatal("the neutral head was applied twice")
	}
	if string(second) != string(first) {
		t.Fatal("a second pass changed the body")
	}
}

func mustContent(t *testing.T, payload []byte) string {
	t.Helper()
	var obj struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil || len(obj.Messages) == 0 {
		t.Fatalf("cannot read body: %v", err)
	}
	return obj.Messages[0].Content
}

func TestPerturbFingerprintsIsIdempotent(t *testing.T) {
	payload := body(t, map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "Powered by DeepSeek Harness. Next sentence here."}},
	})
	first, changed := channelgate.PerturbFingerprints(payload, false)
	if !changed {
		t.Fatal("no perturbation applied")
	}
	if !strings.Contains(string(first), channelgate.ZeroWidthSpace) {
		t.Fatal("no zero-width space in the output")
	}
	second, changedAgain := channelgate.PerturbFingerprints(first, false)
	if changedAgain || string(second) != string(first) {
		t.Fatal("perturbation stacked on a second pass")
	}
}

func TestPrepareBodyForcesShape(t *testing.T) {
	payload := body(t, map[string]any{
		"model":       "deepseek-v4.1-flash",
		"stream":      false,
		"tool_choice": map[string]any{"type": "none"},
		"tools":       []any{map[string]any{"type": "function"}},
		"messages": []any{
			map[string]any{"role": "developer", "content": "be terse"},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out := channelgate.PrepareBody(payload)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["stream"] != true {
		t.Error("stream was not forced true")
	}
	if _, ok := obj["tools"]; ok {
		t.Error("tool_choice none must drop tools")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Error("tool_choice none must be dropped")
	}
	messages := obj["messages"].([]any)
	if role := messages[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("developer was not rewritten to system: %v", role)
	}
}

func TestPrepareBodyAddsSystemHeadWhenMissing(t *testing.T) {
	payload := body(t, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out := channelgate.PrepareBody(payload)
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(obj.Messages) != 2 || obj.Messages[0].Role != "system" {
		t.Fatalf("system head missing: %s", out)
	}
	if obj.Messages[0].Content != channelgate.FallbackSystemPrompt {
		t.Fatalf("wrong placeholder: %q", obj.Messages[0].Content)
	}
}

func TestIsChannelReject(t *testing.T) {
	reject := []string{
		`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
		`{"code": 11128, "msg":"x"}`,
		`{"msg":"first message is not system prompt"}`,
		`{"msg":"conversation id missing"}`,
	}
	for _, raw := range reject {
		if !channelgate.IsChannelReject([]byte(raw)) {
			t.Errorf("not recognised as a channel reject: %s", raw)
		}
	}
	// Deterministic refusals must NOT be retried: retrying spends the budget on
	// an answer that cannot change.
	keep := []string{
		`{"code":11140,"msg":"content policy"}`,
		`{"code":14018,"msg":"credits exhausted"}`,
		`{"msg":"Offline user session not found","code":12153}`,
		``,
	}
	for _, raw := range keep {
		if channelgate.IsChannelReject([]byte(raw)) {
			t.Errorf("wrongly treated as retryable: %s", raw)
		}
	}
}

// The upstream declares canDisableThinking:true on models that then answer
// 400 / 11133 to `off`. Measured 2026-11-03: every other level (low, medium,
// high, xhigh, max, minimal, none) answers 200, so the refusal is specific to
// `off`. CPA benches the credential on that 400, which is why it must be
// stripped before the request leaves rather than retried after it fails.
func TestPrepareBodyDropsRefusedOff(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash-sg", "deepseek-v4.1-flash", "gpt-5.6-terra"} {
		payload := body(t, map[string]any{
			"model":            model,
			"reasoning_effort": "off",
			"messages":         []any{map[string]any{"role": "system", "content": "hi"}},
		})
		out := channelgate.PrepareBody(payload)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, still := obj["reasoning_effort"]; still {
			t.Errorf("%s: reasoning_effort off was not dropped", model)
		}
	}
}

// Every other level is accepted, so stripping them would silently change how
// the model thinks on a request the upstream answers today.
func TestPrepareBodyKeepsAcceptedEfforts(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "minimal", "none"} {
		payload := body(t, map[string]any{
			"model":            "deepseek-v4.1-flash-sg",
			"reasoning_effort": effort,
			"messages":         []any{map[string]any{"role": "system", "content": "hi"}},
		})
		out := channelgate.PrepareBody(payload)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got, _ := obj["reasoning_effort"].(string); got != effort {
			t.Errorf("effort %q was altered to %q", effort, got)
		}
	}
}

// A model NOT on the refusal list keeps its `off`: the table errs towards
// hiding, but it must not over-hide.
func TestPrepareBodyKeepsOffForAcceptingModel(t *testing.T) {
	payload := body(t, map[string]any{
		"model":            "kimi-k3",
		"reasoning_effort": "off",
		"messages":         []any{map[string]any{"role": "system", "content": "hi"}},
	})
	out := channelgate.PrepareBody(payload)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := obj["reasoning_effort"].(string); got != "off" {
		t.Errorf("kimi-k3 accepts off, but it was %q", got)
	}
}
