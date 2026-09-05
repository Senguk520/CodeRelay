package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestRelayServerExecutesNonStreamingRequestThroughRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"ok":true}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.streamCalls != 0 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastReq.Model != "gpt-5.5" || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("unexpected executor request: %#v %#v", runtime.lastReq, runtime.lastOpts)
	}
	if runtime.lastOpts.Headers.Get("Authorization") != "Bearer client-key" {
		t.Fatalf("request headers should be forwarded to CPA executor")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS header should match CPA server behavior")
	}
}

func TestRelayServerRejectsGPTImageModelsOnChatCompletionsBeforeRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{}
	apiKey := &apiKeySpec{
		ID:          "key_1",
		Label:       "Test key",
		Key:         "client-key",
		ModelPrefix: "team",
		Enabled:     true,
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{*apiKey},
		ModelIDs: []string{"gpt-5.5", "gpt-image-2"},
		ModelAliases: []modelAliasSpec{{
			SourceModel: "gpt-image-2",
			Alias:       "image-latest",
		}},
		apiKeyByValue: map[string]*apiKeySpec{"client-key": apiKey},
		aliasToSource: map[string]string{"image-latest": "gpt-image-2"},
	}
	router := (&relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	for _, model := range []string{"team/gpt-image-2", "team/image-latest"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"draw"}]}`, model)))
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("model %q status = %d, want %d; body=%s", model, w.Code, http.StatusBadRequest, w.Body.String())
		}
		var payload struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("model %q response should be JSON: %v", model, err)
		}
		if payload.Error.Type != "invalid_request_error" || !strings.Contains(payload.Error.Message, "Chat Completions") {
			t.Fatalf("model %q unexpected error payload: %#v", model, payload.Error)
		}
	}

	if runtime.executeCalls != 0 || runtime.streamCalls != 0 {
		t.Fatalf("image-only models must be rejected before runtime scheduling: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
}

func TestRelayServerAcceptsCodexAutoReviewModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"ok":true}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"codex-auto-review","input":"allow?","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastReq.Model != codexAutoReviewModel {
		t.Fatalf("auto review request should be forwarded unchanged: calls=%d req=%#v", runtime.executeCalls, runtime.lastReq)
	}
}

func TestRelayServerModelsExposeCodexAutoReview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), codexAutoReviewModel) {
		t.Fatalf("models response should expose auto review model: %s", w.Body.String())
	}
}

func TestRelayServerResetAuthStateClearsSelectedAccountCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-1.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.5": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(30 * time.Minute),
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	spec := &apiKeySpec{ID: "key_1", Key: "client-key", Enabled: true, AccountIDs: []string{"account-1"}}
	account := &accountSpec{ID: "account-1", AuthID: "auth-1.json"}
	m := &manifest{
		APIKeys:       []apiKeySpec{*spec},
		Accounts:      []accountSpec{*account},
		apiKeyByValue: map[string]*apiKeySpec{"client-key": spec},
		accountByID:   map[string]*accountSpec{"account-1": account},
	}
	router := (&relayServer{
		runtime:     &fakeRuntime{},
		cfg:         &config.Config{},
		manifest:    m,
		authManager: manager,
		policy:      &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/coderelay/auth/reset", strings.NewReader(`{"accountIds":["account-1"]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	updated, ok := manager.GetByID("auth-1.json")
	if !ok || updated == nil || len(updated.ModelStates) != 0 || updated.Unavailable {
		t.Fatalf("auth state was not reset: %#v", updated)
	}
}

