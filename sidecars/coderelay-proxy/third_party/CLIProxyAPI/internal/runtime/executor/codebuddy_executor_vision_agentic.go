package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// inspectImageToolName is the tool injected into the text-only model so it can
// autonomously query the vision model for image details during reasoning.
const inspectImageToolName = "inspect_image"

// codebuddyAgenticImageRef holds an extracted image's original content part,
// kept in memory so inspect_image can re-send it to the vision model on demand.
type codebuddyAgenticImageRef struct {
	id       int
	partJSON []byte // {"type":"image_url","image_url":{...}} raw part
}

// codebuddyVisionAgenticEnabled reports whether the agentic vision-proxy mode
// is active.
func (e *CodebuddyExecutor) codebuddyVisionAgenticEnabled() bool {
	return e.cfg.CodebuddyVision.NormalizedVisionMode() == config.CodebuddyVisionModeAgentic
}

// inspectImageToolDef returns the OpenAI function-tool definition injected into
// the request so the text-only model can inspect attached images.
func inspectImageToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        inspectImageToolName,
			"description": "查看已附加图片的细节。当你需要确认图片中的具体内容（文字、数字、位置、颜色、图表数据等）时调用。可多次调用以查看不同细节。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"image_id": map[string]any{
						"type":        "integer",
						"description": "图片编号，从 1 开始。",
					},
					"question": map[string]any{
						"type":        "string",
						"description": "针对该图片的具体问题，例如“图片右上角的文字是什么”。",
					},
				},
				"required": []string{"image_id", "question"},
			},
		},
	}
}

// extractCodebuddyImagesForAgentic rewrites the body so no image part reaches a
// text-only model: images in the current turn (the last user message and any
// subsequent assistant/tool messages) are extracted as inspect_image targets,
// while images in earlier messages (stale history re-sent by the client) are
// replaced with a neutral placeholder.
func extractCodebuddyImagesForAgentic(body []byte) ([]byte, []codebuddyAgenticImageRef, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, nil, nil
	}
	arr := messages.Array()
	lastUserIdx := lastCodebuddyUserMessageIndex(arr)
	if lastUserIdx < 0 {
		return body, nil, nil
	}

	out := body
	var images []codebuddyAgenticImageRef
	for mi, msg := range arr {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		isCurrent := mi >= lastUserIdx
		for ci, part := range content.Array() {
			if !isCodebuddyImagePartType(part.Get("type").String()) {
				continue
			}
			path := fmt.Sprintf("messages.%d.content.%d", mi, ci)

			var replacement []byte
			if isCurrent {
				id := len(images) + 1
				images = append(images, codebuddyAgenticImageRef{
					id:       id,
					partJSON: append([]byte(nil), []byte(part.Raw)...),
				})
				text := fmt.Sprintf("[图片 #%d 已附加，可用 inspect_image 工具查看，image_id=%d]", id, id)
				var err error
				replacement, err = json.Marshal(map[string]string{"type": "text", "text": text})
				if err != nil {
					return body, nil, err
				}
			} else {
				replacement, _ = json.Marshal(map[string]string{"type": "text", "text": codebuddyHistoricalImageText})
			}

			var err error
			out, err = sjson.SetRawBytes(out, path, replacement)
			if err != nil {
				return body, nil, err
			}
		}
	}
	return out, images, nil
}

// injectCodebuddyInspectTool prepends a system guidance message and injects the
// inspect_image tool definition into the request body.
func injectCodebuddyInspectTool(body []byte, imageCount int) []byte {
	guide := fmt.Sprintf(
		"用户消息中附带了 %d 张图片，但图片内容已从消息中移除，你无法直接看到。你需要使用 inspect_image 工具查看图片细节。当回答涉及图片具体内容（文字、数字、位置、颜色、图表数据等）时，请先调用 inspect_image 工具获取细节，再作答。你可以多次调用该工具查看不同细节。",
		imageCount,
	)
	out, err := prependCodebuddySystemMessage(body, guide)
	if err != nil {
		out = body
	}

	toolsJSON, err := json.Marshal([]any{inspectImageToolDef()})
	if err != nil {
		return out
	}
	out, err = sjson.SetRawBytes(out, "tools", toolsJSON)
	if err != nil {
		return body
	}
	// The client may have sent a `tool_choice` naming one of its own tools (or
	// "required"). Since `tools` was just replaced with the single inspect_image
	// tool, a stale tool_choice is now inconsistent and can be rejected by the
	// strict backend with 400 — reset it to "auto".
	out, err = sjson.SetBytes(out, "tool_choice", "auto")
	if err != nil {
		return body
	}
	return out
}

