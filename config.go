package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	DefaultJA3       = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513-21,29-23-24,0"
	DefaultAkamai    = "1:65536;2:0;4:6291456;6:262144|15663105|0|m,a,s,p"
)

type AppConfig struct {
	Lang           string `json:"lang"`
	Opacity        int    `json:"opacity"`
	Blur           int    `json:"blur"`
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
		Blur:           0,
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
	var loaded AppConfig
	if err := json.Unmarshal(data, &loaded); err != nil {
		return cfg
	}
	if loaded.Lang == "zh-TW" || loaded.Lang == "zh-CN" || loaded.Lang == "en" {
		cfg.Lang = loaded.Lang
	}
	if loaded.Opacity >= 15 && loaded.Opacity <= 85 {
		cfg.Opacity = loaded.Opacity
	}
	cfg.Blur = 0
	cfg.RefererEnabled = loaded.RefererEnabled
	if loaded.SaveDir != "" {
		cfg.SaveDir = loaded.SaveDir
	}
	if loaded.UserAgent != "" {
		cfg.UserAgent = loaded.UserAgent
	}
	if loaded.JA3 != "" {
		cfg.JA3 = loaded.JA3
	}
	if loaded.Akamai != "" {
		cfg.Akamai = loaded.Akamai
	}
	return cfg
}

func SaveConfig(cfg AppConfig) {
	p := getConfigPath()
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	if cfg.Opacity < 15 {
		cfg.Opacity = 15
	}
	if cfg.Opacity > 85 {
		cfg.Opacity = 85
	}
	cfg.Blur = 0
	if cfg.Lang != "zh-TW" && cfg.Lang != "zh-CN" && cfg.Lang != "en" {
		cfg.Lang = "zh-TW"
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, data, 0644)
}