func TestRelayServerResetSchedulerStateAcceptsAPIKeyAccountWithoutAuthID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "api-auth-1",
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "upstream-key"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.5": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(30 * time.Minute),
			},
		},
	}); err != nil {
		t.Fatalf("register API-key auth: %v", err)
	}
	spec := &apiKeySpec{
		ID:         "key_1",
		Key:        "client-key",
		Enabled:    true,
		AccountIDs: []string{"api-account-1"},
	}
	account := &accountSpec{
		ID:             "api-account-1",
		AuthKind:       "api_key",
		UpstreamAPIKey: "upstream-key",
	}
	m := &manifest{
		APIKeys:       []apiKeySpec{*spec},
		Accounts:      []accountSpec{*account},
		apiKeyByValue: map[string]*apiKeySpec{"client-key": spec},
		accountByID:   map[string]*accountSpec{"api-account-1": account},
		accountByAPIKey: map[string]*accountSpec{
			"upstream-key": account,
		},
	}
	router := (&relayServer{
		runtime:     &fakeRuntime{},
		cfg:         &config.Config{},
		manifest:    m,
		authManager: manager,
		policy:      &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/coderelay/accounts/reset-scheduler",
		strings.NewReader(`{"accountIds":["api-account-1"]}`),
	)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Reset      int      `json:"reset"`
		AccountIDs []string `json:"accountIds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if payload.Reset != 1 {
		t.Fatalf("API-key auth scheduler state was not reset: %#v", payload)
	}
	if !reflect.DeepEqual(payload.AccountIDs, []string{"api-account-1"}) {
		t.Fatalf("unexpected reset account ids: %#v", payload.AccountIDs)
	}
	updated, ok := manager.GetByID("api-auth-1")
	if !ok || updated == nil || len(updated.ModelStates) != 0 || updated.Unavailable {
		t.Fatalf("API-key auth state was not reset: %#v", updated)
	}
}

func TestRelayServerFramesStreamingChatCompletionThroughRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk, 2)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"choices":[]}`)}
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{
				"Content-Type":       []string{"application/json"},
				"Connection":         []string{"X-Remove-Me"},
				"X-Remove-Me":        []string{"secret"},
				"X-Litellm-Trace":    []string{"gateway"},
				"Content-Encoding":   []string{"gzip"},
				"X-Upstream":         []string{"ok"},
				"Access-Control-Foo": []string{"bar"},
			},
			Chunks: stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 0 || runtime.streamCalls != 1 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI || !runtime.lastOpts.Stream {
		t.Fatalf("unexpected stream options: %#v", runtime.lastOpts)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content type: %q", got)
	}
	if values := w.Header().Values("Content-Type"); len(values) != 1 {
		t.Fatalf("Content-Type should not be duplicated: %#v", values)
	}
	if w.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("upstream headers should be preserved")
	}
	if w.Header().Get("X-Remove-Me") != "" ||
		w.Header().Get("X-Litellm-Trace") != "" ||
		w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("filtered upstream headers leaked: %#v", w.Header())
	}
	if got := w.Body.String(); got != "data: {\"choices\":[]}\n\ndata: [DONE]\n\n" {
		t.Fatalf("unexpected framed stream:\n%s", got)
	}
}

func TestRelayServerTimesOutWhenStreamDoesNotOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()
	router := testRelayRouter(&fakeRuntime{streamWaitForContext: true})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stream_open") {
		t.Fatalf("timeout response should name stream_open phase: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_first_byte_timeout") {
		t.Fatalf("timeout response should expose first-byte timeout code: %s", w.Body.String())
	}
}

func TestRelayServerUsesLongOpenTimeoutForImageGenerationTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldOpenTimeout := streamOpenTimeout
	oldImageOpenTimeout := imageStreamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	imageStreamOpenTimeout = 120 * time.Millisecond
	streamOpenMaxAttempts = 1
	defer func() {
		streamOpenTimeout = oldOpenTimeout
		imageStreamOpenTimeout = oldImageOpenTimeout
		streamOpenMaxAttempts = oldAttempts
	}()
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`event: response.completed
data: {"type":"response.completed"}

`)}
	close(stream)
	runtime := &fakeRuntime{
		streamOpenDelay: 60 * time.Millisecond,
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"draw","stream":true,"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("image stream should use longer open timeout, got status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("expected one stream runtime call, got %d", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("image stream response was not forwarded: %s", w.Body.String())
	}
}

func TestRelayServerHandlesImagesGenerationsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`event: response.completed
data: {"type":"response.completed","response":{"created_at":1710000000,"output":[{"type":"image_generation_call","result":"ZmFrZS1wbmc=","output_format":"png","size":"1024x1024"}]}}

`)}
	close(stream)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 || runtime.executeCalls != 0 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastReq.Model != defaultImagesMainModel {
		t.Fatalf("image endpoint should execute via main model, got %q", runtime.lastReq.Model)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be json: %v body=%s", err, w.Body.String())
	}
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected one image result: %#v", body)
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "ZmFrZS1wbmc=" {
		t.Fatalf("unexpected image payload: %#v", body)
	}
}

func TestRelayServerRetriesWhenStreamDoesNotOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamWaitAttempts: 1,
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 2 {
		t.Fatalf("expected retry to call stream runtime twice, got %d", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("retry should stream successful second attempt: %s", w.Body.String())
	}
}

func TestRelayServerKeepsStreamContextOpenAfterOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldOpenTimeout := streamOpenTimeout
	oldIdleTimeout := streamIdleTimeout
	streamOpenTimeout = 100 * time.Millisecond
	streamIdleTimeout = time.Second
	defer func() {
		streamOpenTimeout = oldOpenTimeout
		streamIdleTimeout = oldIdleTimeout
	}()
	runtime := &fakeRuntime{
		streamResultFromContext: true,
		streamResultDelay:       20 * time.Millisecond,
		streamResultPayload:     []byte(`[DONE]`),
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("expected one stream runtime call, got %d", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream context should stay alive after opening: %s", w.Body.String())
	}
}

func TestRelayServerTimesOutIdleOpenedStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamIdleTimeout
	streamIdleTimeout = 20 * time.Millisecond
	defer func() {
		streamIdleTimeout = oldTimeout
	}()
	stream := make(chan cliproxyexecutor.StreamChunk)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream should be opened before idle timeout, got status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stream_idle") {
		t.Fatalf("idle timeout should be sent as terminal SSE error: %s", w.Body.String())
	}
}

func TestRelayServerAnthropicMessagesUsesClaudeFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatClaude || runtime.lastReq.Format != sdktranslator.FormatClaude {
		t.Fatalf("expected Claude executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
}

func TestRelayServerAnthropicCountTokensUsesClaudeShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello world"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Fatalf("Anthropic token count response should use input_tokens: %s", w.Body.String())
	}
}

