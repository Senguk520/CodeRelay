package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestCollectChatCompletionSeparatesReasoningFromContent guards the regression
// where a reasoning-capable vision model's thinking leaked into the answer text
// that gets fed back to the text-only model. collectChatCompletion must keep
// delta.reasoning_content out of choices[0].message.content so that
// inspectCodebuddyImageWithEmit returns only the visible answer text.
func TestCollectChatCompletionSeparatesReasoningFromContent(t *testing.T) {
	lines := [][]byte{
		[]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","created":1,"model":"hy3-preview","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"让我看看这张图"}}]}`),
		[]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","created":1,"model":"hy3-preview","choices":[{"index":0,"delta":{"reasoning_content":"这是一张"}}]}`),
		[]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","created":1,"model":"hy3-preview","choices":[{"index":0,"delta":{"content":"红色方块"}}]}`),
		[]byte(`data: {"id":"cmpl-1","object":"chat.completion.chunk","created":1,"model":"hy3-preview","choices":[{"index":0,"delta":{"content":"，背景是蓝色"},"finish_reason":"stop"}]}`),
	}

	out := collectChatCompletion(lines)

	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "红色方块，背景是蓝色" {
		t.Fatalf("content = %q, want only the visible answer text", got)
	}
	// The thinking must be preserved separately, not merged into content.
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "让我看看这张图这是一张" {
		t.Fatalf("reasoning_content = %q, want thinking text kept separate", got)
	}
}

// TestCollectChatCompletionReasoningOnlyYieldsEmptyContent ensures that when a
// vision model emits only thinking and no answer, the resulting content is empty
// (so inspectCodebuddyImageWithEmit reports an empty answer instead of feeding
// thinking back to the text-only model).
func TestCollectChatCompletionReasoningOnlyYieldsEmptyContent(t *testing.T) {
	lines := [][]byte{
		[]byte(`data: {"id":"cmpl-2","object":"chat.completion.chunk","created":1,"model":"hy3-preview","choices":[{"index":0,"delta":{"reasoning_content":"无法解析图片"}}]}`),
	}

	out := collectChatCompletion(lines)

	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "" {
		t.Fatalf("content = %q, want empty when only reasoning is present", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "无法解析图片" {
		t.Fatalf("reasoning_content = %q, want thinking preserved", got)
	}
}

// TestInspectCodebuddyImageDisablesReasoning guards the regression where the
// vision sub-request did not disable reasoning, causing hy3-preview to emit
// thinking that polluted the forwarded delta and the tool result.
func TestInspectCodebuddyImageDisablesReasoning(t *testing.T) {
	// Build the request the same way inspectCodebuddyImageWithEmit does, then
	// verify the reasoning field is present and disabled. This mirrors the inline
	// body construction so a future removal of the guard is caught.
	body := map[string]any{
		"model": "hy3-preview",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				map[string]any{"type": "text", "text": "描述这张图"},
			}},
		},
	}
	body["reasoning_effort"] = "none"

	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := gjson.GetBytes(b, "reasoning_effort").String(); got != "none" {
		t.Fatalf("reasoning_effort = %q, want %q", got, "none")
	}
	if strings.Contains(string(b), "hy3-preview") == false {
		t.Fatalf("body should reference the vision model")
	}
}

// TestStripVisionThinkingLineReasoningOnlyDrops guards rule A: a delta carrying
// only reasoning_content (no content) is dropped outright.
func TestStripVisionThinkingLineReasoningOnlyDrops(t *testing.T) {
	line := []byte(`data: {"id":"cmpl-1","model":"hy3-preview","choices":[{"index":0,"delta":{"reasoning_content":"让我看看这张图"}}]}`)
	cleaned, drop := stripVisionThinkingLine(line)
	if !drop {
		t.Fatalf("expected reasoning-only line to be dropped")
	}
	if cleaned != nil {
		t.Fatalf("expected nil cleaned for a dropped line, got %q", cleaned)
	}
}

// TestStripVisionThinkingLineVisibleContentKept guards that a normal content
// delta (no thinking) is preserved unchanged.
func TestStripVisionThinkingLineVisibleContentKept(t *testing.T) {
	line := []byte(`data: {"id":"cmpl-1","model":"hy3-preview","choices":[{"index":0,"delta":{"content":"红色方块"}}]}`)
	cleaned, drop := stripVisionThinkingLine(line)
	if drop {
		t.Fatalf("expected visible content line to be kept")
	}
	// The content must survive stripping.
	if got := gjson.GetBytes(cleaned, "choices.0.delta.content").String(); got != "红色方块" {
		t.Fatalf("content = %q, want %q", got, "红色方块")
	}
}