// appendAgenticMessage appends a raw JSON message to the messages array.
func appendAgenticMessage(body []byte, msgRaw []byte) ([]byte, error) {
	return sjson.SetRawBytes(body, "messages.-1", msgRaw)
}

// doCodebuddyChatRequest performs a single forced-stream chat completion and
// aggregates it into a non-streaming ChatCompletion JSON. It returns the
// aggregated payload and the upstream response headers.
func (e *CodebuddyExecutor) doCodebuddyChatRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	creds codebuddy.Creds,
	baseURL string,
	body []byte,
) ([]byte, http.Header, error) {
	var err error
	body, err = sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, nil, err
	}
	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, nil, err
	}
	// Clamp oversized max_tokens (Cursor sends 65536) to the model's declared
	// ceiling, matching the normal request path.
	body = clampCodebuddyMaxTokens(body, gjson.GetBytes(body, "model").String())

	// Normalize tool-related message fields so the strict backend does not
	// reject accumulated tool-calling rounds with 400 invalid_parameter_value,
	// matching the normal request path.
	body, err = normalizeCodebuddyToolMessages(body)
	if err != nil {
		return nil, nil, err
	}

	url := baseURL + codebuddy.ChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	applyCodebuddyHeaders(httpReq, creds)
	httpReq.Header.Set("Accept", "text/event-stream")

	// Diagnostic: dump the agentic sub-request body (redacted) so the exact
	// tools/tool_choice/max_tokens the loop sends can be inspected on failure.
	helps.DumpCodebuddyDebugBody("agentic-request", body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.DumpCodebuddyDebugBody("agentic-error", b)
		log.Warnf("codebuddy vision agentic: round request failed status=%d body=%s",
			httpResp.StatusCode, summarize(b))
		return nil, httpResp.Header.Clone(), statusErr{code: codebuddyEffectiveStatus(httpResp.StatusCode, b), msg: string(b)}
	}

	lines := make([][]byte, 0, 64)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		lines = append(lines, bytes.Clone(scanner.Bytes()))
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, httpResp.Header.Clone(), errScan
	}
	return collectChatCompletion(lines), httpResp.Header.Clone(), nil
}

