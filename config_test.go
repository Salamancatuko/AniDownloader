//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadConfigMissingFieldsKeepsDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	cfgPath := getConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	// Missing refererEnabled, userAgent, ja3, akamai
	if err := os.WriteFile(cfgPath, []byte(`{"lang":"en","opacity":70}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.Lang != "en" {
		t.Fatalf("Lang = %q, want en", cfg.Lang)
	}
	if cfg.Opacity != 70 {
		t.Fatalf("Opacity = %d, want 70", cfg.Opacity)
	}
	if !cfg.RefererEnabled {
		t.Fatal("RefererEnabled should default to true when omitted from config.json")
	}
	if cfg.UserAgent != DefaultUserAgent || cfg.JA3 != DefaultJA3 || cfg.Akamai != DefaultAkamai {
		t.Fatalf("omitted fingerprint fields should retain defaults, got %+v", cfg)
	}
}

func TestLoadConfigPreservesExplicitFalseAndEmptyStrings(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	cfgPath := getConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	raw := `{
  "lang": "zh-CN",
  "opacity": 32,
  "refererEnabled": false,
  "saveDir": "D:\\Videos",
  "userAgent": "",
  "ja3": "",
  "akamai": ""
}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.RefererEnabled {
		t.Fatal("explicit refererEnabled=false should be preserved")
	}
	if cfg.UserAgent != "" || cfg.JA3 != "" || cfg.Akamai != "" {
		t.Fatalf("explicit empty UA/JA3/Akamai strings must be preserved, got %+v", cfg)
	}
}

func TestLoadConfigClampsOpacityAndHandlesMalformedJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	cfgPath := getConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cfgPath, []byte(`{"lang":"jp","opacity":5,"saveDir":""}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig()
	if cfg.Lang != "zh-TW" {
		t.Fatalf("invalid lang should fallback to zh-TW, got %q", cfg.Lang)
	}
	if cfg.Opacity != 15 {
		t.Fatalf("low opacity should clamp to 15, got %d", cfg.Opacity)
	}
	if strings.TrimSpace(cfg.SaveDir) == "" {
		t.Fatal("empty saveDir should fallback to default download dir")
	}

	if err := os.WriteFile(cfgPath, []byte(`{"opacity":999}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg = LoadConfig()
	if cfg.Opacity != 85 {
		t.Fatalf("high opacity should clamp to 85, got %d", cfg.Opacity)
	}

	if err := os.WriteFile(cfgPath, []byte(`{not-valid-json`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg = LoadConfig()
	if cfg.Opacity != 48 || !cfg.RefererEnabled {
		t.Fatalf("malformed JSON should fallback cleanly to DefaultConfig, got %+v", cfg)
	}
}

func TestSaveConfigConcurrentAtomic(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				cfg := DefaultConfig()
				cfg.Opacity = 15 + ((id + i) % 71)
				cfg.RefererEnabled = (id+i)%2 == 0
				if i%2 == 0 {
					cfg.JA3 = ""
					cfg.Akamai = ""
				}
				SaveConfig(cfg)
			}
		}(g)
	}
	wg.Wait()

	cfgPath := getConfigPath()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	var parsed AppConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("saved config.json corrupted after concurrent writes: %v\n%s", err, string(data))
	}

	entries, err := os.ReadDir(filepath.Dir(cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			t.Fatalf("leftover temp file found: %s", ent.Name())
		}
	}
}
