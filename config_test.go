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

func TestDefaultConfigJA3Empty(t *testing.T) {
	if DefaultJA3 != "" {
		t.Fatalf("DefaultJA3 = %q, want empty string", DefaultJA3)
	}
	if cfg := DefaultConfig(); cfg.JA3 != "" {
		t.Fatalf("DefaultConfig().JA3 = %q, want empty string", cfg.JA3)
	}
}

func TestLoadConfigMigratesLegacyJA3AndPreservesCustomJA3(t *testing.T) {
	t.Run("MigratesLegacyDefaultJA3", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("LOCALAPPDATA", tmp)

		cfgPath := getConfigPath()
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
			t.Fatal(err)
		}
		legacyJSON, err := json.Marshal(AppConfig{
			Lang:           "zh-TW",
			Opacity:        48,
			RefererEnabled: true,
			SaveDir:        tmp,
			UserAgent:      DefaultUserAgent,
			JA3:            legacyDefaultJA3,
			Akamai:         DefaultAkamai,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, legacyJSON, 0644); err != nil {
			t.Fatal(err)
		}

		loaded := LoadConfig()
		if loaded.JA3 != "" {
			t.Fatalf("loaded.JA3 = %q, want empty after legacy migration", loaded.JA3)
		}

		persistedBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var persisted AppConfig
		if err := json.Unmarshal(persistedBytes, &persisted); err != nil {
			t.Fatal(err)
		}
		if persisted.JA3 != "" {
			t.Fatalf("persisted.JA3 = %q, want empty on disk after migration", persisted.JA3)
		}
	})

	t.Run("PreservesCustomJA3", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("LOCALAPPDATA", tmp)

		cfgPath := getConfigPath()
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
			t.Fatal(err)
		}
		const customJA3 = "771,4865-4866,0-23,29,0"
		customJSON, err := json.Marshal(AppConfig{
			Lang:           "zh-TW",
			Opacity:        48,
			RefererEnabled: true,
			SaveDir:        tmp,
			UserAgent:      DefaultUserAgent,
			JA3:            customJA3,
			Akamai:         DefaultAkamai,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, customJSON, 0644); err != nil {
			t.Fatal(err)
		}

		loaded := LoadConfig()
		if loaded.JA3 != customJA3 {
			t.Fatalf("loaded.JA3 = %q, want preserved %q", loaded.JA3, customJA3)
		}
	})
}

func TestConfigBiliSessDataPersistence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	if def := DefaultConfig(); def.BiliSessData != "" {
		t.Fatalf("DefaultConfig().BiliSessData = %q, want empty", def.BiliSessData)
	}

	cfgPath := getConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	// Legacy config.json without biliSessData defaults to empty string
	if err := os.WriteFile(cfgPath, []byte(`{"lang":"zh-TW","opacity":48}`), 0644); err != nil {
		t.Fatal(err)
	}
	if loaded := LoadConfig(); loaded.BiliSessData != "" {
		t.Fatalf("missing biliSessData should load as empty, got %q", loaded.BiliSessData)
	}

	// Non-empty BiliSessData persists across SaveConfig / LoadConfig
	cfg := DefaultConfig()
	cfg.BiliSessData = "abc%2C123%2Csecret"
	SaveConfig(cfg)
	if loaded := LoadConfig(); loaded.BiliSessData != "abc%2C123%2Csecret" {
		t.Fatalf("persisted BiliSessData = %q, want abc%%2C123%%2Csecret", loaded.BiliSessData)
	}

	// Clearing BiliSessData persists as empty string
	cfg.BiliSessData = ""
	SaveConfig(cfg)
	if loaded := LoadConfig(); loaded.BiliSessData != "" {
		t.Fatalf("cleared BiliSessData = %q, want empty", loaded.BiliSessData)
	}
}
