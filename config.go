//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	DefaultJA3       = "" // 空字串代表使用 azuretls.Chrome 內建最新原生 TLS 1.3 指紋
	legacyDefaultJA3 = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513-21,29-23-24,0"
	DefaultAkamai    = "1:65536;2:0;4:6291456;6:262144|15663105|0|m,a,s,p"
)

var configSaveMu sync.Mutex

type AppConfig struct {
	Lang           string `json:"lang"`
	Opacity        int    `json:"opacity"`
	RefererEnabled bool   `json:"refererEnabled"`
	SaveDir        string `json:"saveDir"`
	UserAgent      string `json:"userAgent"`
	JA3            string `json:"ja3"`
	Akamai         string `json:"akamai"`
}

func getConfigPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "AniDownloader", "config.json")
}

func DefaultConfig() AppConfig {
	return AppConfig{
		Lang:           "zh-TW",
		Opacity:        48,
		RefererEnabled: true,
		SaveDir:        GetDefaultDownloadDir(),
		UserAgent:      DefaultUserAgent,
		JA3:            DefaultJA3,
		Akamai:         DefaultAkamai,
	}
}

func LoadConfig() AppConfig {
	cfg := DefaultConfig()
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig()
	}
	if cfg.Lang != "zh-TW" && cfg.Lang != "zh-CN" && cfg.Lang != "en" {
		cfg.Lang = "zh-TW"
	}
	if cfg.Opacity < 15 {
		cfg.Opacity = 15
	} else if cfg.Opacity > 85 {
		cfg.Opacity = 85
	}
	if strings.TrimSpace(cfg.SaveDir) == "" {
		cfg.SaveDir = GetDefaultDownloadDir()
	}
	if strings.TrimSpace(cfg.JA3) == legacyDefaultJA3 {
		cfg.JA3 = ""
		SaveConfig(cfg)
	}
	return cfg
}

func SaveConfig(cfg AppConfig) {
	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	p := getConfigPath()
	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, 0755)
	if cfg.Opacity < 15 {
		cfg.Opacity = 15
	}
	if cfg.Opacity > 85 {
		cfg.Opacity = 85
	}
	if cfg.Lang != "zh-TW" && cfg.Lang != "zh-CN" && cfg.Lang != "en" {
		cfg.Lang = "zh-TW"
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}

	tmpFile, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := renameWithRetry(tmpPath, p); err != nil {
		_ = os.Remove(tmpPath)
	}
}