func TestRelayServerGeminiGenerateInjectsPathModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-5.5:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatGemini || runtime.lastReq.Model != "gpt-5.5" {
		t.Fatalf("expected Gemini executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
	if !strings.Contains(string(runtime.lastReq.Payload), `"model":"gpt-5.5"`) {
		t.Fatalf("Gemini path model should be injected into executor payload: %s", runtime.lastReq.Payload)
	}
}

func TestRelayServerGeminiModelsResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"models/gpt-5.5"`) ||
		!strings.Contains(w.Body.String(), `"streamGenerateContent"`) ||
		!strings.Contains(w.Body.String(), `"countTokens"`) {
		t.Fatalf("Gemini models response has unexpected shape: %s", w.Body.String())
	}
}

func TestRelayServerOllamaChatConvertsNonStreamingResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI || runtime.lastReq.Model != "gpt-5.5" {
		t.Fatalf("expected OpenAI chat executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
	if !strings.Contains(w.Body.String(), `"done":true`) || !strings.Contains(w.Body.String(), `"content":"ok"`) || !strings.Contains(w.Body.String(), `"eval_count":3`) {
		t.Fatalf("Ollama response has unexpected shape: %s", w.Body.String())
	}
}

func TestRelayServerOllamaChatConvertsStreamingChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)}
	close(chunks)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  chunks,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("expected OpenAI chat stream executor request, got calls=%d opts=%#v", runtime.streamCalls, runtime.lastOpts)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected content and final Ollama chunks, got %d lines: %s", len(lines), w.Body.String())
	}
	if !strings.Contains(lines[0], `"content":"ok"`) || !strings.Contains(lines[1], `"done":true`) || !strings.Contains(lines[1], `"eval_count":3`) {
		t.Fatalf("unexpected Ollama stream body: %s", w.Body.String())
	}
}

func TestRelayServerHandlesCORSPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" ||
		w.Header().Get("Access-Control-Allow-Headers") != "*" {
		t.Fatalf("unexpected CORS headers: %#v", w.Header())
	}
}

func testRelayRouter(runtime executorRuntime) *gin.Engine {
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true}},
		ModelIDs: []string{"gpt-5.5", "gpt-image-2"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
		},
	}
	policy := &requestPolicy{manifest: m}
	return (&relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		policy:   policy,
	}).router()
}

type fakeRuntime struct {
	response                cliproxyexecutor.Response
	streamResult            *cliproxyexecutor.StreamResult
	err                     error
	streamWaitForContext    bool
	streamWaitAttempts      int
	streamResultFromContext bool
	streamOpenDelay         time.Duration
	streamResultDelay       time.Duration
	streamResultPayload     []byte

	executeCalls int
	streamCalls  int
	lastReq      cliproxyexecutor.Request
	lastOpts     cliproxyexecutor.Options

	alphaSearchStatus  int
	alphaSearchHeaders http.Header
	alphaSearchPayload []byte
	alphaSearchErr     error
	alphaSearchCalls   int
	lastAlphaModel     string
	lastAlphaBody      []byte
	lastAlphaHeaders   http.Header
}

func (r *fakeRuntime) Execute(_ context.Context, _ []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	r.executeCalls++
	r.lastReq = req
	r.lastOpts = opts
	return r.response, r.err
}

func (r *fakeRuntime) CodexAlphaSearch(_ context.Context, model string, body []byte, headers http.Header) (int, http.Header, []byte, error) {
	r.alphaSearchCalls++
	r.lastAlphaModel = model
	r.lastAlphaBody = append([]byte(nil), body...)
	if headers != nil {
		r.lastAlphaHeaders = headers.Clone()
	}
	status := r.alphaSearchStatus
	if status == 0 {
		status = http.StatusOK
	}
	payload := r.alphaSearchPayload
	if payload == nil {
		payload = []byte(`{"ok":true}`)
	}
	return status, r.alphaSearchHeaders, payload, r.alphaSearchErr
}

func (r *fakeRuntime) ExecuteStream(ctx context.Context, _ []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	r.streamCalls++
	r.lastReq = req
	r.lastOpts = opts
	if r.streamWaitForContext || r.streamCalls <= r.streamWaitAttempts {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.streamOpenDelay > 0 {
		timer := time.NewTimer(r.streamOpenDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if r.streamResultFromContext {
		stream := make(chan cliproxyexecutor.StreamChunk, 1)
		delay := r.streamResultDelay
		if delay <= 0 {
			delay = 10 * time.Millisecond
		}
		payload := r.streamResultPayload
		if len(payload) == 0 {
			payload = []byte(`[DONE]`)
		}
		go func() {
			defer close(stream)
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				stream <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}()
		return &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		}, nil
	}
	return r.streamResult, r.err
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
		_ = reader.Close()
	}()

	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return string(data)
}

func TestRelayAcceptsResponsesPathAppendedToChatCompletionsBase(t *testing.T) {
	t.Parallel()
	// Route registration only: ensure compatibility paths are not NoRoute 404.
	m := &manifest{}
	policy := &requestPolicy{manifest: m}
	router := (&relayServer{
		manifest: m,
		policy:   policy,
	}).router()
	for _, path := range []string{
		"/v1/chat/completions/v1/responses",
		"/v1/chat/completions/v1/responses/compact",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer unused")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Fatalf("path %s should not be NoRoute 404 (got %d body=%s)", path, w.Code, w.Body.String())
		}
	}
}

func TestRelayRegistersCodexLiveRoutesAndSkipsModelRewriting(t *testing.T) {
	t.Parallel()
	spec := &apiKeySpec{
		ID:            "live-key",
		Key:           "client-key",
		Enabled:       true,
		AllowedModels: []string{"gpt-5.6-sol"},
	}
	m := &manifest{
		APIKeys:       []apiKeySpec{*spec},
		ModelIDs:      []string{"gpt-5.6-sol"},
		apiKeyByValue: map[string]*apiKeySpec{spec.Key: spec},
	}
	router := (&relayServer{
		manifest: m,
		policy:   &requestPolicy{manifest: m, tokenLimiter: newAPIKeyTokenLimiter(m)},
	}).router()

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"model":"gpt-live-1-codex","sdp":"v=0"}`))
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedRecorder := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d; body=%s", unauthorizedRecorder.Code, http.StatusUnauthorized, unauthorizedRecorder.Body.String())
	}

	authorized := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"model":"gpt-live-1-codex","sdp":"v=0"}`))
	authorized.Header.Set("Authorization", "Bearer "+spec.Key)
	authorized.Header.Set("Content-Type", "application/json")
	authorizedRecorder := httptest.NewRecorder()
	router.ServeHTTP(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorized status = %d, want %d; body=%s", authorizedRecorder.Code, http.StatusServiceUnavailable, authorizedRecorder.Body.String())
	}
	if strings.Contains(authorizedRecorder.Body.String(), "model_not_available") {
		t.Fatalf("live request was rejected by text model validation: %s", authorizedRecorder.Body.String())
	}

	sideband := httptest.NewRequest(http.MethodGet, "/v1/live/call-123", nil)
	sideband.Header.Set("Authorization", "Bearer "+spec.Key)
	sidebandRecorder := httptest.NewRecorder()
	router.ServeHTTP(sidebandRecorder, sideband)
	if sidebandRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("sideband status = %d, want %d; body=%s", sidebandRecorder.Code, http.StatusServiceUnavailable, sidebandRecorder.Body.String())
	}

	realtimeCall := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader(`{"model":"gpt-live-1-codex","sdp":"v=0"}`))
	realtimeCall.Header.Set("Authorization", "Bearer "+spec.Key)
	realtimeCall.Header.Set("Content-Type", "application/json")
	realtimeRecorder := httptest.NewRecorder()
	router.ServeHTTP(realtimeRecorder, realtimeCall)
	if realtimeRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("realtime call status = %d, want %d; body=%s", realtimeRecorder.Code, http.StatusServiceUnavailable, realtimeRecorder.Body.String())
	}
}

func TestSanitizeCodexAlphaSearchBodyRemovesLocalRoutingFields(t *testing.T) {
	t.Parallel()
	body := []byte(`{"query":"hello","prompt_cache_key":"drop-me","prompt_cache_retention":"24h","id":"sess-1"}`)
	out := sanitizeCodexAlphaSearchBody(body)
	if strings.Contains(string(out), "prompt_cache_key") || strings.Contains(string(out), "prompt_cache_retention") {
		t.Fatalf("local routing fields survived: %s", out)
	}
	if !strings.Contains(string(out), `"query":"hello"`) || !strings.Contains(string(out), `"id":"sess-1"`) {
		t.Fatalf("expected search fields preserved: %s", out)
	}
}

func TestResolveCodexAlphaSearchURL(t *testing.T) {
	t.Parallel()
	if got := resolveCodexAlphaSearchURL(nil); got != defaultCodexAlphaSearchURL {
		t.Fatalf("nil auth = %q, want default", got)
	}
	auth := &coreauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/backend-api/codex/"}}
	if got := resolveCodexAlphaSearchURL(auth); got != "https://example.test/backend-api/codex/alpha/search" {
		t.Fatalf("codex base = %q", got)
	}
	auth.Attributes["base_url"] = "https://example.test/backend-api"
	if got := resolveCodexAlphaSearchURL(auth); got != "https://example.test/backend-api/codex/alpha/search" {
		t.Fatalf("backend-api base = %q", got)
	}
}

func TestRequestKindFromPathTreatsAlphaSearchAsText(t *testing.T) {
	t.Parallel()
	if got := requestKindFromPath("/v1/alpha/search"); got != "text" {
		t.Fatalf("requestKindFromPath(/v1/alpha/search) = %q, want text", got)
	}
	if got := requestKindFromPath("/backend-api/codex/alpha/search"); got != "text" {
		t.Fatalf("requestKindFromPath(direct) = %q, want text", got)
	}
}

func TestCodexAlphaSearchRouteForwardsToRuntime(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{
		alphaSearchStatus:  http.StatusOK,
		alphaSearchPayload: []byte(`{"results":[{"title":"ok"}]}`),
		alphaSearchHeaders: http.Header{"Content-Type": []string{"application/json"}},
	}
	m := &manifest{
		APIKeys: []apiKeySpec{{
			ID:      "key_1",
			Label:   "Test key",
			Key:     "client-key",
			Enabled: true,
		}},
		ModelIDs: []string{"gpt-5.6-sol"},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{
		"client-key": &m.APIKeys[0],
	}
	router := (&relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	body := `{"query":"OpenAI Codex authentication documentation","model":"gpt-5.6-sol","id":"sess-42"}`
	req := httptest.NewRequest(http.MethodPost, codexAlphaSearchPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Openai-Actor-Authorization", "actor-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if runtime.alphaSearchCalls != 1 {
		t.Fatalf("alphaSearchCalls = %d, want 1", runtime.alphaSearchCalls)
	}
	if runtime.lastAlphaModel != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", runtime.lastAlphaModel)
	}
	if !strings.Contains(string(runtime.lastAlphaBody), `"query":"OpenAI Codex authentication documentation"`) {
		t.Fatalf("body not forwarded: %s", runtime.lastAlphaBody)
	}
	if got := runtime.lastAlphaHeaders.Get("X-Session-ID"); got != "sess-42" {
		t.Fatalf("X-Session-ID = %q, want sess-42", got)
	}
	if got := runtime.lastAlphaHeaders.Get("X-Openai-Actor-Authorization"); got != "actor-token" {
		t.Fatalf("actor header = %q", got)
	}
	if !strings.Contains(w.Body.String(), `"results"`) {
		t.Fatalf("response body missing results: %s", w.Body.String())
	}
}

func TestCodexAlphaSearchDirectPathIsRegistered(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{alphaSearchPayload: []byte(`{"ok":true}`)}
	m := &manifest{
		APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true, ResponsesWebsockets: true}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  runtime,
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, codexDirectAlphaSearchPath, strings.NewReader(`{"query":"ping"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatalf("direct path should not be NoRoute 404: %s", w.Body.String())
	}
	if runtime.alphaSearchCalls != 1 {
		t.Fatalf("alphaSearchCalls = %d, want 1", runtime.alphaSearchCalls)
	}
}

