//go:build windows

package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "illegal chars and single mp4", in: "a/b:c*d?.mp4", want: "a_b_c_d_"},
		{name: "duplicate mp4 suffix", in: "Episode01.mp4.MP4", want: "Episode01"},
		{name: "reserved CON with trailing dot", in: "CON.", want: "_CON"},
		{name: "reserved lowercase con", in: "con", want: "_con"},
		{name: "reserved PRN", in: "PRN", want: "_PRN"},
		{name: "reserved AUX", in: "aux", want: "_aux"},
		{name: "reserved NUL", in: "NUL.txt", want: "_NUL.txt"},
		{name: "reserved COM1", in: "COM1", want: "_COM1"},
		{name: "reserved COM9", in: "com9", want: "_com9"},
		{name: "reserved LPT1", in: "LPT1", want: "_LPT1"},
		{name: "reserved LPT9", in: "lpt9.mkv", want: "_lpt9.mkv"},
		{name: "control chars and trailing dots", in: "  hello\x00\x1fworld...  ", want: "helloworld"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeFilename(tc.in); got != tc.want {
				t.Fatalf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("overlong filename truncated to 180 runes without trailing dot", func(t *testing.T) {
		long := strings.Repeat("動", 179) + ".abc"
		got := SanitizeFilename(long)
		runes := []rune(got)
		if len(runes) > 180 {
			t.Fatalf("len(runes) = %d, want <= 180", len(runes))
		}
		if strings.HasSuffix(got, ".") {
			t.Fatalf("expected no trailing dot, got %q", got)
		}
		if len(runes) != 179 {
			t.Fatalf("len(runes) = %d, want 179 (dot trimmed after 180-rune slice)", len(runes))
		}
	})

	t.Run("empty or whitespace fallback", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\r\n\t", "..."} {
			got := SanitizeFilename(in)
			if !strings.HasPrefix(got, "巴哈動畫瘋_") {
				t.Fatalf("SanitizeFilename(%q) = %q, want fallback prefix", in, got)
			}
		}
	})
}

func TestSanitizeHeaderValue(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
	if got := sanitizeHeaderValue("  " + ua + "  "); got != ua {
		t.Fatalf("got %q, want %q", got, ua)
	}

	injected := "Mozilla/5.0\r\nX-Injected: 1\nEvil: true\x00"
	got := sanitizeHeaderValue(injected)
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("control chars remained in %q", got)
	}
	if got != "Mozilla/5.0X-Injected: 1Evil: true" {
		t.Fatalf("unexpected sanitized value: %q", got)
	}
}

