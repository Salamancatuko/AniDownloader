package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type AppConfig struct {
	Lang           string `json:"lang"`
	Opacity        int    `json:"opacity"`
	Blur           int    `json:"blur"`
	RefererEnabled bool   `json:"refererEnabled"`
	SaveDir        string `json:"saveDir"`
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
		Blur:           22,
		RefererEnabled: true,
		SaveDir:        GetDefaultDownloadDir(),
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
	if loaded.Blur >= 0 && loaded.Blur <= 40 {
		cfg.Blur = loaded.Blur
	}
	cfg.RefererEnabled = loaded.RefererEnabled
	if loaded.SaveDir != "" {
		cfg.SaveDir = loaded.SaveDir
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
	if cfg.Blur < 0 {
		cfg.Blur = 0
	}
	if cfg.Blur > 40 {
		cfg.Blur = 40
	}
	if cfg.Lang != "zh-TW" && cfg.Lang != "zh-CN" && cfg.Lang != "en" {
		cfg.Lang = "zh-TW"
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, data, 0644)
}
