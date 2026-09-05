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

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	log "github.com/sirupsen/logrus"
)

// openAICompatVisionNeedsPreprocess reports whether an OpenAI-compatible request
// should be handled by the preprocess strategy (describe images first via the
// vision model, then continue with the original text-only model). It mirrors the
// routing decision of openAICompatVisionPlan without performing any I/O, so the
// streaming path can defer the (blocking, ~seconds) vision call into the stream
// goroutine. It is the OpenAI-compat counterpart of codebuddyVisionNeedsPreprocess.
func (e *OpenAICompatExecutor) openAICompatVisionNeedsPreprocess(body []byte, baseModel string) bool {
	visionCfg := e.cfg.CodebuddyVision
	mode := visionCfg.NormalizedVisionMode()
	if mode != config.CodebuddyVisionModePreprocess {
		return false
	}
	if !codebuddyChatHasImageInput(body) {
		return false
	}
	visionModel := visionCfg.VisionModel()
	currentModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if currentModel == "" {
		currentModel = strings.TrimSpace(baseModel)
	}
	// Never re-route the vision engine itself (avoids recursion).
	if strings.EqualFold(strings.TrimSpace(currentModel), strings.TrimSpace(visionModel)) {
		return false
	}
	return true
}

// rewriteOpenAICompatHistoricalImages replaces image parts in historical
// messages (before the current turn) with a text marker, mirroring
// rewriteCodebuddyHistoricalImagesForTextModel. OpenAI-compatible providers
// have no native-vision capability registry, so every non-vision-engine model
// is treated as text-only (consistent with openAICompatVisionPlan being
// invoked with currentSupportsImages=false).
func (e *OpenAICompatExecutor) rewriteOpenAICompatHistoricalImages(body []byte, baseModel string) []byte {
	visionCfg := e.cfg.CodebuddyVision
	mode := visionCfg.NormalizedVisionMode()
	if mode != config.CodebuddyVisionModePreprocess && mode != config.CodebuddyVisionModeRouting {
		return body
	}
	currentModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if currentModel == "" {
		currentModel = strings.TrimSpace(baseModel)
	}
	if strings.EqualFold(currentModel, strings.TrimSpace(visionCfg.VisionModel())) {
		return body
	}
	body = replaceCodebuddyHistoricalImagesWithText(body, codebuddyHistoricalImageText)
	// Truncated stubs inside the current turn get the same treatment (mirrors
	// rewriteCodebuddyHistoricalImagesForTextModel).
	body = replaceCodebuddyCurrentTurnStubsWithText(body, codebuddyHistoricalImageText)
	return body
}

// openAICompatVisionPlan is a pure function deciding how the vision-proxy layer
// should handle an OpenAI-compatible request. It mirrors codebuddyVisionPlan.
func openAICompatVisionPlan(mode, visionModel, currentModel string, hasImage, currentSupportsImages bool) codebuddyVisionAction {
	if mode != config.CodebuddyVisionModeRouting && mode != config.CodebuddyVisionModePreprocess {
		return codebuddyVisionPassThrough
	}
	if !hasImage {
		return codebuddyVisionPassThrough
	}
	if strings.EqualFold(strings.TrimSpace(currentModel), strings.TrimSpace(visionModel)) {
		return codebuddyVisionPassThrough
	}
	if currentSupportsImages {
		return codebuddyVisionPassThrough
	}
	if mode == config.CodebuddyVisionModePreprocess {
		return codebuddyVisionPreprocess
	}
	return codebuddyVisionRoute
}

// applyOpenAICompatVisionProxy is the OpenAI-compat counterpart of
// applyCodebuddyVisionProxy. It rewrites image input for non-vision models. In
// routing mode it swaps the model; in preprocess mode it calls the vision model
// (hy4-preview by default) via the same OpenAI-compatible upstream credentials,
// then swaps image parts for the returned text descriptions. On failure it
// degrades to omitting images rather than failing the whole request.
//
// When emit is non-nil (streaming path), each vision description delta is
// forwarded through it so the user can watch the image being described in real
// time; otherwise deltas are only aggregated.
func (e *OpenAICompatExecutor) applyOpenAICompatVisionProxy(ctx context.Context, auth *cliproxyauth.Auth, body []byte, baseModel string, emit func([]byte) bool, reporter *helps.UsageReporter) ([]byte, bool) {
	visionCfg := e.cfg.CodebuddyVision
	mode := visionCfg.NormalizedVisionMode()
	if mode == config.CodebuddyVisionModeOff {
		return body, false
	}
	if !codebuddyChatHasImageInput(body) {
		return body, false
	}

	visionModel := visionCfg.VisionModel()
	currentModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if currentModel == "" {
		currentModel = strings.TrimSpace(baseModel)
	}

	action := openAICompatVisionPlan(mode, visionModel, currentModel, true, false)
	switch action {
	case codebuddyVisionRoute:
		log.Infof("openai compat vision proxy: routing %s -> %s", currentModel, visionModel)
		return rewriteCodebuddyModel(body, visionModel), true

	case codebuddyVisionPreprocess:
		descriptions, visionUsage, err := e.describeOpenAICompatImages(ctx, auth, body, visionModel, visionCfg.PreprocessPrompt, baseModel, emit)
		if err != nil {
			log.Warnf("openai compat vision proxy: preprocess failed for %s (vision=%s): %v; omitting images", currentModel, visionModel, err)
			return replaceCodebuddyImagesWithText(body, codebuddyOmittedImageText), true
		}
		log.Infof("openai compat vision proxy: preprocessed %d image(s) for %s via %s", len(descriptions), currentModel, visionModel)
		if reporter != nil {
			reporter.PublishAdditionalModelAlways(ctx, visionModel, visionUsage)
		}
		return replaceCodebuddyImagesWithDescriptions(body, descriptions, codebuddyOmittedImageText), true

	default:
		return body, false
	}
}

