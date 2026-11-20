//go:build windows

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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
// Priority: executable directory -> executable/bin -> managed %LOCALAPPDATA%\AniDownloader\bin (with SHA-256 update check).
func EnsureBinaries(logFn func(tag, color, msg string)) (ToolInfo, error) {
	var info ToolInfo
	if logFn == nil {
		logFn = func(string, string, string) {}
	}

	var overrideDirs []string
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		if exeDir != "" {
			overrideDirs = append(overrideDirs, exeDir, filepath.Join(exeDir, "bin"))
		}
	}

	info.YtDlpPath, info.FFmpegPath = resolveOverrideTools(overrideDirs)

	if info.YtDlpPath == "" || info.FFmpegPath == "" {
		var managedDirs []string
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			managedDirs = append(managedDirs, filepath.Join(localAppData, "AniDownloader", "bin"))
		}
		if appData := os.Getenv("APPDATA"); appData != "" {
			managedDirs = append(managedDirs, filepath.Join(appData, "AniDownloader", "bin"))
		}
		if tmpDir := os.TempDir(); tmpDir != "" {
			managedDirs = append(managedDirs, filepath.Join(tmpDir, "AniDownloader", "bin"))
		}

		var targetDir string
		var mkdirErr error
		for _, dir := range managedDirs {
			if err := os.MkdirAll(dir, 0755); err == nil {
				targetDir = dir
				break
			} else {
				mkdirErr = err
			}
		}
		if targetDir == "" {
			if mkdirErr != nil {
				return info, fmt.Errorf("建立工具目錄失敗: %w", mkdirErr)
			}
			return info, fmt.Errorf("找不到可用的工具解壓目錄")
		}

		if info.YtDlpPath == "" {
			info.YtDlpPath = filepath.Join(targetDir, "yt-dlp.exe")
			if err := ensureExtractedBinary(ytDlpGz, info.YtDlpPath, "yt-dlp.exe", logFn); err != nil {
				return info, fmt.Errorf("解壓 yt-dlp.exe 失敗: %w", err)
			}
		}
		if info.FFmpegPath == "" {
			info.FFmpegPath = filepath.Join(targetDir, "ffmpeg.exe")
			if err := ensureExtractedBinary(ffmpegGz, info.FFmpegPath, "ffmpeg.exe", logFn); err != nil {
				return info, fmt.Errorf("解壓 ffmpeg.exe 失敗: %w", err)
			}
		}
	}

	ffVer, err := detectFFmpegVersion(info.FFmpegPath)
	if err != nil {
		return info, err
	}
	ytVer, err := detectYtDlpVersion(info.YtDlpPath)
	if err != nil {
		return info, err
	}
	info.FFmpegVersion = ffVer
	info.YtDlpVersion = ytVer

	logFn("[提示]", "#38bdf8", fmt.Sprintf("內建模組就緒：FFMPEG (%s) / YT-DLP (%s)", info.FFmpegVersion, info.YtDlpVersion))
	return info, nil
}

func resolveOverrideTools(overrideDirs []string) (ytPath, ffPath string) {
	for _, dir := range overrideDirs {
		if dir == "" {
			continue
		}
		if ytPath == "" {
			yt := filepath.Join(dir, "yt-dlp.exe")
			if fileExistsNonEmpty(yt) {
				ytPath = yt
			}
		}
		if ffPath == "" {
			ff := filepath.Join(dir, "ffmpeg.exe")
			if fileExistsNonEmpty(ff) {
				ffPath = ff
			}
		}
		if ytPath != "" && ffPath != "" {
			break
		}
	}
	return ytPath, ffPath
}

func renameWithRetry(src, dst string) error {
	const maxAttempts = 5
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		if attempt < maxAttempts-1 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return err
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ensureExtractedBinary(gzData []byte, destPath, name string, logFn func(tag, color, msg string)) error {
	if logFn == nil {
		logFn = func(string, string, string) {}
	}
	expectedHash := sha256Hex(gzData)
	hashPath := destPath + ".sha256"

	savedHashBytes, readErr := os.ReadFile(hashPath)
	savedHash := strings.TrimSpace(string(savedHashBytes))
	if fileExistsNonEmpty(destPath) && readErr == nil && savedHash == expectedHash {
		return nil
	}

	if fileExistsNonEmpty(destPath) {
		logFn("[環境]", "#38bdf8", fmt.Sprintf("偵測到版本更新：正在更新內嵌 %s...", name))
	} else {
		logFn("[環境]", "#38bdf8", fmt.Sprintf("首次啟動：正在解壓內嵌 %s...", name))
	}

	if err := decompressGz(gzData, destPath); err != nil {
		return err
	}

	if err := writeFileAtomic(hashPath, []byte(expectedHash+"\n"), 0644); err != nil {
		return err
	}
	return nil
}

func writeFileAtomic(destPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(destPath)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(destPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmpFile.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmpFile.Chmod(perm); err != nil {
		// Chmod may be partial on Windows; ignore unless Close/Rename fails
		_ = err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := renameWithRetry(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

var ffmpegVerRe = regexp.MustCompile(`ffmpeg version\s+([^\s-]+)`)

func detectFFmpegVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("無法執行 ffmpeg (%s): %w", path, err)
	}
	if m := ffmpegVerRe.FindStringSubmatch(string(out)); len(m) > 1 {
		v := strings.TrimPrefix(m[1], "v")
		if len(v) > 12 {
			v = v[:12]
		}
		if v != "" {
			return "v" + v, nil
		}
	}
	return "", fmt.Errorf("無法解析 ffmpeg 版本資訊 (%s)", path)
}

func detectYtDlpVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("無法執行 yt-dlp (%s): %w", path, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		v := strings.TrimSpace(line)
		if v != "" {
			if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("yt-dlp 未回傳有效版本資訊 (%s)", path)
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

	dir := filepath.Dir(destPath)
	out, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpPath := out.Name()

	if _, err := io.Copy(out, gr); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := gr.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := renameWithRetry(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