// doCodebuddyChatRequestWithEmit is doCodebuddyChatRequest with an optional
// streaming sink: as each content delta is scanned, it is forwarded through
// emit (if non-nil). The aggregated ChatCompletion is still returned for the
// caller. This lets the vision sub-agent's answer stream to the client in real
// time instead of being buffered until the round completes. baseModel is used
// to rewrite the forwarded chunk's model field so the client always sees the
// requested model rather than the vision model's backend engine name.
func (e *CodebuddyExecutor) doCodebuddyChatRequestWithEmit(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	creds codebuddy.Creds,
	baseURL string,
	body []byte,
	baseModel string,
	emit func([]byte) bool,
) ([]byte, http.Header, error) {
	var err error
	body, err = sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, nil, err
	}
	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, nil, err
	}
	// Clamp oversized max_tokens (Cursor sends 65536) to the model's declared
	// ceiling, matching the normal request path.
	body = clampCodebuddyMaxTokens(body, gjson.GetBytes(body, "model").String())

	// Normalize tool-related message fields so the strict backend does not
	// reject accumulated tool-calling rounds with 400 invalid_parameter_value,
	// matching the normal request path.
	body, err = normalizeCodebuddyToolMessages(body)
	if err != nil {
		return nil, nil, err
	}

	url := baseURL + codebuddy.ChatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	applyCodebuddyHeaders(httpReq, creds)
	httpReq.Header.Set("Accept", "text/event-stream")

	// Diagnostic: dump the agentic sub-request body (redacted) so the exact
	// tools/tool_choice/max_tokens the loop sends can be inspected on failure.
	helps.DumpCodebuddyDebugBody("agentic-request", body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.DumpCodebuddyDebugBody("agentic-error", b)
		log.Warnf("codebuddy vision agentic: round request failed status=%d body=%s",
			httpResp.StatusCode, summarize(b))
		return nil, httpResp.Header.Clone(), statusErr{code: codebuddyEffectiveStatus(httpResp.StatusCode, b), msg: string(b)}
	}

	lines := make([][]byte, 0, 64)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		rawLine := scanner.Bytes()
		// Diagnostic: dump the vision sub-model's raw SSE line (redacted) so the
		// exact field placement of thinking (reasoning_content vs content) and
		// the usage token shape can be confirmed. This dump intentionally uses the
		// raw line so it is unaffected by stripping.
		helps.DumpCodebuddyDebugBody("vision-raw-sse", rawLine)

		// Strip thinking from the line before it enters either the aggregation
		// buffer (collectChatCompletion) or the client-forwarding path, so any
		// thinking that landed inside delta.content never reaches the text-only
		// model's tool result nor the client.
		cleaned, drop := stripVisionThinkingLine(rawLine)
		if drop {
			continue
		}
		lines = append(lines, cleaned)

		if emit != nil {
			// Forward only visible content deltas to the client. role/reasoning/
			// usage/finish lines are skipped so the client only sees answer text.
			trimmed := bytes.TrimSpace(cleaned)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) && gjson.ValidBytes(payload) {
					for _, ch := range gjson.GetBytes(payload, "choices").Array() {
						delta := ch.Get("delta")
						if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.String() != "" {
							// Rewrite the chunk's model field to baseModel so the
							// client sees a single consistent model name.
							outLine := trimmed
							if baseModel != "" {
								if rewritten, errSet := sjson.SetBytes(payload, "model", baseModel); errSet == nil {
									outLine = append([]byte("data: "), rewritten...)
								}
							}
							if !emit(outLine) {
								return nil, httpResp.Header.Clone(), ctx.Err()
							}
							break
						}
					}
				}
			}
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, httpResp.Header.Clone(), errScan
	}
	return collectChatCompletion(lines), httpResp.Header.Clone(), nil
}

// stripVisionThinkingLine cleans a single vision sub-model SSE line so that any
// thinking text that landed inside delta.content (as opposed to the standard
// delta.reasoning_content field) is removed before the line is either forwarded
// to the client or aggregated by collectChatCompletion.
//
// It returns:
//   - cleaned: the line with thinking-only content deltas emptied out. When no
//     stripping applies, cleaned is a clone of line so the caller can buffer it
//     without aliasing scanner memory.
//   - drop: true when the whole line carries nothing but thinking and should be
//     discarded entirely (no visible content, no structural fields worth keeping).
//
// The function is a pure transform with no network/context dependency, so its
// rules are exhaustively unit-testable.
//
// Rules (in priority order):
//   - A. A delta carrying non-empty reasoning_content and no non-empty content is
//     thinking-only and is dropped. (This mirrors the existing emit guard.)
//   - B. A delta with empty content is left structurally intact (finish_reason,
//     usage, etc. survive) but contributes no visible text.
//   - C. content text is passed through stripThinkingMarkersFromContent, a
//     conservative marker table that is currently empty (defensive extension
//     point only). hy3-preview reports thinking via reasoning_content, so no
//     content-level marker has been confirmed; we do NOT guess to avoid stripping
//     legitimate image descriptions.
func stripVisionThinkingLine(line []byte) (cleaned []byte, drop bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return bytes.Clone(line), false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
		return bytes.Clone(line), false
	}

	choices := gjson.GetBytes(payload, "choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		return bytes.Clone(line), false
	}

	changed := false
	hasVisibleContent := false
	for i, ch := range choices.Array() {
		delta := ch.Get("delta")
		reasoning := delta.Get("reasoning_content")
		hasReasoning := reasoning.Exists() && reasoning.Type == gjson.String && reasoning.String() != ""

		content := delta.Get("content")
		hasContent := content.Exists() && content.Type == gjson.String && content.String() != ""

		if hasContent {
			// Rule C: strip confirmed thinking markers from content (currently a
			// no-op table; see stripThinkingMarkersFromContent).
			stripped := stripThinkingMarkersFromContent(content.String())
			if stripped != content.String() {
				changed = true
				payload, _ = sjson.SetBytes(payload, fmt.Sprintf("choices.%d.delta.content", i), stripped)
			}
			if strings.TrimSpace(stripped) != "" {
				hasVisibleContent = true
			}
		} else if hasReasoning {
			// Rule A: reasoning-only delta (no visible content) → mark for drop.
			changed = true
		}
	}

	if !hasVisibleContent {
		// The line holds only thinking (reasoning-only deltas and/or content that
		// emptied out after stripping) — nothing worth keeping.
		return nil, true
	}
	if !changed {
		return bytes.Clone(line), false
	}
	return append([]byte("data: "), payload...), false
}

