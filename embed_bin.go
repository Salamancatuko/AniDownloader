//go:build windows

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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
//go:embed bin_embed/ffmpeg.exe.gz
var ffmpegGz []byte

const ytDlpReleaseDownloadURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"

var (
	ytDlpDownloadURL           = ytDlpReleaseDownloadURL
	ytDlpHTTPClient            = &http.Client{Timeout: 5 * time.Minute}
	minYtDlpBinarySize   int64 = 1 << 20 // 1 MiB minimum PE size guard
	findOptionalYtDlpFn        = FindOptionalYtDlp
	detectYtDlpVersionFn       = detectYtDlpVersion
	runYtDlpSelfUpdateFn       = runYtDlpSelfUpdate
	osExecutableFn             = os.Executable
	execLookPathFn             = exec.LookPath
)

type ToolInfo struct {
	FFmpegPath    string
	FFmpegVersion string
	YtDlpPath     string
	YtDlpVersion  string
	CoreVersion   string
}

// EnsureBinaries locates or unpacks ffmpeg.exe and populates native engine metadata.
// Priority: executable directory -> executable/bin -> managed %LOCALAPPDATA%\AniDownloader\bin (with SHA-256 update check).
func EnsureBinaries(logFn func(tag, color, msg string)) (ToolInfo, error) {
	info := ToolInfo{
		CoreVersion: NativeHLSEngineVersion,
	}
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

	info.FFmpegPath = resolveOverrideTools(overrideDirs)
	fromManagedDir := false

	if info.FFmpegPath == "" {
		fromManagedDir = true
		targetDir, err := getManagedBinDir()
		if err != nil {
			return info, err
		}

		info.FFmpegPath = filepath.Join(targetDir, "ffmpeg.exe")
		if err := ensureExtractedBinary(ffmpegGz, info.FFmpegPath, "ffmpeg.exe", logFn); err != nil {
			return info, fmt.Errorf("解壓 ffmpeg.exe 失敗: %w", err)
		}
	}

	ffVer, err := detectFFmpegVersion(info.FFmpegPath)
	if err != nil && fromManagedDir {
		logFn("[警告]", "#fbbf24", fmt.Sprintf("內建 ffmpeg.exe 驗證失敗，正在嘗試重新解壓修復: %v", err))
		_ = os.Remove(info.FFmpegPath)
		_ = os.Remove(info.FFmpegPath + ".sha256")
		if reErr := ensureExtractedBinary(ffmpegGz, info.FFmpegPath, "ffmpeg.exe", logFn); reErr != nil {
			return info, fmt.Errorf("重新解壓 ffmpeg.exe 失敗: %w", reErr)
		}
		ffVer, err = detectFFmpegVersion(info.FFmpegPath)
	}
	if err != nil {
		return info, err
	}
	info.FFmpegVersion = ffVer

	info.YtDlpPath, info.YtDlpVersion = findOptionalYtDlpFn()
	ytStatus := "未安裝 (可於設定一鍵安裝)"
	if info.YtDlpPath != "" {
		if info.YtDlpVersion != "" {
			ytStatus = info.YtDlpVersion
		} else {
			ytStatus = "已安裝"
		}
	}

	logFn("[提示]", "#38bdf8", fmt.Sprintf("內建模組就緒：HLS-CORE (%s) / FFMPEG (%s) / YT-DLP (%s)", info.CoreVersion, info.FFmpegVersion, ytStatus))
	return info, nil
}

func managedBinDirCandidates() []string {
	var dirs []string
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		dirs = append(dirs, filepath.Join(localAppData, "AniDownloader", "bin"))
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		dirs = append(dirs, filepath.Join(appData, "AniDownloader", "bin"))
	}
	if tmpDir := os.TempDir(); tmpDir != "" {
		dirs = append(dirs, filepath.Join(tmpDir, "AniDownloader", "bin"))
	}
	return dirs
}

func getManagedBinDir() (string, error) {
	var mkdirErr error
	for _, dir := range managedBinDirCandidates() {
		if err := os.MkdirAll(dir, 0755); err == nil {
			return dir, nil
		} else {
			mkdirErr = err
		}
	}
	if mkdirErr != nil {
		return "", fmt.Errorf("建立工具目錄失敗: %w", mkdirErr)
	}
	return "", fmt.Errorf("找不到可用的工具解壓目錄")
}

