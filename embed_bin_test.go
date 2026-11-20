//go:build windows

package main

import (
	"bytes"
	"compress/gzip"
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
	dest := filepath.Join(dir, "yt-dlp.exe")

	gzV1 := makeGzipBytes(t, []byte("binary-v1"))
	var logs []string
	logFn := func(tag, _, msg string) {
		logs = append(logs, tag+" "+msg)
	}

	if err := ensureExtractedBinary(gzV1, dest, "yt-dlp.exe", logFn); err != nil {
		t.Fatalf("v1 extract failed: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "首次啟動") {
		t.Fatalf("expected initial extract log, got %v", logs)
	}

	// Same gzV1 should skip extraction
	logs = nil
	if err := ensureExtractedBinary(gzV1, dest, "yt-dlp.exe", logFn); err != nil {
		t.Fatalf("same-version check failed: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected no re-extraction when hash matches, got %v", logs)
	}

	// New gzV2 must overwrite existing binary and update .sha256
	gzV2 := makeGzipBytes(t, []byte("binary-v2-upgraded"))
	if err := ensureExtractedBinary(gzV2, dest, "yt-dlp.exe", logFn); err != nil {
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
	if v, err := detectYtDlpVersion(bogus); err == nil || v != "" {
		t.Fatalf("expected error and empty version for bogus yt-dlp, got v=%q err=%v", v, err)
	}
	if v, err := detectFFmpegVersion(filepath.Join(dir, "missing.exe")); err == nil || v != "" {
		t.Fatalf("expected error for missing ffmpeg, got v=%q err=%v", v, err)
	}
	if v, err := detectYtDlpVersion(filepath.Join(dir, "missing.exe")); err == nil || v != "" {
		t.Fatalf("expected error for missing yt-dlp, got v=%q err=%v", v, err)
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

func TestResolveOverrideToolsIndependent(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Only yt-dlp.exe in dir1, only ffmpeg.exe in dir2
	yt1 := filepath.Join(dir1, "yt-dlp.exe")
	ff2 := filepath.Join(dir2, "ffmpeg.exe")
	if err := os.WriteFile(yt1, []byte("yt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ff2, []byte("ff"), 0755); err != nil {
		t.Fatal(err)
	}

	gotYt, gotFf := resolveOverrideTools([]string{dir1, dir2})
	if gotYt != yt1 || gotFf != ff2 {
		t.Fatalf("resolveOverrideTools independent = (%q, %q), want (%q, %q)", gotYt, gotFf, yt1, ff2)
	}

	// Single tool override: only ffmpeg.exe in dir1 leaves ytPath empty
	onlyFfDir := t.TempDir()
	ffOnly := filepath.Join(onlyFfDir, "ffmpeg.exe")
	if err := os.WriteFile(ffOnly, []byte("ff"), 0755); err != nil {
		t.Fatal(err)
	}
	gotYt2, gotFf2 := resolveOverrideTools([]string{onlyFfDir})
	if gotYt2 != "" || gotFf2 != ffOnly {
		t.Fatalf("single override = (%q, %q), want (%q, %q)", gotYt2, gotFf2, "", ffOnly)
	}
}
