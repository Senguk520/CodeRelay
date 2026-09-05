package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// providerGatewayVisionPrompt is the system constraint injected into every
// provider-gateway vision sub-request. It mirrors the CodeBuddy preprocess
// prompt wording: extract only objective image details and never propose
// solutions or actions, so the pure-text main model keeps full control over
// reasoning.
const providerGatewayVisionPrompt = "你是一个图片细节提取助手。请只客观、准确、详细地描述图片中实际存在的内容（文字、数字、位置、颜色、图表数据等）。不要提出任何解决方案、修改建议、操作步骤、分析判断或额外评论。"

// providerGatewayImagePart is a single image part located inside the request
// body, along with its gjson path so it can be replaced in place.
type providerGatewayImagePart struct {
	path string // e.g. "input.0.content.1" or "messages.0.content.1"
	raw  []byte // raw JSON of the image part
}

// providerGatewayVisionBackfill describes the visual model to use for
// image-to-text preprocessing on a provider gateway request. When VisionModel
// is empty, the backfill layer is disabled and the request falls through to the
// existing omit/route logic unchanged.
func providerGatewayVisionBackfillEnabled(gateway *providerGatewaySpec) bool {
	return gateway != nil && strings.TrimSpace(gateway.VisionModel) != ""
}

// providerGatewayDescribeAndBackfill runs the vision preprocess for a request
// that carries image input. It sends each image to the provider gateway's own
// vision model (via gateway.BaseURL + gateway.APIKey, i.e. the user's key),
// collects the text descriptions, and rewrites the body so every image part is
// replaced by its description. On success the returned body no longer contains
// image input; on any failure it returns (false, "") so the caller can fall back
// to the existing omit/route behavior without interrupting the request.
func (s *relayServer) providerGatewayDescribeAndBackfill(c *gin.Context, gateway *providerGatewaySpec, body []byte, sourceFormat sdktranslator.Format) (bool, []byte) {
	visionModel := strings.TrimSpace(gateway.VisionModel)
	if visionModel == "" {
		return false, body
	}
	images := providerGatewayExtractImages(body)
	if len(images) == 0 {
		return false, body
	}

	question := providerGatewayUserQuestion(body)
	descriptions := make([]string, 0, len(images))
	for i, img := range images {
		desc, err := s.providerGatewayDescribeImage(c, gateway, visionModel, question, img)
		if err != nil {
			log.Warnf("provider gateway vision: image %d/%d description failed (vision=%s): %v", i+1, len(images), visionModel, err)
			descriptions = append(descriptions, "")
			continue
		}
		descriptions = append(descriptions, desc)
	}

	// If every image failed to describe, fall back to the existing logic so the
	// request is still served (omitted or routed) rather than blocked.
	anyDescription := false
	for _, d := range descriptions {
		if strings.TrimSpace(d) != "" {
			anyDescription = true
			break
		}
	}
	if !anyDescription {
		return false, body
	}

	textType := "text"
	if sourceFormatEqual(sourceFormat, sdktranslator.FormatOpenAIResponse) {
		textType = "input_text"
	}
	next, err := providerGatewayReplaceImagesWithDescriptions(body, descriptions, textType, providerGatewayOmittedImageText)
	if err != nil {
		log.Warnf("provider gateway vision: backfill failed: %v", err)
		return false, body
	}
	return true, next
}

