package main

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Embedded compressed Windows x64 binaries
//
//go:embed bin_embed/yt-dlp.exe.gz
var ytDlpGz []byte

//go:embed bin_embed/ffmpeg.exe.gz
var ffmpegGz []byte

type ToolInfo struct {
	YtDlpPath     string
	FFmpegPath    string
	YtDlpVersion  string
	FFmpegVersion string
}

// EnsureBinaries locates or unpacks ffmpeg.exe and yt-dlp.exe.
// Priority: executable directory -> executable/bin -> %LOCALAPPDATA%\AniDownloader\bin.
func EnsureBinaries(logFn func(tag, color, msg string)) (ToolInfo, error) {
	var info ToolInfo

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)

	candidateDirs := []string{
		exeDir,
		filepath.Join(exeDir, "bin"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "AniDownloader", "bin"),
		filepath.Join(os.Getenv("APPDATA"), "AniDownloader", "bin"),
		filepath.Join(os.TempDir(), "AniDownloader", "bin"),
	}

	for _, dir := range candidateDirs {
		if dir == "" {
			continue
		}
		yt := filepath.Join(dir, "yt-dlp.exe")
		ff := filepath.Join(dir, "ffmpeg.exe")
		if fileExistsNonEmpty(yt) && fileExistsNonEmpty(ff) {
			info.YtDlpPath = yt
			info.FFmpegPath = ff
			break
		}
	}

	if info.YtDlpPath == "" || info.FFmpegPath == "" {
		targetDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "AniDownloader", "bin")
		if os.Getenv("LOCALAPPDATA") == "" {
			targetDir = filepath.Join(os.TempDir(), "AniDownloader", "bin")
		}
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			targetDir = filepath.Join(os.TempDir(), "AniDownloader", "bin")
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return info, fmt.Errorf("建立工具目錄失敗: %w", err)
			}
		}

		info.YtDlpPath = filepath.Join(targetDir, "yt-dlp.exe")
		info.FFmpegPath = filepath.Join(targetDir, "ffmpeg.exe")

		if !fileExistsNonEmpty(info.YtDlpPath) {
			logFn("[環境]", "#38bdf8", "首次啟動：正在解壓內嵌 yt-dlp.exe...")
			if err := decompressGz(ytDlpGz, info.YtDlpPath); err != nil {
				return info, fmt.Errorf("解壓 yt-dlp.exe 失敗: %w", err)
			}
		}
		if !fileExistsNonEmpty(info.FFmpegPath) {
			logFn("[環境]", "#38bdf8", "首次啟動：正在解壓內嵌 ffmpeg.exe...")
			if err := decompressGz(ffmpegGz, info.FFmpegPath); err != nil {
				return info, fmt.Errorf("解壓 ffmpeg.exe 失敗: %w", err)
			}
		}
	}

	info.FFmpegVersion = detectFFmpegVersion(info.FFmpegPath)
	info.YtDlpVersion = detectYtDlpVersion(info.YtDlpPath)

	logFn("[提示]", "#38bdf8", fmt.Sprintf("內建模組就緒：FFMPEG (%s) / YT-DLP (%s)", info.FFmpegVersion, info.YtDlpVersion))
	return info, nil
}

var ffmpegVerRe = regexp.MustCompile(`ffmpeg version\s+([^\s-]+)`)

func detectFFmpegVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "v6.1.1"
	}
	if m := ffmpegVerRe.FindStringSubmatch(string(out)); len(m) > 1 {
		v := strings.TrimPrefix(m[1], "v")
		if len(v) > 12 {
			v = v[:12]
		}
		return "v" + v
	}
	return "v6.1.1"
}

func detectYtDlpVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "v2026.10"
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "v2026.10"
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func fileExistsNonEmpty(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Size() > 0
}

func decompressGz(data []byte, destPath string) error {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()

	tmpPath := destPath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, gr); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	out.Close()

	_ = os.Remove(destPath)
	return os.Rename(tmpPath, destPath)
}