// stripThinkingMarkersFromContent removes thinking wrapper markers from a content
// string. It is intentionally conservative: only text bounded by an explicitly
// listed marker pair is stripped, so normal image-description text is never
// touched.
//
// The marker table is currently empty. hy3-preview reports thinking via the
// dedicated reasoning_content field, so no content-level marker has been
// confirmed; guessing would risk stripping legitimate answer text. Add a marker
// pair here once a real wrapper is captured in the vision-raw-sse diagnostic
// dump.
func stripThinkingMarkersFromContent(content string) string {
	type markerPair struct{ open, close string }

	// Known thinking wrappers observed across reasoning-capable backends. A pair
	// with an empty close acts as a sentinel prefix: everything up to and
	// including the sentinel is dropped, the remainder kept.
	markers := []markerPair{
		// Example once confirmed (do not enable without a real dump):
		// {"<thinking>", "</thinking>"},
	}

	result := content
	for _, m := range markers {
		if m.close == "" {
			if idx := strings.Index(result, m.open); idx >= 0 {
				result = result[idx+len(m.open):]
			}
			continue
		}
		for {
			start := strings.Index(result, m.open)
			if start < 0 {
				break
			}
			end := strings.Index(result[start+len(m.open):], m.close)
			if end < 0 {
				break
			}
			end += start + len(m.open) + len(m.close)
			result = result[:start] + result[end:]
		}
	}
	return result
}

// inspectCodebuddyImage asks the vision model a specific question about a single
// image, returning the model's answer text.
func (e *CodebuddyExecutor) inspectCodebuddyImage(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	creds codebuddy.Creds,
	baseURL string,
	imagePart []byte,
	question string,
	visionModel string,
	baseModel string,
) (string, usage.Detail, error) {
	return e.inspectCodebuddyImageWithEmit(ctx, auth, creds, baseURL, imagePart, question, visionModel, baseModel, nil)
}

// inspectCodebuddyImageWithEmit is inspectCodebuddyImage with an optional
// streaming sink that receives the vision model's content deltas as they arrive.
// baseModel is used to rewrite the forwarded chunk's model field.
func (e *CodebuddyExecutor) inspectCodebuddyImageWithEmit(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	creds codebuddy.Creds,
	baseURL string,
	imagePart []byte,
	question string,
	visionModel string,
	baseModel string,
	emit func([]byte) bool,
) (string, usage.Detail, error) {
	userContent := []any{
		json.RawMessage(imagePart),
		map[string]any{"type": "text", "text": question},
	}
	body := map[string]any{
		"model": visionModel,
		"messages": []any{
			map[string]any{"role": "user", "content": userContent},
		},
	}
	// Disable reasoning on the vision sub-request so the model returns only the
	// image description text. Reasoning-capable vision models (e.g. hy3-preview)
	// otherwise emit thinking content that leaks into the forwarded delta and,
	// worse, gets stored as the tool result the text-only model consumes — which
	// makes the text-only model believe the image was never actually described.
	body["reasoning_effort"] = "none"
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", usage.Detail{}, err
	}

	// Inject the system constraint so the vision model only extracts objective
	// image details and never proposes solutions/actions. This mirrors the
	// preprocess path's defaultCodebuddyVisionPrompt and keeps both paths
	// consistent about "describe, don't advise".
	bodyJSON, err = prependCodebuddySystemMessage(bodyJSON, codebuddyInspectImageSystemPrompt)
	if err != nil {
		return "", usage.Detail{}, err
	}

	aggregated, _, err := e.doCodebuddyChatRequestWithEmit(ctx, auth, creds, baseURL, bodyJSON, baseModel, emit)
	if err != nil {
		return "", usage.Detail{}, err
	}
	// Take only the visible answer text. reasoning_content (thinking) is stored
	// separately by collectChatCompletion and must never become the tool result:
	// if a reasoning-capable vision model returns only thinking and no answer, we
	// treat it as an empty answer rather than feeding the thinking back to the
	// text-only model (which would make it conclude the image was not described).
	content := strings.TrimSpace(gjson.GetBytes(aggregated, "choices.0.message.content").String())
	if content == "" {
		return "", helps.ParseOpenAIUsage(aggregated), fmt.Errorf("vision model returned empty answer")
	}
	return content, helps.ParseOpenAIUsage(aggregated), nil
}