// FindOptionalYtDlp searches override folders, managed AniDownloader bin folders, and PATH
// for an optional yt-dlp.exe without failing if absent.
func FindOptionalYtDlp() (string, string) {
	var candidates []string
	if exePath, err := osExecutableFn(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		if exeDir != "" {
			candidates = append(candidates,
				filepath.Join(exeDir, "yt-dlp.exe"),
				filepath.Join(exeDir, "bin", "yt-dlp.exe"),
			)
		}
	}
	for _, dir := range managedBinDirCandidates() {
		candidates = append(candidates, filepath.Join(dir, "yt-dlp.exe"))
	}

	seen := make(map[string]struct{}, len(candidates)+1)
	checkCandidate := func(path string) (string, string, bool) {
		if path == "" {
			return "", "", false
		}
		clean := filepath.Clean(path)
		if _, dup := seen[clean]; dup {
			return "", "", false
		}
		seen[clean] = struct{}{}
		if !fileExistsNonEmpty(clean) {
			return "", "", false
		}
		ver, err := detectYtDlpVersionFn(clean)
		v := strings.TrimSpace(ver)
		if err != nil || v == "" {
			return clean, "已安裝", true
		}
		if v != "已安裝" {
			if strings.HasPrefix(v, "V") {
				v = "v" + v[1:]
			} else if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}
		}
		return clean, v, true
	}

	for _, cand := range candidates {
		if p, v, ok := checkCandidate(cand); ok {
			return p, v
		}
	}
	if pathHit, err := execLookPathFn("yt-dlp.exe"); err == nil && pathHit != "" {
		if p, v, ok := checkCandidate(pathHit); ok {
			return p, v
		}
	}
	return "", ""
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
	line := strings.TrimSpace(string(out))
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "v"), "V"))
	if line == "" {
		return "", fmt.Errorf("無法解析 yt-dlp 版本資訊 (%s)", path)
	}
	return "v" + line, nil
}

// InstallOrUpdateYtDlp updates an existing yt-dlp.exe via `-U` or downloads the official
// Windows release into the managed AniDownloader bin directory.
func InstallOrUpdateYtDlp(ctx context.Context, logFn func(tag, color, msg string)) (ToolInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if logFn == nil {
		logFn = func(string, string, string) {}
	}

	info := ToolInfo{
		CoreVersion: NativeHLSEngineVersion,
	}

	existingPath, _ := findOptionalYtDlpFn()
	if existingPath != "" {
		logFn("[系統]", "#38bdf8", fmt.Sprintf("偵測到本機 yt-dlp.exe (%s)，正在執行 yt-dlp -U 檢查更新...", existingPath))
		if err := runYtDlpSelfUpdateFn(ctx, existingPath, logFn); err != nil {
			return info, err
		}
	} else {
		dir, err := getManagedBinDir()
		if err != nil {
			return info, err
		}
		destPath := filepath.Join(dir, "yt-dlp.exe")
		logFn("[系統]", "#38bdf8", "正在從 GitHub 官方發布頁下載 yt-dlp.exe (Windows x64)...")
		if err := downloadYtDlpBinary(ctx, ytDlpDownloadURL, destPath); err != nil {
			return info, err
		}
	}

	ytPath, ytVer := findOptionalYtDlpFn()
	if ytPath == "" {
		return info, fmt.Errorf("安裝或更新完成後仍無法定位 yt-dlp.exe")
	}
	info.YtDlpPath = ytPath
	info.YtDlpVersion = ytVer
	logFn("[完成]", "#34d399", fmt.Sprintf("外部 YT-DLP 就緒：版本 %s (%s)", ytVer, ytPath))
	return info, nil
}

func runYtDlpSelfUpdate(ctx context.Context, ytDlpPath string, logFn func(tag, color, msg string)) error {
	updateCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(updateCtx, ytDlpPath, "-U")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	if text := strings.TrimSpace(string(out)); text != "" {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				logFn("[yt-dlp]", "#38bdf8", trimmed)
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("執行 yt-dlp -U 更新失敗: %w", err)
	}
	return nil
}

func downloadYtDlpBinary(ctx context.Context, downloadURL, destPath string) error {
	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("建立 yt-dlp 下載請求失敗: %w", err)
	}
	req.Header.Set("User-Agent", DefaultUserAgent)

	resp, err := ytDlpHTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("下載 yt-dlp.exe 失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下載 yt-dlp.exe 失敗：伺服器回應 HTTP %d", resp.StatusCode)
	}

	dir := filepath.Dir(destPath)
	tmpFile, err := os.CreateTemp(dir, "yt-dlp.exe.*.tmp")
	if err != nil {
		return fmt.Errorf("建立 yt-dlp 暫存檔失敗: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	const maxBinarySize = 128 << 20
	written, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxBinarySize+1))
	if err != nil {
		cleanup()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("寫入 yt-dlp 暫存檔失敗: %w", err)
	}
	if written > maxBinarySize {
		cleanup()
		return errors.New("下載的 yt-dlp.exe 超過檔案大小上限 (128 MB)")
	}
	if written < minYtDlpBinarySize {
		cleanup()
		return fmt.Errorf("下載的 yt-dlp.exe 檔案過小 (%d bytes)，可能並非有效執行檔", written)
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return err
	}
	var magic [2]byte
	if _, err := io.ReadFull(tmpFile, magic[:]); err != nil {
		cleanup()
		return err
	}
	if magic[0] != 'M' || magic[1] != 'Z' {
		cleanup()
		return errors.New("下載的 yt-dlp.exe 缺少有效的 Windows PE (MZ) 檔頭")
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceFileSafe(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("置換 yt-dlp.exe 至目標路徑失敗: %w", err)
	}
	return nil
}

func resolveOverrideTools(overrideDirs []string) string {
	for _, dir := range overrideDirs {
		if dir == "" {
			continue
		}
		ff := filepath.Join(dir, "ffmpeg.exe")
		if fileExistsNonEmpty(ff) {
			return ff
		}
	}
	return ""
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