func TestCodexAlphaSearchRequiresAPIKey(t *testing.T) {
	t.Parallel()
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: &manifest{},
		policy:   &requestPolicy{manifest: &manifest{}},
	}).router()
	req := httptest.NewRequest(http.MethodPost, codexAlphaSearchPath, strings.NewReader(`{"query":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesWebsocketRouteRequiresAPIKey(t *testing.T) {
	t.Parallel()
	called := false
	m := &manifest{
		APIKeys: []apiKeySpec{{
			ID:                  "key_1",
			Key:                 "client-key",
			Enabled:             true,
			ResponsesWebsockets: true,
		}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
		responsesWebsocket: func(c *gin.Context) {
			called = true
			c.Status(http.StatusSwitchingProtocols)
		},
	}).router()

	// Missing key → 401, handler not invoked.
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401 body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("websocket handler should not run without API key")
	}

	// Valid key → handler runs.
	req = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if !called {
		t.Fatal("websocket handler should run with valid API key")
	}
	if w.Code != http.StatusSwitchingProtocols {
		t.Fatalf("valid key status = %d, want 101 body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesWebsocketRouteUnavailableWithoutHandler(t *testing.T) {
	t.Parallel()
	m := &manifest{
		APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true, ResponsesWebsockets: true}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "responses websocket unavailable") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestResponsesWebsocketRouteDisabledByDefault(t *testing.T) {
	t.Parallel()
	called := false
	m := &manifest{
		APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
		responsesWebsocket: func(c *gin.Context) {
			called = true
			c.Status(http.StatusSwitchingProtocols)
		},
	}).router()

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("websocket handler should not run when disabled")
	}
	if !strings.Contains(w.Body.String(), "responses websocket is disabled") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestFilterRegistryModelsByExcludedModels(t *testing.T) {
	models := []*cliproxy.ModelInfo{
		{ID: "gpt-5.3-codex"},
		{ID: "gpt-5.3-codex-spark"},
		{ID: "gpt-5.4"},
	}
	filtered := filterRegistryModelsByExcluded(models, []string{"gpt-5.3-*"})
	if len(filtered) != 1 || filtered[0].ID != "gpt-5.4" {
		t.Fatalf("unexpected filtered models: %#v", filtered)
	}
}

func TestExcludedModelsForAuthMergesManifestAndMetadata(t *testing.T) {
	m := &manifest{
		ExcludedModels: []string{"gpt-image-*"},
		AccountModelRules: []accountModelRule{{
			AccountID:      "plus-account",
			ExcludedModels: []string{"gpt-5.3-codex-spark"},
		}},
		Accounts: []accountSpec{{
			ID:     "plus-account",
			AuthID: "plus-auth",
			Email:  "plus@example.com",
		}},
	}
	m.accountByID = map[string]*accountSpec{"plus-account": &m.Accounts[0]}
	m.accountByAuthID = map[string]*accountSpec{"plus-auth": &m.Accounts[0]}
	auth := &coreauth.Auth{
		ID: "plus-auth",
		Metadata: map[string]any{
			"excluded_models": []string{"custom-model"},
		},
		Attributes: map[string]string{
			"account_id": "plus-account",
		},
	}
	excluded := excludedModelsForAuth(m, auth)
	if len(excluded) != 3 {
		t.Fatalf("expected 3 excluded patterns, got %#v", excluded)
	}
	if !authModelExcluded(m, auth, "gpt-5.3-codex-spark") {
		t.Fatal("spark should be excluded for plus auth")
	}
	if authModelExcluded(m, auth, "gpt-5.4") {
		t.Fatal("gpt-5.4 should remain available")
	}
}

func TestRegisterManifestModelsForAuthRespectsPerAccountExclusions(t *testing.T) {
	m := &manifest{
		ModelIDs: []string{"gpt-5.3-codex", codexSparkModel, "gpt-5.4"},
		Accounts: []accountSpec{{
			ID:     "plus-account",
			AuthID: "plus-auth",
		}},
	}
	m.accountByID = map[string]*accountSpec{"plus-account": &m.Accounts[0]}
	m.accountByAuthID = map[string]*accountSpec{"plus-auth": &m.Accounts[0]}
	auth := &coreauth.Auth{
		ID: "plus-auth",
		Metadata: map[string]any{
			"excluded_models": []string{codexSparkModel},
		},
		Attributes: map[string]string{
			"account_id": "plus-account",
		},
	}
	manager := coreauth.NewManager(nil, &coderelaySelector{manifest: m}, nil)
	t.Cleanup(func() {
		cliproxy.GlobalModelRegistry().UnregisterClient(auth.ID)
	})
	registerManifestModelsForAuth(manager, m, auth)
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	for _, model := range models {
		if strings.EqualFold(model.ID, codexSparkModel) {
			t.Fatalf("spark should not be registered for excluded auth: %#v", models)
		}
	}
}

func TestCoreAuthSelectorFiltersNewModelExclusionsBeforeSessionAffinity(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityTTL = "1h"
	selector := buildCoreAuthSelector(cfg, &coreauth.RoundRobinSelector{}, &manifest{}, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		t.Cleanup(stoppable.Stop)
	}

	plus := &coreauth.Auth{
		ID:       "plus.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{},
	}
	pro := &coreauth.Auth{
		ID:       "pro.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	auths := []*coreauth.Auth{plus, pro}
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"Session_id": []string{"spark-session"}},
	}

	first, err := selector.Pick(context.Background(), "codex", codexSparkModel, opts, auths)
	if err != nil {
		t.Fatalf("initial Pick: %v", err)
	}
	if first == nil || first.ID != plus.ID {
		t.Fatalf("initial Pick = %#v, want %q", first, plus.ID)
	}

	plus.Metadata["excluded_models"] = []any{codexSparkModel}
	second, err := selector.Pick(context.Background(), "codex", codexSparkModel, opts, auths)
	if err != nil {
		t.Fatalf("Pick after exclusion: %v", err)
	}
	if second == nil || second.ID != pro.ID {
		t.Fatalf("Pick after exclusion = %#v, want %q", second, pro.ID)
	}
}

func TestSidecarRuntimeDoesNotSelectAccountWithExcludedModel(t *testing.T) {
	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config path: %v", err)
	}

	blockedFile := "blocked-luna.json"
	allowedFile := "allowed-luna.json"
	if err := os.WriteFile(filepath.Join(authDir, blockedFile), []byte(`{
  "type":"codex",
  "email":"blocked@example.com",
  "access_token":"blocked-token",
  "account_id":"acct-blocked",
  "excluded_models":["GPT-5.6-LUNA", "gpt-5.7-*"]
}`), 0o600); err != nil {
		t.Fatalf("write blocked auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, allowedFile), []byte(`{
  "type":"codex",
  "email":"allowed@example.com",
  "access_token":"allowed-token",
  "account_id":"acct-allowed"
}`), 0o600); err != nil {
		t.Fatalf("write allowed auth: %v", err)
	}

	m := &manifest{
		Accounts: []accountSpec{
			{ID: "blocked-account", Email: "blocked@example.com", AuthID: blockedFile, AuthKind: "oauth"},
			{ID: "allowed-account", Email: "allowed@example.com", AuthID: allowedFile, AuthKind: "oauth"},
		},
		ModelIDs:         []string{"gpt-5.6-luna", "gpt-5.7-preview", "gpt-5.4"},
		accountByID:      make(map[string]*accountSpec),
		accountByAuthID:  make(map[string]*accountSpec),
		accountByAPIKey:  make(map[string]*accountSpec),
		accountByChatGPT: make(map[string]*accountSpec),
		accountByEmail:   make(map[string]*accountSpec),
	}
	for index := range m.Accounts {
		account := &m.Accounts[index]
		m.accountByID[account.ID] = account
		m.accountByAuthID[strings.ToLower(account.AuthID)] = account
		m.accountByEmail[strings.ToLower(account.Email)] = account
	}

	cfg := &config.Config{AuthDir: authDir}
	manager := buildCoreAuthManager(cfg, &coderelaySelector{manifest: m}, &authHook{manifest: m}, m, nil, newRequestUsageTracker())
	runtime, err := newSidecarRuntime(context.Background(), configPath, cfg, m, manager)
	if err != nil {
		t.Fatalf("newSidecarRuntime: %v", err)
	}
	defer runtime.Stop()

	for attempt := 0; attempt < 8; attempt++ {
		selected, errSelect := manager.SelectAuth(context.Background(), "codex", "gpt-5.6-luna", cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("SelectAuth attempt %d: %v", attempt, errSelect)
		}
		if account := accountForAuthInManifest(m, selected); account == nil || account.ID != "allowed-account" {
			t.Fatalf("SelectAuth attempt %d selected blocked account: auth=%#v account=%#v", attempt, selected, account)
		}
	}

	blockedAuth, ok := manager.GetByID(blockedFile)
	if !ok || blockedAuth == nil {
		t.Fatalf("blocked auth %q was not registered", blockedFile)
	}
	blockedModels := registry.GetGlobalRegistry().GetModelsForClient(blockedAuth.ID)
	if info := findModelInfoForTest(blockedModels, "gpt-5.6-luna"); info != nil {
		t.Fatalf("blocked auth still advertises exact excluded model: %#v", info)
	}
	if info := findModelInfoForTest(blockedModels, "gpt-5.7-preview"); info != nil {
		t.Fatalf("blocked auth still advertises wildcard-excluded model: %#v", info)
	}
	if info := findModelInfoForTest(blockedModels, "gpt-5.4"); info == nil {
		t.Fatal("blocked auth lost a non-excluded model")
	}
}