// runAgenticLoop runs the server-side tool-calling loop: repeatedly send the
// accumulated messages to the text-only model, intercept inspect_image tool
// calls, answer them via the vision model, and continue until the model stops
// calling tools or the round limit is reached. It returns the final aggregated
// ChatCompletion JSON and the last upstream response headers.
func (e *CodebuddyExecutor) runAgenticLoop(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	creds codebuddy.Creds,
	baseURL string,
	body []byte,
	images []codebuddyAgenticImageRef,
	visionModel string,
	baseModel string,
	maxRounds int,
	reporter *helps.UsageReporter,
	heartbeat func(),
	emit func([]byte) bool,
) ([]byte, http.Header, error) {
	var lastAggregated []byte
	var lastHeaders http.Header
	var mainUsage usage.Detail
	var visionUsage usage.Detail

	// beat emits a downstream keep-alive if the caller supplied one. It is nil
	// safe so the non-streaming path can pass nil.
	beat := func() {
		if heartbeat != nil {
			heartbeat()
		}
	}

	// Publish the usage of the agentic loop exactly once per model, regardless
	// of which return path terminates the loop. The text-only model's usage is
	// published as the main record, and the vision sub-model's usage as a
	// separate additional-model record so both credit/token sets are visible.
	defer func() {
		if reporter != nil {
			helps.DumpCodebuddyDebugBody("vision-reporter-publish",
				[]byte(fmt.Sprintf("model=%s inputTokens=%d outputTokens=%d credit=%v visionInputTokens=%d visionOutputTokens=%d visionCredit=%v",
					reporter.Model(), mainUsage.InputTokens, mainUsage.OutputTokens, mainUsage.Credit,
					visionUsage.InputTokens, visionUsage.OutputTokens, visionUsage.Credit)))
			reporter.Publish(ctx, mainUsage)
			reporter.PublishAdditionalModelAlways(ctx, visionModel, visionUsage)
		}
	}()

	for round := 0; round < maxRounds; round++ {
		beat()
		aggregated, headers, err := e.doCodebuddyChatRequest(ctx, auth, creds, baseURL, body)
		beat()
		if err != nil {
			if lastAggregated != nil {
				return lastAggregated, lastHeaders, nil
			}
			return nil, nil, err
		}
		lastAggregated = aggregated
		lastHeaders = headers

		// Accumulate the text-only model's usage into the main record.
		addCodebuddyAgenticUsage(&mainUsage, helps.ParseOpenAIUsage(aggregated))

		toolCalls := gjson.GetBytes(aggregated, "choices.0.message.tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			// No tool calls: this is the final answer.
			return aggregated, headers, nil
		}

		// Append the assistant message (carrying tool_calls) first.
		assistantRaw := gjson.GetBytes(aggregated, "choices.0.message").Raw
		body, err = appendAgenticMessage(body, []byte(assistantRaw))
		if err != nil {
			return aggregated, headers, nil
		}

		handled := false
		for _, tc := range toolCalls.Array() {
			fn := tc.Get("function")
			if fn.Get("name").String() != inspectImageToolName {
				continue
			}
			handled = true
			toolCallID := tc.Get("id").String()
			argsStr := fn.Get("arguments").String()
			argsParsed := gjson.Parse(argsStr)
			imageID := int(argsParsed.Get("image_id").Int())
			question := argsParsed.Get("question").String()

			var answer string
			if imageID >= 1 && imageID <= len(images) {
				beat()
				a, inspectUsage, inspectErr := e.inspectCodebuddyImageWithEmit(
					ctx, auth, creds, baseURL, images[imageID-1].partJSON, question, visionModel, baseModel, emit,
				)
				beat()
				if inspectErr != nil {
					answer = fmt.Sprintf("[图片查看失败: %v]", inspectErr)
					log.Warnf("codebuddy vision agentic: inspect_image(%d) failed: %v", imageID, inspectErr)
				} else {
					answer = a
					// Accumulate the vision sub-model usage into its own record.
					addCodebuddyAgenticUsage(&visionUsage, inspectUsage)
				}
			} else {
				answer = fmt.Sprintf("[无效的图片编号 %d，可用范围 1-%d]", imageID, len(images))
			}

			toolMsgJSON, err := json.Marshal(map[string]any{
				"role":         "tool",
				"tool_call_id": toolCallID,
				"content":      answer,
			})
			if err != nil {
				continue
			}
			body, err = appendAgenticMessage(body, toolMsgJSON)
			if err != nil {
				return aggregated, headers, nil
			}
		}

		if !handled {
			// Tool calls present but none is inspect_image: return as-is.
			return aggregated, headers, nil
		}
	}

	// Round limit reached: return the last accumulated result.
	if lastAggregated != nil {
		return lastAggregated, lastHeaders, nil
	}
	return nil, nil, fmt.Errorf("codebuddy vision agentic: no result after %d rounds", maxRounds)
}

