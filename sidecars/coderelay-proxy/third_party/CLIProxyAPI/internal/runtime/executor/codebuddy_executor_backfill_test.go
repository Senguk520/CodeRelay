package executor

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
)

// buildReadToolImageBody constructs a request body that models the captured
// CodeBuddy read-tool image workflow: an assistant read_file tool_call carrying
// a filePath, followed by a role=tool result whose content is the "image already
// analyzed" placeholder (base64 omitted), and finally a text-only user turn.
//
// The body is built with encoding/json so Windows paths (backslashes) are escaped
// correctly without hand-written JSON string literals.
func buildReadToolImageBody(path string) string {
	args, _ := json.Marshal(map[string]string{"filePath": path})
	placeholder := "[Image already analyzed in an earlier step; base64 content omitted to save memory. Path: " + path + ". Re-invoke read_file to view it again if needed.]"
	body := map[string]any{
		"model": "deepseek-v4-pro",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "读一下这张图"}}},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "read_file",
							"arguments": string(args),
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": placeholder},
			map[string]any{"role": "assistant", "content": "看完了"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "图里有什么？"}}},
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// writeTestPNG writes a minimal valid PNG header + payload so the backfill has a
// real (small) image file to base64-encode.
func writeTestPNG(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	// Minimal 1x1 transparent PNG.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(p, png, 0o644); err != nil {
		t.Fatalf("writeTestPNG: %v", err)
	}
	return p
}

func TestCodebuddyBackfillReadToolImages_ShortCircuits(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty body", ``},
		{"invalid json", `not-json`},
		{"no read tool", `{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`},
		{"read tool but no messages", `{"model":"x","tools":[{"function":{"name":"read"}}]}`},
		{"no user message", `{"model":"x","messages":[{"role":"assistant","content":"ok"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(tt.in)
			out := codebuddyBackfillReadToolImages(in)
			if string(out) != string(in) {
				t.Fatalf("expected unchanged, got %s", out)
			}
		})
	}
}

func TestCodebuddyBackfillReadToolImages_AlreadyHasImage(t *testing.T) {
	// Even though a read tool + placeholder is present, the current turn already
	// carries an image_url, so the backfill must not double-append.
	in := `{"model":"x","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"读图"}]},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"filePath\":\"a.png\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"[Image already analyzed]"},` +
		`{"role":"user","content":[{"type":"text","text":"看"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}` +
		`]}`
	out := codebuddyBackfillReadToolImages([]byte(in))
	if string(out) != in {
		t.Fatalf("expected unchanged (already has image), got %s", out)
	}
}

func TestCodebuddyBackfillReadToolImages_AttachesFromFilePath(t *testing.T) {
	dir := t.TempDir()
	img := writeTestPNG(t, dir, "photo.png")
	body := buildReadToolImageBody(img)

	out := codebuddyBackfillReadToolImages([]byte(body))

	// The vision router must now see an image input.
	if !codebuddyChatHasImageInput(out) {
		t.Fatalf("backfill should make codebuddyChatHasImageInput true; out=%s", out)
	}

	// The image_url part must be appended to the LAST user message (index 4).
	lastUserContent := gjson.GetBytes(out, "messages.4.content")
	if !lastUserContent.IsArray() {
		t.Fatalf("last user content should be array, got %s", lastUserContent.Raw)
	}
	parts := lastUserContent.Array()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + image_url), got %d: %s", len(parts), lastUserContent.Raw)
	}
	imgPart := parts[1]
	if imgPart.Get("type").String() != "image_url" {
		t.Fatalf("appended part type = %q, want image_url", imgPart.Get("type").String())
	}
	url := imgPart.Get("image_url.url").String()
	if url == "" {
		t.Fatalf("image_url.url empty")
	}
	// Verify the URL is a real data URL that round-trips to the original bytes.
	raw := writeTestPNG(t, dir, "ref.png")
	rawBytes, _ := os.ReadFile(raw)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(rawBytes)
	if url != want {
		t.Fatalf("data URL mismatch: got %q, want %q", url, want)
	}

	// The original text part must be untouched.
	if parts[0].Get("text").String() != "图里有什么？" {
		t.Fatalf("text part corrupted: %s", parts[0].Raw)
	}
}

func TestCodebuddyBackfillReadToolImages_MissingFileDegrades(t *testing.T) {
	body := buildReadToolImageBody(filepath.Join(t.TempDir(), "nope.png"))
	out := codebuddyBackfillReadToolImages([]byte(body))
	if string(out) != body {
		t.Fatalf("expected unchanged when file missing, got %s", out)
	}
}

func TestCodebuddyBackfillReadToolImages_UnsupportedExtensionDegrades(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(bad, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := buildReadToolImageBody(bad)
	out := codebuddyBackfillReadToolImages([]byte(body))
	if string(out) != body {
		t.Fatalf("expected unchanged for non-image extension, got %s", out)
	}
}

func TestCodebuddyBackfillReadToolImages_NonPlaceholderToolResultIgnored(t *testing.T) {
	// A read tool whose result is real text (not a placeholder) must NOT trigger
	// a backfill even if a filePath is present.
	dir := t.TempDir()
	img := writeTestPNG(t, dir, "photo.png")
	args, _ := json.Marshal(map[string]string{"filePath": img})
	body := map[string]any{
		"model": "x",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "读一下"}}},
			map[string]any{
				"role":    "assistant",
				"tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": string(args)}},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "文件内容是一段普通文本"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "继续"}}},
		},
	}
	in, _ := json.Marshal(body)
	out := codebuddyBackfillReadToolImages(in)
	if string(out) != string(in) {
		t.Fatalf("expected unchanged for non-placeholder tool result, got %s", out)
	}
}

func TestExtractCodebuddyToolFilePath(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{"filePath key", `{"filePath":"h:\\a\\b.png"}`, `h:\a\b.png`},
		{"path key", `{"path":"c:/x/y.jpg"}`, `c:/x/y.jpg`},
		{"empty", ``, ``},
		{"no path", `{"offset":3}`, ``},
		{"malformed json fallback", `{"filePath":"z.png"`, `z.png`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractCodebuddyToolFilePath(tt.args); got != tt.want {
				t.Fatalf("extractCodebuddyToolFilePath(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestIsCodebuddyImagePlaceholder(t *testing.T) {
	for _, in := range []string{
		"[Image already analyzed in an earlier step; base64 content omitted to save memory.]",
		"base64 content omitted to save memory",
		"IMAGE ALREADY ANALYZED",
	} {
		if !isCodebuddyImagePlaceholder(in) {
			t.Fatalf("isCodebuddyImagePlaceholder(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "普通文本结果", "file content here"} {
		if isCodebuddyImagePlaceholder(in) {
			t.Fatalf("isCodebuddyImagePlaceholder(%q) = true, want false", in)
		}
	}
}