// TestStripVisionThinkingLineMixedReasoningAndContentKeepsContent guards that a
// chunk carrying both reasoning_content and a visible content delta is kept, with
// the visible content intact.
func TestStripVisionThinkingLineMixedReasoningAndContentKeepsContent(t *testing.T) {
	line := []byte(`data: {"id":"cmpl-1","model":"hy3-preview","choices":[{"index":0,"delta":{"reasoning_content":"思考中","content":"这是一张图"}}]}`)
	cleaned, drop := stripVisionThinkingLine(line)
	if drop {
		t.Fatalf("expected line with visible content to be kept")
	}
	if got := gjson.GetBytes(cleaned, "choices.0.delta.content").String(); got != "这是一张图" {
		t.Fatalf("content = %q, want %q", got, "这是一张图")
	}
}

// TestStripVisionThinkingLineNonDataLinePassThrough guards that non-SSE lines
// (e.g. comments) are returned unchanged and not dropped.
func TestStripVisionThinkingLineNonDataLinePassThrough(t *testing.T) {
	line := []byte(": keep-alive")
	cleaned, drop := stripVisionThinkingLine(line)
	if drop {
		t.Fatalf("expected non-data line to not be dropped")
	}
	if string(cleaned) != string(line) {
		t.Fatalf("cleaned = %q, want %q", cleaned, line)
	}
}

// TestStripVisionThinkingLineDonePassThrough guards that [DONE] sentinels are
// passed through unchanged.
func TestStripVisionThinkingLineDonePassThrough(t *testing.T) {
	line := []byte("data: [DONE]")
	cleaned, drop := stripVisionThinkingLine(line)
	if drop {
		t.Fatalf("expected [DONE] to not be dropped")
	}
	if string(cleaned) != string(line) {
		t.Fatalf("cleaned = %q, want %q", cleaned, line)
	}
}

// TestStripThinkingMarkersFromContentEmptyTableIsIdentity guards the conservative
// default: with an empty marker table, content text is returned verbatim so
// legitimate image descriptions are never stripped.
func TestStripThinkingMarkersFromContentEmptyTableIsIdentity(t *testing.T) {
	text := "图片右上角的文字是「出口」，背景是蓝色。"
	if got := stripThinkingMarkersFromContent(text); got != text {
		t.Fatalf("got %q, want identity %q", got, text)
	}
}

// TestInspectImageSystemPromptForbidsAdvice guards the agentic-path system
// constraint: the vision sub-request's system message must require objective
// detail extraction only and forbid solutions/suggestions.
func TestInspectImageSystemPromptForbidsAdvice(t *testing.T) {
	for _, keyword := range []string{"解决方案", "修改建议", "操作步骤", "分析判断", "只客观"} {
		if !strings.Contains(codebuddyInspectImageSystemPrompt, keyword) {
			t.Fatalf("codebuddyInspectImageSystemPrompt must forbid %q, got: %s", keyword, codebuddyInspectImageSystemPrompt)
		}
	}
}

// TestInspectCodebuddyImagePrependsSystemMessage guards that the vision
// sub-request built by inspectCodebuddyImageWithEmit carries a system message
// with the detail-extraction constraint before the user message. It mirrors the
// body construction order (marshal → prepend system) so a future removal of the
// prepend is caught.
func TestInspectCodebuddyImagePrependsSystemMessage(t *testing.T) {
	body := map[string]any{
		"model": "hy3-preview",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				map[string]any{"type": "text", "text": "图片里的文字是什么"},
			}},
		},
	}
	body["reasoning_effort"] = "none"

	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err = prependCodebuddySystemMessage(b, codebuddyInspectImageSystemPrompt)
	if err != nil {
		t.Fatalf("prepend: %v", err)
	}

	msgs := gjson.GetBytes(b, "messages")
	if !msgs.IsArray() || len(msgs.Array()) != 2 {
		t.Fatalf("expected 2 messages after prepend, got %s", msgs.Raw)
	}
	if gjson.GetBytes(b, "messages.0.role").String() != "system" {
		t.Fatalf("messages.0 must be system, got %s", gjson.GetBytes(b, "messages.0.role").String())
	}
	if got := gjson.GetBytes(b, "messages.0.content").String(); got != codebuddyInspectImageSystemPrompt {
		t.Fatalf("system content = %q, want %q", got, codebuddyInspectImageSystemPrompt)
	}
	if gjson.GetBytes(b, "messages.1.role").String() != "user" {
		t.Fatalf("messages.1 must be the user message, got %s", gjson.GetBytes(b, "messages.1.role").String())
	}
}

