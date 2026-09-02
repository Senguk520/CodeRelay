package main

import (
	"path/filepath"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCodebuddyModelCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	cachePath := codebuddyModelCachePath(manifestPath)
	if cachePath != filepath.Join(dir, codebuddyModelCacheFilename) {
		t.Fatalf("cache path = %q, want %q", cachePath, filepath.Join(dir, codebuddyModelCacheFilename))
	}

	models := []*internalregistry.ModelInfo{
		{ID: "kimi-k3", Type: "codebuddy", OwnedBy: "tencent", SupportsImages: true, ContextLength: 200000, MaxCompletionTokens: 32768},
		{ID: "glm-5.3", Type: "codebuddy", OwnedBy: "tencent", SupportsImages: false},
	}
	if err := saveCodebuddyModelCache(cachePath, models); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadCodebuddyModelCache(cachePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d models, want 2", len(loaded))
	}
	if loaded[0].ID != "kimi-k3" || !loaded[0].SupportsImages || loaded[0].ContextLength != 200000 || loaded[0].MaxCompletionTokens != 32768 {
		t.Fatalf("model capabilities not preserved: %+v", loaded[0])
	}
	if loaded[1].ID != "glm-5.3" || loaded[1].SupportsImages {
		t.Fatalf("model capabilities not preserved: %+v", loaded[1])
	}
}

func TestCodebuddyModelCacheMissingFile(t *testing.T) {
	if _, err := loadCodebuddyModelCache(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing cache file")
	}
}

func TestCodebuddyModelCacheEmptyManifestPath(t *testing.T) {
	if got := codebuddyModelCachePath(""); got != "" {
		t.Fatalf("cache path = %q, want empty", got)
	}
}