func TestValidateDownloadURL(t *testing.T) {
	valid := []string{
		"https://bahamut.akamaized.net/1080p/playlist.m3u8?token=abc",
		"  http://example.com/live.m3u8  ",
	}
	for _, raw := range valid {
		u, err := validateDownloadURL(raw)
		if err != nil || u == "" {
			t.Fatalf("expected valid URL for %q, got err=%v", raw, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"ftp://example.com/a.m3u8",
		"file:///C:/test.m3u8",
		"https:///missing-host.m3u8",
		"https://example.com/a.m3u8\r\nX-Test: 1",
		"https://example.com/a.m3u8\x00",
		"not-a-url",
	}
	for _, raw := range invalid {
		if _, err := validateDownloadURL(raw); err == nil {
			t.Fatalf("expected error for invalid URL %q", raw)
		}
	}
}

func TestBuildYtDlpArgsAndEnv(t *testing.T) {
	opt := DownloadOptions{
		M3U8Url:        "https://example.com/playlist.m3u8",
		Title:          "Test",
		OutputDir:      `C:\Videos`,
		Engine:         "ytdlp",
		RefererEnabled: true,
		UserAgent:      "CustomUA/1.0\r\nX-Evil: 1",
		JA3:            "771,4865",
		Akamai:         "1:65536",
		Concurrency:    1,
		YtDlpPath:      `C:\bin\yt-dlp.exe`,
		FFmpegPath:     `C:\bin\ffmpeg.exe`,
	}

	args := buildYtDlpArgs(opt, opt.UserAgent, opt.JA3, opt.Akamai, `C:\Videos\Test.%(ext)s`)
	if slices.Contains(args, "--concurrent-fragments") {
		t.Fatalf("unexpected redundant --concurrent-fragments in %v", args)
	}
	if !containsPair(args, "-N", "1") {
		t.Fatalf("expected -N 1 in %v", args)
	}
	if !containsPair(args, "--remux-video", "mp4") || !containsPair(args, "--merge-output-format", "mp4") {
		t.Fatalf("expected mp4 remux and merge flags in %v", args)
	}
	if !slices.Contains(args, "--force-overwrites") {
		t.Fatalf("expected --force-overwrites flag in %v", args)
	}
	if !containsPair(args, "--impersonate", "chrome") {
		t.Fatalf("expected --impersonate chrome when JA3/Akamai non-empty, got %v", args)
	}
	if !containsPair(args, "--user-agent", "CustomUA/1.0X-Evil: 1") {
		t.Fatalf("expected sanitized UA in %v", args)
	}

	argsNoFP := buildYtDlpArgs(opt, "", "", "", `C:\Videos\Test.%(ext)s`)
	if slices.Contains(argsNoFP, "--impersonate") {
		t.Fatalf("did not expect --impersonate when both JA3 and Akamai are empty, got %v", argsNoFP)
	}
	if !containsPair(argsNoFP, "--user-agent", DefaultUserAgent) {
		t.Fatalf("expected fallback DefaultUserAgent when UA empty, got %v", argsNoFP)
	}

	optNoRef := opt
	optNoRef.RefererEnabled = false
	argsNoRef := buildYtDlpArgs(optNoRef, opt.UserAgent, opt.JA3, opt.Akamai, `C:\Videos\Test.%(ext)s`)
	if slices.Contains(argsNoRef, "--impersonate") || slices.Contains(argsNoRef, "--add-header") {
		t.Fatalf("did not expect referer/impersonate flags when RefererEnabled=false, got %v", argsNoRef)
	}

	env := buildDownloadEnv([]string{
		"PATH=C:\\Windows\\System32",
		"YT_DLP_JA3=stale",
		"YT_DLP_AKAMAI=stale",
	})
	for _, kv := range env {
		if strings.HasPrefix(kv, "YT_DLP_JA3=") || strings.HasPrefix(kv, "YT_DLP_AKAMAI=") {
			t.Fatalf("unexpected invalid env var injected: %s", kv)
		}
	}
	if !slices.Contains(env, "PYTHONUTF8=1") || !slices.Contains(env, "PYTHONIOENCODING=utf-8:replace") {
		t.Fatalf("missing UTF-8 Python env vars in %v", env)
	}
}

func TestBuildFFmpegArgs(t *testing.T) {
	opt := DownloadOptions{
		M3U8Url:        "https://example.com/live.m3u8",
		RefererEnabled: true,
	}
	args := buildFFmpegArgs(opt, "TestUA/1.0\r\nX-Injected: 1", `C:\Videos\out.mp4`)
	idx := slices.Index(args, "-headers")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("expected -headers in %v", args)
	}
	headers := args[idx+1]
	lines := strings.Split(strings.TrimSuffix(headers, "\r\n"), "\r\n")
	if len(lines) != 3 {
		t.Fatalf("expected exactly 3 header lines (no CRLF injection), got %d: %q", len(lines), headers)
	}
	if lines[2] != "User-Agent: TestUA/1.0X-Injected: 1" {
		t.Fatalf("unexpected User-Agent header line: %q", lines[2])
	}

	opt.RefererEnabled = false
	argsOff := buildFFmpegArgs(opt, "TestUA", `C:\Videos\out.mp4`)
	if slices.Contains(argsOff, "-headers") {
		t.Fatalf("did not expect -headers when RefererEnabled=false, got %v", argsOff)
	}
}

func TestParseHMSAndProgressRegex(t *testing.T) {
	if got := parseHMS("01", "02", "03.5"); got != 3723.5 {
		t.Fatalf("parseHMS = %v, want 3723.5", got)
	}

	ytLine := "[download]  42.7% of ~  120.50MiB at    3.45MiB/s ETA 00:22 (frag 18/42)"
	if m := ytdlpPctRegex.FindStringSubmatch(ytLine); len(m) != 2 || m[1] != "42.7" {
		t.Fatalf("ytdlpPctRegex mismatch: %v", m)
	}
	if m := ytdlpSpeedRegex.FindStringSubmatch(ytLine); len(m) != 2 || m[1] != "3.45MiB/s" {
		t.Fatalf("ytdlpSpeedRegex mismatch: %v", m)
	}
	if m := ytdlpFragRegex.FindStringSubmatch(ytLine); len(m) != 3 || m[1] != "18" || m[2] != "42" {
		t.Fatalf("ytdlpFragRegex mismatch: %v", m)
	}

	ffDur := "Duration: 00:24:10.50, start: 0.000000, bitrate: N/A"
	if m := ffmpegDurRegex.FindStringSubmatch(ffDur); len(m) != 4 || m[1] != "00" || m[2] != "24" || m[3] != "10.50" {
		t.Fatalf("ffmpegDurRegex mismatch: %v", m)
	}
	ffProg := "frame= 1200 fps= 60 q=-1.0 size= 15360kB time=00:05:12.25 bitrate= 403.0kbits/s speed=4.12x"
	if m := ffmpegTimeRegex.FindStringSubmatch(ffProg); len(m) != 4 || m[2] != "05" || m[3] != "12.25" {
		t.Fatalf("ffmpegTimeRegex mismatch: %v", m)
	}
	if m := ffmpegSpeedRegex.FindStringSubmatch(ffProg); len(m) != 2 || m[1] != "4.12x" {
		t.Fatalf("ffmpegSpeedRegex mismatch: %v", m)
	}
}

func TestBuildYtDlpOutTemplatePercentEscaping(t *testing.T) {
	dir := `C:\Users\100%Anime\Downloads`
	title := `Ep01_100%_%(title)s`
	got := buildYtDlpOutTemplate(dir, title)
	want := `C:\Users\100%%Anime\Downloads\Ep01_100%%_%%(title)s.%(ext)s`
	if got != want {
		t.Fatalf("buildYtDlpOutTemplate(%q, %q) = %q, want %q", dir, title, got, want)
	}
}

func TestRunDownloadValidationWithNilCallbacks(t *testing.T) {
	_, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url: "https://example.com/a.m3u8\r\nBad",
	}, DownloadCallbacks{})
	if err == nil {
		t.Fatal("expected error for invalid URL with CRLF")
	}

	// Valid URL + missing executable exercises nil OnLog / OnProgress guards inside RunDownload
	missingDir := t.TempDir()
	_, err = RunDownload(context.Background(), DownloadOptions{
		M3U8Url:        "https://example.com/live.m3u8",
		Title:          "NilCallbackCheck",
		OutputDir:      t.TempDir(),
		Engine:         "ytdlp",
		RefererEnabled: true,
		JA3:            DefaultJA3,
		YtDlpPath:      filepath.Join(missingDir, "missing-yt-dlp.exe"),
		FFmpegPath:     filepath.Join(missingDir, "missing-ffmpeg.exe"),
	}, DownloadCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "啟動 YT-DLP 失敗") {
		t.Fatalf("expected missing yt-dlp start error without panic, got %v", err)
	}
}

func containsPair(slice []string, a, b string) bool {
	for i := 0; i+1 < len(slice); i++ {
		if slice[i] == a && slice[i+1] == b {
			return true
		}
	}
	return false
}