// addCodebuddyAgenticUsage accumulates a single round's usage into the running
// total for the vision sub-agent loop.
func addCodebuddyAgenticUsage(total *usage.Detail, add usage.Detail) {
	if total == nil {
		return
	}
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.ReasoningTokens += add.ReasoningTokens
	total.CachedTokens += add.CachedTokens
	total.CacheReadTokens += add.CacheReadTokens
	total.CacheCreationTokens += add.CacheCreationTokens
	total.TotalTokens += add.TotalTokens
	total.Credit += add.Credit
	total.TokenBreakdown.TotalTokens += add.TokenBreakdown.TotalTokens
	total.TokenBreakdown.Input.TotalTokens += add.TokenBreakdown.Input.TotalTokens
	total.TokenBreakdown.Input.UncachedTokens += add.TokenBreakdown.Input.UncachedTokens
	total.TokenBreakdown.Input.CacheReadTokens += add.TokenBreakdown.Input.CacheReadTokens
	total.TokenBreakdown.Input.CacheWriteTokens += add.TokenBreakdown.Input.CacheWriteTokens
	total.TokenBreakdown.Output.TotalTokens += add.TokenBreakdown.Output.TotalTokens
	total.TokenBreakdown.Output.NonReasoningTokens += add.TokenBreakdown.Output.NonReasoningTokens
	total.TokenBreakdown.Output.ReasoningTokens += add.TokenBreakdown.Output.ReasoningTokens
	total.TokenBreakdown.UnclassifiedTokens += add.TokenBreakdown.UnclassifiedTokens
}