// describeOpenAICompatImages describes every image in the current turn, one at a
// time (serial), via the vision model using the same OpenAI-compatible upstream
// credentials as the main request. It returns one description per image, in body
// order, plus the accumulated vision-model usage across all images. It is the
// OpenAI-compat counterpart of describeImagesWithVisionModel.
func (e *OpenAICompatExecutor) describeOpenAICompatImages(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	body []byte,
	visionModel, prompt string,
	baseModel string,
	emit func([]byte) bool,
) ([]string, usage.Detail, error) {
	question := codebuddyUserQuestion(body)
	fullPrompt := buildCodebuddyVisionPrompt(prompt, question)

	images := extractCodebuddyCurrentImages(body)
	if len(images) == 0 {
		return nil, usage.Detail{}, fmt.Errorf("vision model called with no images")
	}

	var totalUsage usage.Detail
	descriptions := make([]string, 0, len(images))
	for i, img := range images {
		desc, u, err := e.describeOpenAICompatSingleImage(ctx, auth, body, visionModel, fullPrompt, baseModel, img, emit)
		if err != nil {
			log.Warnf("openai compat vision proxy: image %d/%d description failed (vision=%s): %v", i+1, len(images), visionModel, err)
			descriptions = append(descriptions, "")
			continue
		}
		addCodebuddyVisionUsage(&totalUsage, u)
		descriptions = append(descriptions, desc)
	}
	return descriptions, totalUsage, nil
}

// describeOpenAICompatSingleImage sends a single image to the vision model via
// the OpenAI-compatible upstream and returns its text description plus the
// vision-model usage for that call. The request body is rebuilt so that only the
// target image (plus the injected user question, if any) is sent.
func (e *OpenAICompatExecutor) describeOpenAICompatSingleImage(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	body []byte,
	visionModel, prompt string,
	baseModel string,
	img codebuddyImagePart,
	emit func([]byte) bool,
) (string, usage.Detail, error) {
	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return "", usage.Detail{}, fmt.Errorf("missing provider baseURL for vision model")
	}

	question := codebuddyUserQuestion(body)
	userContent := []any{json.RawMessage(img.raw)}
	if question != "" {
		userContent = append(userContent, map[string]any{"type": "text", "text": question})
	}
	reqBody := map[string]any{
		"model": visionModel,
		"messages": []any{
			map[string]any{"role": "user", "content": userContent},
		},
	}
	descBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", usage.Detail{}, err
	}
	descBody, err = prependCodebuddySystemMessage(descBody, prompt)
	if err != nil {
		return "", usage.Detail{}, err
	}
	descBody, err = sjson.SetBytes(descBody, "stream", true)
	if err != nil {
		return "", usage.Detail{}, err
	}
	descBody, err = sjson.SetBytes(descBody, "stream_options.include_usage", true)
	if err != nil {
		return "", usage.Detail{}, err
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(descBody))
	if err != nil {
		return "", usage.Detail{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat-vision")
	httpReq.Header.Set("Accept", "text/event-stream")

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", usage.Detail{}, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		return "", usage.Detail{}, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	var (
		id            string
		created       int64
		content       strings.Builder
		firstEmitDone bool
		usageDetail   usage.Detail
	)
	// The vision chunk's model field is always rewritten to baseModel so the
	// client sees a single consistent model from start to finish.
	model := baseModel
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(payload) {
			continue
		}
		if u, ok := helps.ParseOpenAIStreamUsage(line); ok {
			addCodebuddyVisionUsage(&usageDetail, u)
		}
		res := gjson.ParseBytes(payload)
		if id == "" {
			id = res.Get("id").String()
		}
		if created == 0 {
			created = res.Get("created").Int()
		}
		for _, ch := range res.Get("choices").Array() {
			delta := ch.Get("delta")
			if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.String() != "" {
				content.WriteString(c.String())
				if emit != nil {
					if !firstEmitDone {
						firstEmitDone = true
						roleChunk := buildCodebuddyVisionChunk(id, model, created, nil, "assistant")
						if !emit(roleChunk) {
							return "", usage.Detail{}, ctx.Err()
						}
					}
					contentStr := c.String()
					contentChunk := buildCodebuddyVisionChunk(id, model, created, &contentStr, "")
					if !emit(contentChunk) {
						return "", usage.Detail{}, ctx.Err()
					}
				}
			}
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return "", usage.Detail{}, errScan
	}

	desc := strings.TrimSpace(content.String())
	if desc == "" {
		return "", usage.Detail{}, fmt.Errorf("vision model returned empty description")
	}
	return desc, usageDetail, nil
}