// providerGatewayDescribeImage sends a single image to the provider gateway's
// vision model and returns its text description. The request is minimal: the
// vision model plus the single image part plus the user question (if any), with
// the objective-description system prompt prepended. It uses a non-streaming
// chat completion call for simple, reliable parsing.
func (s *relayServer) providerGatewayDescribeImage(c *gin.Context, gateway *providerGatewaySpec, visionModel, question string, img providerGatewayImagePart) (string, error) {
	userContent := []any{json.RawMessage(img.raw)}
	if strings.TrimSpace(question) != "" {
		userContent = append(userContent, map[string]any{"type": "text", "text": question})
	}
	reqBody := map[string]any{
		"model": visionModel,
		"messages": []any{
			map[string]any{"role": "system", "content": providerGatewayVisionPrompt},
			map[string]any{"role": "user", "content": userContent},
		},
		"stream": false,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url, err := providerGatewayURL(gateway.BaseURL, "/v1/chat/completions")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(relayContext(c), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+gateway.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vision model returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if !gjson.ValidBytes(respBody) {
		return "", fmt.Errorf("vision model returned non-JSON response")
	}
	desc := strings.TrimSpace(gjson.GetBytes(respBody, "choices.0.message.content").String())
	if desc == "" {
		return "", fmt.Errorf("vision model returned empty description")
	}
	return desc, nil
}

// providerGatewayExtractImages walks the body and returns every image part
// (image_url / input_image) in document order with its replacement path. It
// scans both OpenAI chat-style bodies (messages[].content[]) and Responses-style
// bodies (input[].content[]), which is the union the provider gateway accepts.
func providerGatewayExtractImages(body []byte) []providerGatewayImagePart {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	var images []providerGatewayImagePart
	for _, root := range []string{"messages", "input"} {
		items := gjson.GetBytes(body, root)
		if !items.IsArray() {
			continue
		}
		items.ForEach(func(key, value gjson.Result) bool {
			content := value.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(ckey, cvalue gjson.Result) bool {
				if !isProviderGatewayImagePartType(cvalue.Get("type").String()) {
					return true
				}
				images = append(images, providerGatewayImagePart{
					path: fmt.Sprintf("%s.%s.content.%s", root, key.String(), ckey.String()),
					raw:  append([]byte(nil), []byte(cvalue.Raw)...),
				})
				return true
			})
			return true
		})
	}
	return images
}

// isProviderGatewayImagePartType reports whether a content-part type carries
// image input (OpenAI image_url or Anthropic/Responses-style input_image).
func isProviderGatewayImagePartType(typ string) bool {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "image_url", "input_image":
		return true
	default:
		return false
	}
}

// providerGatewayUserQuestion extracts the text of the last user message so the
// vision model can focus its description on what the user asked. It handles both
// chat-style (messages) and Responses-style (input) bodies.
func providerGatewayUserQuestion(body []byte) string {
	for _, root := range []string{"messages", "input"} {
		items := gjson.GetBytes(body, root)
		if !items.IsArray() {
			continue
		}
		arr := items.Array()
		lastUserIdx := -1
		for i := len(arr) - 1; i >= 0; i-- {
			role := arr[i].Get("role").String()
			if role == "" {
				// Responses-style items carry "type":"message" alongside role.
				role = arr[i].Get("role").String()
			}
			if strings.EqualFold(strings.TrimSpace(role), "user") {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx < 0 {
			continue
		}
		content := arr[lastUserIdx].Get("content")
		if content.Type == gjson.String {
			return strings.TrimSpace(content.String())
		}
		if !content.IsArray() {
			return ""
		}
		var sb strings.Builder
		for _, part := range content.Array() {
			if part.Get("type").String() != "text" && part.Get("type").String() != "input_text" {
				continue
			}
			text := part.Get("text").String()
			if text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(text)
		}
		return sb.String()
	}
	return ""
}

// providerGatewayReplaceImagesWithDescriptions rewrites the body so each image
// part is replaced by a text part carrying its description (or the fallback text
// when a description is empty). It operates on the same path layout produced by
// providerGatewayExtractImages.
func providerGatewayReplaceImagesWithDescriptions(body []byte, descriptions []string, textType, fallbackText string) ([]byte, error) {
	out := body
	idx := 0
	for _, root := range []string{"messages", "input"} {
		items := gjson.GetBytes(out, root)
		if !items.IsArray() {
			continue
		}
		items.ForEach(func(key, value gjson.Result) bool {
			content := value.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(ckey, cvalue gjson.Result) bool {
				if !isProviderGatewayImagePartType(cvalue.Get("type").String()) {
					return true
				}
				text := fallbackText
				if idx < len(descriptions) && strings.TrimSpace(descriptions[idx]) != "" {
					text = descriptions[idx]
				}
				idx++
				path := fmt.Sprintf("%s.%s.content.%s", root, key.String(), ckey.String())
				replacement, err := json.Marshal(map[string]string{"type": textType, "text": text})
				if err != nil {
					return false
				}
				out, err = sjson.SetRawBytes(out, path, replacement)
				if err != nil {
					return false
				}
				return true
			})
			return true
		})
	}
	return out, nil
}
