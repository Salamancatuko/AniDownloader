//go:build windows

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeGzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecompressGzAtomicAndCorruptCleanup(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "tool.exe")
	gz := makeGzipBytes(t, []byte("hello-binary-v1"))

	if err := decompressGz(gz, dest); err != nil {
		t.Fatalf("decompressGz valid failed: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "hello-binary-v1" {
		t.Fatalf("unexpected extracted content: %q, err=%v", string(got), err)
	}

	// Corrupt gzip must fail and leave no .tmp files
	if err := decompressGz([]byte("not-gzip"), filepath.Join(dir, "bad.exe")); err == nil {
		t.Fatal("expected error for corrupt gzip")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			t.Fatalf("leftover temp file found: %s", ent.Name())
		}
	}
}

func TestEnsureExtractedBinaryHashUpdate(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "ffmpeg.exe")

	gzV1 := makeGzipBytes(t, []byte("binary-v1"))
	var logs []string
	logFn := func(tag, _, msg string) {
		logs = append(logs, tag+" "+msg)
	}

	if err := ensureExtractedBinary(gzV1, dest, "ffmpeg.exe", logFn); err != nil {
		t.Fatalf("v1 extract failed: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "首次啟動") {
		t.Fatalf("expected initial extract log, got %v", logs)
	}

	// Same gzV1 should skip extraction
	logs = nil
	if err := ensureExtractedBinary(gzV1, dest, "ffmpeg.exe", logFn); err != nil {
		t.Fatalf("same-version check failed: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected no re-extraction when hash matches, got %v", logs)
	}

	// New gzV2 must overwrite existing binary and update .sha256
	gzV2 := makeGzipBytes(t, []byte("binary-v2-upgraded"))
	if err := ensureExtractedBinary(gzV2, dest, "ffmpeg.exe", logFn); err != nil {
		t.Fatalf("v2 upgrade extract failed: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "版本更新") {
		t.Fatalf("expected update log, got %v", logs)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "binary-v2-upgraded" {
		t.Fatalf("expected updated binary content, got %q", string(got))
	}
	hashBytes, err := os.ReadFile(dest + ".sha256")
	if err != nil || strings.TrimSpace(string(hashBytes)) != sha256Hex(gzV2) {
		t.Fatalf("expected updated sha256 %q, got %q (err=%v)", sha256Hex(gzV2), string(hashBytes), err)
	}
}

func TestDetectVersionsNoFakeFallback(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "corrupt.exe")
	if err := os.WriteFile(bogus, []byte("not an executable"), 0755); err != nil {
		t.Fatal(err)
	}

	if v, err := detectFFmpegVersion(bogus); err == nil || v != "" {
		t.Fatalf("expected error and empty version for bogus ffmpeg, got v=%q err=%v", v, err)
	}
	if v, err := detectFFmpegVersion(filepath.Join(dir, "missing.exe")); err == nil || v != "" {
		t.Fatalf("expected error for missing ffmpeg, got v=%q err=%v", v, err)
	}
}

func TestRenameWithRetry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.tmp")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := renameWithRetry(src, dst); err != nil {
		t.Fatalf("renameWithRetry failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("unexpected dst content: %q, err=%v", string(got), err)
	}

	// Overwrite existing destination file
	src2 := filepath.Join(dir, "src2.tmp")
	if err := os.WriteFile(src2, []byte("updated-payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := renameWithRetry(src2, dst); err != nil {
		t.Fatalf("renameWithRetry overwrite failed: %v", err)
	}
	got2, err := os.ReadFile(dst)
	if err != nil || string(got2) != "updated-payload" {
		t.Fatalf("unexpected overwritten dst content: %q, err=%v", string(got2), err)
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be moved, stat err=%v", src2, err)
	}
}

func TestResolveOverrideToolsFFmpeg(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Empty dir1 should fall through to ffmpeg.exe in dir2
	ff2 := filepath.Join(dir2, "ffmpeg.exe")
	if err := os.WriteFile(ff2, []byte("ff"), 0755); err != nil {
		t.Fatal(err)
	}

	if gotFf := resolveOverrideTools([]string{dir1, dir2}); gotFf != ff2 {
		t.Fatalf("resolveOverrideTools = %q, want %q", gotFf, ff2)
	}

	// Empty override list returns empty string
	if gotEmpty := resolveOverrideTools([]string{dir1}); gotEmpty != "" {
		t.Fatalf("resolveOverrideTools missing = %q, want empty", gotEmpty)
	}
}

func TestEnsureBinariesSelfHealCorruptManagedFFmpeg(t *testing.T) {
	localApp := t.TempDir()
	t.Setenv("LOCALAPPDATA", localApp)

	managedBinDir := filepath.Join(localApp, "AniDownloader", "bin")
	if err := os.MkdirAll(managedBinDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write corrupt bytes into managed ffmpeg.exe while keeping a matching .sha256
	corruptFF := filepath.Join(managedBinDir, "ffmpeg.exe")
	if err := os.WriteFile(corruptFF, []byte("corrupt-ffmpeg-payload"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptFF+".sha256", []byte(sha256Hex(ffmpegGz)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var logs []string
	info, err := EnsureBinaries(func(tag, _, msg string) {
		logs = append(logs, tag+" "+msg)
	})
	if err != nil {
		t.Fatalf("EnsureBinaries self-heal failed: %v", err)
	}
	if info.CoreVersion != NativeHLSEngineVersion || info.FFmpegVersion == "" {
		t.Fatalf("unexpected ToolInfo after self-heal: %+v", info)
	}
	var sawHealWarning bool
	for _, entry := range logs {
		if strings.Contains(entry, "[警告]") && strings.Contains(entry, "重新解壓修復") {
			sawHealWarning = true
		}
	}
	if !sawHealWarning {
		t.Fatalf("expected self-heal warning log, got %v", logs)
	}
}

func TestFindOptionalYtDlpAndInstallValidation(t *testing.T) {
	origExeFn := osExecutableFn
	origLookPathFn := execLookPathFn
	origDetectVerFn := detectYtDlpVersionFn
	origFindFn := findOptionalYtDlpFn
	origUpdateFn := runYtDlpSelfUpdateFn
	origURL := ytDlpDownloadURL
	origClient := ytDlpHTTPClient
	defer func() {
		osExecutableFn = origExeFn
		execLookPathFn = origLookPathFn
		detectYtDlpVersionFn = origDetectVerFn
		findOptionalYtDlpFn = origFindFn
		runYtDlpSelfUpdateFn = origUpdateFn
		ytDlpDownloadURL = origURL
		ytDlpHTTPClient = origClient
	}()

	tmpExeDir := t.TempDir()
	tmpLocalApp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpLocalApp)
	t.Setenv("APPDATA", tmpLocalApp)
	t.Setenv("TEMP", tmpLocalApp)
	t.Setenv("TMP", tmpLocalApp)
	t.Setenv("PATH", t.TempDir())

	osExecutableFn = func() (string, error) { return filepath.Join(tmpExeDir, "AniDownloader.exe"), nil }
	execLookPathFn = func(file string) (string, error) { return "", errors.New("not found") }

	// 1. Not installed -> empty strings
	if p, v := FindOptionalYtDlp(); p != "" || v != "" {
		t.Fatalf("FindOptionalYtDlp() = (%q, %q), want empty when absent", p, v)
	}

	// 2. HTML error page rejected (must not overwrite or create yt-dlp.exe)
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append([]byte("<!DOCTYPE html><html>error</html>"), bytes.Repeat([]byte("A"), 2<<20)...))
	}))
	defer badSrv.Close()
	ytDlpDownloadURL = badSrv.URL
	ytDlpHTTPClient = badSrv.Client()
	if _, err := InstallOrUpdateYtDlp(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "MZ") {
		t.Fatalf("expected MZ PE header error on HTML body, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpLocalApp, "AniDownloader", "bin", "yt-dlp.exe")); !os.IsNotExist(err) {
		t.Fatalf("invalid download must not create yt-dlp.exe, stat err=%v", err)
	}

	// 3. Valid MZ payload (>1 MiB) installs cleanly into LOCALAPPDATA\AniDownloader\bin\yt-dlp.exe
	goodPayload := append([]byte{'M', 'Z'}, bytes.Repeat([]byte{0x90}, (1<<20)+128)...)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(goodPayload)
	}))
	defer goodSrv.Close()
	ytDlpDownloadURL = goodSrv.URL
	ytDlpHTTPClient = goodSrv.Client()
	detectYtDlpVersionFn = func(path string) (string, error) { return "2026.03.13", nil }

	info, err := InstallOrUpdateYtDlp(context.Background(), nil)
	if err != nil {
		t.Fatalf("InstallOrUpdateYtDlp install failed: %v", err)
	}
	wantManaged := filepath.Join(tmpLocalApp, "AniDownloader", "bin", "yt-dlp.exe")
	if info.YtDlpPath != wantManaged || info.YtDlpVersion != "v2026.03.13" {
		t.Fatalf("unexpected ToolInfo after install: %+v", info)
	}

	// Verify V/v prefix normalization in FindOptionalYtDlp
	detectYtDlpVersionFn = func(path string) (string, error) { return "V2026.04.01", nil }
	if p, v := FindOptionalYtDlp(); p != wantManaged || v != "v2026.04.01" {
		t.Fatalf("FindOptionalYtDlp normalized = (%q, %q), want (%q, v2026.04.01)", p, v, wantManaged)
	}
	detectYtDlpVersionFn = func(path string) (string, error) { return "v2026.05.01", nil }
	if p, v := FindOptionalYtDlp(); p != wantManaged || v != "v2026.05.01" {
		t.Fatalf("FindOptionalYtDlp already-prefixed = (%q, %q), want (%q, v2026.05.01)", p, v, wantManaged)
	}
	detectYtDlpVersionFn = func(path string) (string, error) { return "2026.03.13", nil }

	// 4. Subsequent InstallOrUpdateYtDlp sees existing binary and runs -U updater
	var updateCalled bool
	runYtDlpSelfUpdateFn = func(ctx context.Context, path string, logFn func(tag, color, msg string)) error {
		updateCalled = true
		if path != wantManaged {
			t.Fatalf("update path = %q, want %q", path, wantManaged)
		}
		return nil
	}
	if _, err := InstallOrUpdateYtDlp(context.Background(), nil); err != nil {
		t.Fatalf("InstallOrUpdateYtDlp update failed: %v", err)
	}
	if !updateCalled {
		t.Fatal("expected runYtDlpSelfUpdateFn (-U) to run when yt-dlp.exe is already installed")
	}

	// 5. Fallback to "已安裝" when version command fails
	detectYtDlpVersionFn = func(path string) (string, error) { return "", errors.New("boom") }
	if p, v := FindOptionalYtDlp(); p != wantManaged || v != "已安裝" {
		t.Fatalf("FindOptionalYtDlp fallback = (%q, %q), want (%q, 已安裝)", p, v, wantManaged)
	}
}
