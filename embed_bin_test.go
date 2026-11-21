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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func runEmbeddedFFmpegFlag(t *testing.T, ffPath, flag string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffPath, "-hide_banner", flag)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %s failed: %v\noutput:\n%s", flag, err, string(out))
	}
	return string(out)
}

func parseFFmpegTrimmedLines(output string) map[string]bool {
	set := make(map[string]bool)
	for _, rawLine := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(rawLine); trimmed != "" {
			set[trimmed] = true
		}
	}
	return set
}

func parseFFmpegFormatNames(output string) map[string]bool {
	set := make(map[string]bool)
	inTable := false
	for _, rawLine := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if !inTable {
			if strings.HasPrefix(trimmed, "--") {
				inTable = true
			}
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		for _, name := range strings.Split(fields[1], ",") {
			if n := strings.TrimSpace(name); n != "" {
				set[n] = true
			}
		}
	}
	return set
}

func TestEmbeddedFFmpegCapabilities(t *testing.T) {
	ffPath := filepath.Join(t.TempDir(), "ffmpeg.exe")
	if err := decompressGz(ffmpegGz, ffPath); err != nil {
		t.Fatalf("decompress embedded ffmpeg.exe failed: %v", err)
	}

	ver, err := detectFFmpegVersion(ffPath)
	if err != nil {
		t.Fatalf("detectFFmpegVersion failed: %v", err)
	}
	if ver == "" || !strings.HasPrefix(ver, "v") {
		t.Fatalf("unexpected ffmpeg version %q, want non-empty v-prefixed string", ver)
	}

	protocolsOut := runEmbeddedFFmpegFlag(t, ffPath, "-protocols")
	protocols := parseFFmpegTrimmedLines(protocolsOut)
	for _, want := range []string{"file", "http", "https", "tls", "crypto"} {
		if !protocols[want] {
			t.Errorf("embedded ffmpeg missing required protocol %q\noutput:\n%s", want, protocolsOut)
		}
	}

	demuxersOut := runEmbeddedFFmpegFlag(t, ffPath, "-demuxers")
	demuxers := parseFFmpegFormatNames(demuxersOut)
	for _, want := range []string{"hls", "mpegts", "mov", "matroska", "concat"} {
		if !demuxers[want] {
			t.Errorf("embedded ffmpeg missing required demuxer %q\noutput:\n%s", want, demuxersOut)
		}
	}

	muxersOut := runEmbeddedFFmpegFlag(t, ffPath, "-muxers")
	muxers := parseFFmpegFormatNames(muxersOut)
	for _, want := range []string{"mp4", "matroska"} {
		if !muxers[want] {
			t.Errorf("embedded ffmpeg missing required muxer %q\noutput:\n%s", want, muxersOut)
		}
	}

	bsfsOut := runEmbeddedFFmpegFlag(t, ffPath, "-bsfs")
	bsfs := parseFFmpegTrimmedLines(bsfsOut)
	if !bsfs["aac_adtstoasc"] {
		t.Errorf("embedded ffmpeg missing required bitstream filter %q\noutput:\n%s", "aac_adtstoasc", bsfsOut)
	}

	decodersOut := runEmbeddedFFmpegFlag(t, ffPath, "-decoders")
	decoders := parseFFmpegFormatNames(decodersOut)
	for _, want := range []string{"h264", "aac", "aac_latm"} {
		if !decoders[want] {
			t.Errorf("embedded ffmpeg missing required decoder %q\noutput:\n%s", want, decodersOut)
		}
	}
}

func TestEnsureBinariesReadyLogFormat(t *testing.T) {
	origFindFn := findOptionalYtDlpFn
	defer func() {
		findOptionalYtDlpFn = origFindFn
	}()

	t.Setenv("LOCALAPPDATA", t.TempDir())

	// Case A: yt-dlp absent -> single-parenthesis message without nested "((" or "))"
	findOptionalYtDlpFn = func() (string, string) { return "", "" }
	var logsAbsent []string
	infoAbsent, err := EnsureBinaries(func(tag, _, msg string) {
		logsAbsent = append(logsAbsent, tag+" "+msg)
	})
	if err != nil {
		t.Fatalf("EnsureBinaries (absent yt-dlp) failed: %v", err)
	}
	if infoAbsent.YtDlpPath != "" || infoAbsent.YtDlpVersion != "" {
		t.Fatalf("expected empty yt-dlp info, got %+v", infoAbsent)
	}
	var foundAbsentReady bool
	for _, entry := range logsAbsent {
		if strings.Contains(entry, "內建模組就緒：") {
			foundAbsentReady = true
			if !strings.Contains(entry, "YT-DLP (未安裝，可於設定一鍵安裝)") {
				t.Fatalf("unexpected absent yt-dlp ready log: %q", entry)
			}
			if strings.Contains(entry, "))") {
				t.Fatalf("ready log must not contain nested parentheses '))': %q", entry)
			}
		}
	}
	if !foundAbsentReady {
		t.Fatalf("expected ready log entry, got %v", logsAbsent)
	}

	// Case B: yt-dlp installed -> shows version
	findOptionalYtDlpFn = func() (string, string) { return `C:\tools\yt-dlp.exe`, "v2026.10.26" }
	var logsInstalled []string
	infoInstalled, err := EnsureBinaries(func(tag, _, msg string) {
		logsInstalled = append(logsInstalled, tag+" "+msg)
	})
	if err != nil {
		t.Fatalf("EnsureBinaries (installed yt-dlp) failed: %v", err)
	}
	if infoInstalled.YtDlpVersion != "v2026.10.26" {
		t.Fatalf("unexpected yt-dlp version: %+v", infoInstalled)
	}
	var foundInstalledReady bool
	for _, entry := range logsInstalled {
		if strings.Contains(entry, "內建模組就緒：") {
			foundInstalledReady = true
			if !strings.Contains(entry, "YT-DLP (v2026.10.26)") {
				t.Fatalf("unexpected installed yt-dlp ready log: %q", entry)
			}
		}
	}
	if !foundInstalledReady {
		t.Fatalf("expected ready log entry, got %v", logsInstalled)
	}
}