// executeCodebuddyVisionAgentic runs the agentic loop for non-streaming Execute
// and translates the final aggregated payload back to the client format.
func (e *CodebuddyExecutor) executeCodebuddyVisionAgentic(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
	baseModel string,
	creds codebuddy.Creds,
	baseURL string,
	reporter *helps.UsageReporter,
) (cliproxyexecutor.Response, error) {
	visionCfg := e.cfg.CodebuddyVision
	visionModel := visionCfg.VisionModel()
	maxRounds := visionCfg.MaxVisionToolRounds()

	body, images, err := extractCodebuddyImagesForAgentic(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if len(images) == 0 {
		return cliproxyexecutor.Response{}, fmt.Errorf("codebuddy vision agentic: no images to inspect")
	}
	body = injectCodebuddyInspectTool(body, len(images))

	log.Infof("codebuddy vision agentic: %d images, model=%s, vision=%s, maxRounds=%d",
		len(images), baseModel, visionModel, maxRounds)

	aggregated, headers, err := e.runAgenticLoop(ctx, auth, creds, baseURL, body, images, visionModel, baseModel, maxRounds, reporter, nil, nil)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	respFrom := sdktranslator.FromString("openai")
	respTo := opts.SourceFormat
	var param any
	out := sdktranslator.TranslateNonStream(ctx, respFrom, respTo, req.Model, opts.OriginalRequest, body, aggregated, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

// executeCodebuddyVisionAgenticStream runs the agentic loop in a background
// goroutine and returns immediately, emitting an initial chunk first so the
// relay's stream-open watchdog (default 10s) is satisfied even though the loop
// itself may take 10-30s (multiple deepseek + hy3 round-trips).
func (e *CodebuddyExecutor) executeCodebuddyVisionAgenticStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	body []byte,
	baseModel string,
	creds codebuddy.Creds,
	baseURL string,
	reporter *helps.UsageReporter,
) (*cliproxyexecutor.StreamResult, error) {
	visionCfg := e.cfg.CodebuddyVision
	visionModel := visionCfg.VisionModel()
	maxRounds := visionCfg.MaxVisionToolRounds()

	// Extract images + inject tool synchronously (fast, <1s).
	body, images, err := extractCodebuddyImagesForAgentic(body)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("codebuddy vision agentic: no images to inspect")
	}
	body = injectCodebuddyInspectTool(body, len(images))

	log.Infof("codebuddy vision agentic (stream): %d images, model=%s, vision=%s, maxRounds=%d",
		len(images), baseModel, visionModel, maxRounds)

	respFrom := sdktranslator.FromString("openai")
	respTo := opts.SourceFormat

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var param any
		emit := func(line []byte) bool {
			chunks := sdktranslator.TranslateStream(ctx, respFrom, respTo, req.Model, opts.OriginalRequest, body, line, &param)
			for _, c := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: c}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		// 1. Open the stream immediately (role delta) so the relay watchdog
		//    does not time out while the loop runs.
		initChunk := buildCodebuddyVisionChunk("", baseModel, 0, nil, "assistant")
		if initChunk != nil && !emit(initChunk) {
			return
		}

		// heartbeat keeps the downstream SSE connection alive while the agentic
		// loop performs its multi-round (10-30s) upstream calls without emitting
		// any visible content. It sends an SSE comment keep-alive so the relay's
		// stream idle watchdog does not trip mid-loop.
		heartbeat := func() {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: []byte(": keep-alive\n\n")}:
			case <-ctx.Done():
			}
		}

		// 2. Run the loop (blocking; may take 10-30s). The vision sub-agent's
		//    inspect_image answers are streamed to the client in real time via
		//    inspectEmit, replacing the old post-loop pseudo-replay.
		inspectEmit := func(line []byte) bool {
			// Forward the vision model's raw content delta through the same
			// translator/emit path so the client sees it as assistant content.
			return emit(line)
		}
		aggregated, _, loopErr := e.runAgenticLoop(ctx, auth, creds, baseURL, body, images, visionModel, baseModel, maxRounds, reporter, heartbeat, inspectEmit)
		if loopErr != nil || aggregated == nil {
			// Surface the failure as visible assistant content instead of a
			// silent empty response, and log the full error for diagnosis.
			errText := "codebuddy 视觉代理失败，未能获取图片描述"
			if loopErr != nil {
				errText = fmt.Sprintf("codebuddy 视觉代理失败: %s", summarize([]byte(loopErr.Error())))
				log.Errorf("codebuddy vision agentic (stream): loop failed: %v", loopErr)
			} else {
				log.Errorf("codebuddy vision agentic (stream): loop returned no result")
			}
			errChunk, errJSON := json.Marshal(map[string]any{
				"id": "", "object": "chat.completion.chunk", "created": 0, "model": baseModel,
				"choices": []any{
					map[string]any{"index": 0, "delta": map[string]any{"content": errText}, "finish_reason": nil},
				},
			})
			if errJSON == nil {
				if !emit(append([]byte("data: "), errChunk...)) {
					return
				}
			}
			emit([]byte("data: [DONE]"))
			return
		}

		// 3. Replay the final content pseudo-streamed.
		id := gjson.GetBytes(aggregated, "id").String()
		model := gjson.GetBytes(aggregated, "model").String()
		created := gjson.GetBytes(aggregated, "created").Int()
		content := gjson.GetBytes(aggregated, "choices.0.message.content").String()

		runes := []rune(content)
		const chunkSize = 8
		for i := 0; i < len(runes); i += chunkSize {
			end := i + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			delta := string(runes[i:end])
			chunkJSON, err := json.Marshal(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{
					map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil},
				},
			})
			if err != nil {
				continue
			}
			if !emit(append([]byte("data: "), chunkJSON...)) {
				return
			}
		}

		// Terminal finish chunk.
		finishJSON, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			},
		})
		emit(append([]byte("data: "), finishJSON...))
		emit([]byte("data: [DONE]"))
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}
