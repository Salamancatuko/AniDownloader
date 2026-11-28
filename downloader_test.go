//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestParseHMSAndFFmpegProgressRegex(t *testing.T) {
	if got := parseHMS("01", "02", "03.5"); got != 3723.5 {
		t.Fatalf("parseHMS = %v, want 3723.5", got)
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

func TestRunDownloadEngineRouting(t *testing.T) {
	origHook := hlsRunNativeDownloadFn
	t.Cleanup(func() {
		hlsRunNativeDownloadFn = origHook
	})

	var calledWith []DownloadOptions
	hlsRunNativeDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		calledWith = append(calledWith, opt)
		return filepath.Join(opt.OutputDir, opt.Title+".mp4"), nil
	}

	for _, rawEngine := range []string{"hls", "ytdlp", ""} {
		outDir := t.TempDir()
		out, err := RunDownload(context.Background(), DownloadOptions{
			M3U8Url:        "https://example.com/playlist.m3u8",
			Title:          "RouteCheck",
			OutputDir:      outDir,
			Engine:         rawEngine,
			RefererEnabled: true,
			Concurrency:    8,
		}, DownloadCallbacks{})
		if err != nil {
			t.Fatalf("RunDownload(engine=%q) failed: %v", rawEngine, err)
		}
		if out != filepath.Join(outDir, "RouteCheck.mp4") {
			t.Fatalf("unexpected output path %q", out)
		}
	}

	if len(calledWith) != 3 {
		t.Fatalf("expected 3 native hook invocations, got %d", len(calledWith))
	}
	for i, got := range calledWith {
		if got.Engine != "hls" {
			t.Fatalf("call %d: normalized Engine = %q, want hls", i, got.Engine)
		}
		if got.Concurrency != DefaultConcurrency {
			t.Fatalf("call %d: Concurrency = %d, want %d", i, got.Concurrency, DefaultConcurrency)
		}
	}

	// Nil hook check
	hlsRunNativeDownloadFn = nil
	if _, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url:   "https://example.com/playlist.m3u8",
		OutputDir: t.TempDir(),
		Engine:    "hls",
	}, DownloadCallbacks{}); err == nil || !strings.Contains(err.Error(), "原生 HLS 引擎尚未註冊") {
		t.Fatalf("expected missing native engine hook error, got %v", err)
	}

	// Engine "ffmpeg" routes to FFmpeg branch without calling hlsRunNativeDownloadFn
	nativeCalled := false
	hlsRunNativeDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		nativeCalled = true
		return "", nil
	}
	missingDir := t.TempDir()
	_, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url:        "https://example.com/live.m3u8",
		Title:          "FFmpegRouteCheck",
		OutputDir:      t.TempDir(),
		Engine:         "ffmpeg",
		RefererEnabled: true,
		FFmpegPath:     filepath.Join(missingDir, "missing-ffmpeg.exe"),
	}, DownloadCallbacks{})
	if nativeCalled {
		t.Fatal("Engine=ffmpeg must NOT call hlsRunNativeDownloadFn")
	}
	if err == nil || !strings.Contains(err.Error(), "啟動 FFMPEG 失敗") {
		t.Fatalf("expected missing ffmpeg start error, got %v", err)
	}
}

func TestRunDownloadValidationWithNilCallbacks(t *testing.T) {
	_, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url: "https://example.com/a.m3u8\r\nBad",
	}, DownloadCallbacks{})
	if err == nil {
		t.Fatal("expected error for invalid URL with CRLF")
	}

	// Valid URL + missing ffmpeg executable exercises nil OnLog / OnProgress guards inside RunDownload
	missingDir := t.TempDir()
	_, err = RunDownload(context.Background(), DownloadOptions{
		M3U8Url:        "https://example.com/live.m3u8",
		Title:          "NilCallbackCheck",
		OutputDir:      t.TempDir(),
		Engine:         "ffmpeg",
		RefererEnabled: true,
		JA3:            DefaultJA3,
		FFmpegPath:     filepath.Join(missingDir, "missing-ffmpeg.exe"),
	}, DownloadCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "啟動 FFMPEG 失敗") {
		t.Fatalf("expected missing ffmpeg start error without panic, got %v", err)
	}

	// Empty FFmpegPath fails before launching any command
	_, err = RunDownload(context.Background(), DownloadOptions{
		M3U8Url:    "https://example.com/live.m3u8",
		Title:      "EmptyFFmpegPathCheck",
		OutputDir:  t.TempDir(),
		Engine:     "ffmpeg",
		FFmpegPath: "   ",
	}, DownloadCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "找不到本機 FFMPEG 執行檔路徑") {
		t.Fatalf("expected empty FFmpegPath error, got %v", err)
	}
}

func TestIsLikelyWebpageURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{url: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", want: true},
		{url: "https://youtu.be/abc", want: true},
		{url: "https://music.youtube.com/watch?v=xyz", want: true},
		{url: "https://www.bilibili.com/video/BV1xx411c7mD", want: true},
		{url: "https://ani.gamer.com.tw/animeVideo.php?sn=12345", want: true},
		{url: "https://bahamut.akamaized.net/12345/playlist_basic.m3u8?token=123", want: false},
		{url: "https://www.youtube.com/live/playlist.m3u8", want: false},
		{url: "https://cdn.example.com/stream/video.m3u?x=1", want: false},
		{url: "https://cdn.example.com/watch?id=999", want: false},
	}
	for _, tc := range cases {
		if got := isLikelyWebpageURL(tc.url); got != tc.want {
			t.Errorf("isLikelyWebpageURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestRunDownloadHybridRoutingYouTube(t *testing.T) {
	origYtDlpFn := runExternalYtDlpFn
	origNativeFn := hlsRunNativeDownloadFn
	defer func() {
		runExternalYtDlpFn = origYtDlpFn
		hlsRunNativeDownloadFn = origNativeFn
	}()

	var (
		nativeCalled   bool
		ytCalled       bool
		capturedOpt    DownloadOptions
		capturedRawTit string
	)
	hlsRunNativeDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		nativeCalled = true
		return "", nil
	}
	runExternalYtDlpFn = func(ctx context.Context, opt DownloadOptions, rawTitle string, cb DownloadCallbacks) (string, error) {
		ytCalled = true
		capturedOpt = opt
		capturedRawTit = rawTitle
		return filepath.Join(opt.OutputDir, "Mock Video.mp4"), nil
	}

	outDir := t.TempDir()
	outPath, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url:        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		Title:          "",
		OutputDir:      outDir,
		Engine:         "hls",
		RefererEnabled: false,
		FFmpegPath:     filepath.Join(t.TempDir(), "ffmpeg.exe"),
	}, DownloadCallbacks{})
	if err != nil {
		t.Fatalf("RunDownload youtube failed: %v", err)
	}
	if nativeCalled {
		t.Fatal("expected native HLS NOT to be called for youtube webpage URL")
	}
	if !ytCalled {
		t.Fatal("expected external yt-dlp router to be called")
	}
	if capturedRawTit != "" {
		t.Fatalf("capturedRawTit = %q, want empty so yt-dlp auto-fills video title", capturedRawTit)
	}
	if outPath != filepath.Join(outDir, "Mock Video.mp4") {
		t.Fatalf("outPath = %q, want Mock Video.mp4", outPath)
	}

	args := buildYtDlpArgs(capturedOpt, capturedRawTit, "")
	for _, forbidden := range []string{"--add-header", "--referer", "--user-agent"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("RefererEnabled=false must NOT attach %s in args: %v", forbidden, args)
		}
	}
	outTpl := buildYtDlpOutTemplate(outDir, "")
	if !strings.Contains(outTpl, "%(title).120B.%(ext)s") || strings.Contains(outTpl, "巴哈動畫瘋_") {
		t.Fatalf("auto-title outTemplate = %q, want %%(title).120B.%%(ext)s", outTpl)
	}

	t.Setenv("YT_DLP_JA3", "bad-ja3")
	t.Setenv("YT_DLP_AKAMAI", "bad-akamai")
	env := buildYtDlpEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "YT_DLP_JA3=") || strings.HasPrefix(e, "YT_DLP_AKAMAI=") {
			t.Fatalf("unexpected injected env var in yt-dlp env: %s", e)
		}
	}
	if !slices.Contains(env, "PYTHONUTF8=1") || !slices.Contains(env, "PYTHONIOENCODING=utf-8:replace") {
		t.Fatalf("expected UTF-8 Python env vars, got %v", env)
	}
}

func TestRunDownloadFallbackToYtDlpOnNonM3U8Webpage(t *testing.T) {
	if !looksLikeHTMLWebpage([]byte("\xef\xbb\xbf  <!DOCTYPE html><html><head></head></html>")) {
		t.Fatal("expected looksLikeHTMLWebpage to detect BOM + <!DOCTYPE html>")
	}
	if looksLikeHTMLWebpage([]byte("#EXTM3U\n#EXT-X-VERSION:3\n")) {
		t.Fatal("expected #EXTM3U not to be flagged as HTML")
	}

	origYtDlpFn := runExternalYtDlpFn
	origNativeFn := hlsRunNativeDownloadFn
	defer func() {
		runExternalYtDlpFn = origYtDlpFn
		hlsRunNativeDownloadFn = origNativeFn
	}()

	var nativeCalled, ytCalled bool
	hlsRunNativeDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		nativeCalled = true
		return "", fmt.Errorf("%w: not valid playlist", errNonM3U8Webpage)
	}
	runExternalYtDlpFn = func(ctx context.Context, opt DownloadOptions, rawTitle string, cb DownloadCallbacks) (string, error) {
		ytCalled = true
		return filepath.Join(opt.OutputDir, "Fallback.mp4"), nil
	}

	outPath, err := RunDownload(context.Background(), DownloadOptions{
		M3U8Url:   "https://unknown-video-host.example/watch?id=42",
		Title:     "CustomName",
		OutputDir: t.TempDir(),
		Engine:    "hls",
	}, DownloadCallbacks{})
	if err != nil {
		t.Fatalf("RunDownload HTML fallback failed: %v", err)
	}
	if !nativeCalled || !ytCalled {
		t.Fatalf("expected nativeCalled=true and ytCalled=true, got native=%v yt=%v", nativeCalled, ytCalled)
	}
	if !strings.HasSuffix(outPath, "Fallback.mp4") {
		t.Fatalf("unexpected fallback outPath: %q", outPath)
	}
}

func TestRunExternalYtDlpMissingBinaryError(t *testing.T) {
	origFindFn := findOptionalYtDlpFn
	defer func() { findOptionalYtDlpFn = origFindFn }()
	findOptionalYtDlpFn = func() (string, string) { return "", "" }

	var logs []string
	_, err := runExternalYtDlp(context.Background(), DownloadOptions{
		M3U8Url:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		OutputDir: t.TempDir(),
		YtDlpPath: "",
	}, "", DownloadCallbacks{
		OnLog: func(tag, _, msg string) {
			logs = append(logs, tag+" "+msg)
		},
	})
	if err == nil {
		t.Fatal("expected error when yt-dlp.exe is missing")
	}
	if !strings.Contains(err.Error(), "Settings") || !strings.Contains(err.Error(), "安裝") {
		t.Fatalf("expected error guiding user to Settings to install yt-dlp.exe, got: %v", err)
	}
	if !errors.Is(err, err) || len(logs) == 0 || !strings.Contains(logs[0], "[警告]") {
		t.Fatalf("expected warning log when yt-dlp missing, got %v", logs)
	}
}

func TestBuildYtDlpArgs(t *testing.T) {
	outDir := t.TempDir()
	argsNoHeaders := buildYtDlpArgs(DownloadOptions{
		M3U8Url:        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		OutputDir:      outDir,
		RefererEnabled: false,
		FFmpegPath:     filepath.Join(outDir, "ffmpeg.exe"),
	}, "", "")

	for _, required := range []string{
		"--no-playlist",
		"--no-quiet",
		"--progress",
		"--newline",
		"--print",
		"after_move:__ANIDL_OUTFILE__:%(filepath)s",
		"-N",
		"1",
	} {
		if !slices.Contains(argsNoHeaders, required) {
			t.Fatalf("buildYtDlpArgs missing %q in %v", required, argsNoHeaders)
		}
	}
	for _, forbidden := range []string{"--add-header", "--referer", "--user-agent"} {
		if slices.Contains(argsNoHeaders, forbidden) {
			t.Fatalf("RefererEnabled=false must not include %q: %v", forbidden, argsNoHeaders)
		}
	}

	argsWithHeaders := buildYtDlpArgs(DownloadOptions{
		M3U8Url:        "https://ani.gamer.com.tw/animeVideo.php?sn=1",
		OutputDir:      outDir,
		RefererEnabled: true,
		UserAgent:      "Agent/1.0",
	}, "MyEpisode", "")
	if !slices.Contains(argsWithHeaders, "--no-playlist") {
		t.Fatalf("RefererEnabled=true missing --no-playlist flag: %v", argsWithHeaders)
	}
	if !slices.Contains(argsWithHeaders, "--add-header") || !slices.Contains(argsWithHeaders, "--user-agent") {
		t.Fatalf("RefererEnabled=true missing header flags: %v", argsWithHeaders)
	}
	if !slices.Contains(argsWithHeaders, filepath.Join(outDir, "MyEpisode.%(ext)s")) {
		t.Fatalf("expected sanitized custom title template, got %v", argsWithHeaders)
	}
}

func TestBuildYtDlpEnv(t *testing.T) {
	t.Setenv("YT_DLP_JA3", "stale-ja3")
	t.Setenv("YT_DLP_AKAMAI", "stale-akamai")
	t.Setenv("PYTHONUNBUFFERED", "0")

	env := buildYtDlpEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "YT_DLP_JA3=") || strings.HasPrefix(e, "YT_DLP_AKAMAI=") || e == "PYTHONUNBUFFERED=0" {
			t.Fatalf("unexpected leaked or un-overridden env entry: %s", e)
		}
	}
	for _, want := range []string{
		"PYTHONUNBUFFERED=1",
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8:replace",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("buildYtDlpEnv missing %s in env", want)
		}
	}
}

func TestParseYtDlpOutputLine(t *testing.T) {
	t.Run("StandardYouTubeDownloadAndCompletion", func(t *testing.T) {
		var (
			updates []ProgressUpdate
			logs    []string
			lastAt  time.Time
			lastLog time.Time
		)
		cb := DownloadCallbacks{
			OnProgress: func(p ProgressUpdate) { updates = append(updates, p) },
			OnLog:      func(tag, _, msg string) { logs = append(logs, tag+" "+msg) },
		}

		parseYtDlpOutputLine("[download]  42.5% of   15.23MiB at    3.20MiB/s ETA 00:03", &lastAt, &lastLog, cb)
		if len(updates) != 1 || updates[0].Engine != "hls" || updates[0].Percent != 42.5 || updates[0].Speed != "3.20MiB/s" || updates[0].Detail != "" {
			t.Fatalf("unexpected standard progress update: %+v", updates)
		}

		// Second rapid update within 250ms should be throttled (percent < 100)
		parseYtDlpOutputLine("[download]  45.0% of   15.23MiB at    3.40MiB/s ETA 00:02", &lastAt, &lastLog, cb)
		if len(updates) != 1 {
			t.Fatalf("expected 250ms throttle to suppress immediate update, got %d updates", len(updates))
		}

		// 100% completion bypasses throttle
		parseYtDlpOutputLine("[download] 100% of   15.23MiB in 00:00:05 at 3.04MiB/s", &lastAt, &lastLog, cb)
		if len(updates) != 2 || updates[1].Percent != 100.0 || updates[1].Speed != "3.04MiB/s" {
			t.Fatalf("expected 100%% update to bypass throttle, got %+v", updates)
		}
		if len(logs) < 2 {
			t.Fatalf("expected [進度] logs emitted at start and 100%%, got %v", logs)
		}
	})

	t.Run("UnknownSpeedAndFragmentDetail", func(t *testing.T) {
		var updates []ProgressUpdate
		var lastAt, lastLog time.Time
		cb := DownloadCallbacks{
			OnProgress: func(p ProgressUpdate) { updates = append(updates, p) },
		}

		parseYtDlpOutputLine("[download]  45.0% of   10.00MiB at  Unknown B/s ETA Unknown", &lastAt, &lastLog, cb)
		if len(updates) != 1 || updates[0].Percent != 45.0 || updates[0].Speed != "" {
			t.Fatalf("expected empty Speed for Unknown B/s, got %+v", updates)
		}

		lastAt = time.Time{}
		parseYtDlpOutputLine("[download]  50.0% of ~  20.00MiB at  1.50MiB/s ETA 00:10 (frag 10/20)", &lastAt, &lastLog, cb)
		if len(updates) != 2 || updates[1].Percent != 50.0 || updates[1].Speed != "1.50MiB/s" || updates[1].Detail != "frag 10/20" {
			t.Fatalf("expected fragment detail, got %+v", updates)
		}
	})

	t.Run("MergerOutfileErrorWarningAndStderrPipe", func(t *testing.T) {
		var (
			mu         sync.Mutex
			cbMu       sync.Mutex
			updates    []ProgressUpdate
			loggedMsgs []string
			lastAt     time.Time
			lastLog    time.Time
			outFile    string
		)
		cb := DownloadCallbacks{
			OnProgress: func(p ProgressUpdate) {
				cbMu.Lock()
				updates = append(updates, p)
				cbMu.Unlock()
			},
			OnLog: func(tag, _, msg string) {
				cbMu.Lock()
				loggedMsgs = append(loggedMsgs, tag+" "+msg)
				cbMu.Unlock()
			},
		}

		stderrSim := strings.NewReader(
			"[youtube] Extracting URL: https://www.youtube.com/watch?v=abc\n" +
				"[download] Destination: C:\\Out\\test.f137.mp4\n" +
				"WARNING: transient warning\n" +
				"[download]  64.0% of   15.23MiB at    3.20MiB/s ETA 00:02\r" +
				`[Merger] Merging formats into "C:\Out\test.mp4"` + "\n",
		)
		stdoutSim := strings.NewReader("__ANIDL_OUTFILE__:Z:\\test\\video.mp4\n")

		handleLine := func(line string) {
			mu.Lock()
			defer mu.Unlock()
			f, isOut := parseYtDlpOutputLine(line, &lastAt, &lastLog, cb)
			if isOut {
				outFile = f
			}
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			scanLinesAndReturns(stderrSim, handleLine)
		}()
		go func() {
			defer wg.Done()
			scanLinesAndReturns(stdoutSim, handleLine)
		}()
		wg.Wait()

		if outFile != `Z:\test\video.mp4` {
			t.Fatalf("outFile = %q, want Z:\\test\\video.mp4", outFile)
		}
		if len(updates) != 2 {
			t.Fatalf("expected 2 progress updates (download + merger), got %+v", updates)
		}
		if updates[0].Engine != "hls" || updates[0].Percent != 64.0 {
			t.Fatalf("unexpected stderr download update: %+v", updates[0])
		}
		if updates[1].Engine != "hls" || !updates[1].Merging || updates[1].Percent != 99.0 {
			t.Fatalf("unexpected merger update: %+v", updates[1])
		}

		var sawYT, sawWarn, sawRemux bool
		for _, msg := range loggedMsgs {
			if strings.Contains(msg, "__ANIDL_OUTFILE__:") {
				t.Fatalf("internal __ANIDL_OUTFILE__: marker leaked into console logs: %q", msg)
			}
			if strings.HasPrefix(msg, "[YT-DLP]") {
				sawYT = true
			}
			if strings.HasPrefix(msg, "[警告]") {
				sawWarn = true
			}
			if strings.HasPrefix(msg, "[封裝]") {
				sawRemux = true
			}
		}
		if !sawYT || !sawWarn || !sawRemux {
			t.Fatalf("missing expected log categories (yt=%v warn=%v remux=%v): %v", sawYT, sawWarn, sawRemux, loggedMsgs)
		}

		// Verify ERROR: classification
		loggedMsgs = nil
		parseYtDlpOutputLine("ERROR: unable to download video data", &lastAt, &lastLog, cb)
		if len(loggedMsgs) != 1 || !strings.HasPrefix(loggedMsgs[0], "[錯誤]") {
			t.Fatalf("expected [錯誤] classification, got %v", loggedMsgs)
		}
	})
}

func TestIsBilibiliURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{url: "https://www.bilibili.com/video/BV1xx411c7mD?p=2", want: true},
		{url: "https://m.bilibili.com/video/BV1xx", want: true},
		{url: "https://bangumi.bilibili.com/anime/12345", want: true},
		{url: "https://b23.tv/abc123", want: true},
		{url: "https://www.youtube.com/watch?v=xxx", want: false},
		{url: "https://ani.gamer.com.tw/", want: false},
		{url: "https://evil-bilibili.com/video/1", want: false},
		{url: "https://example.com/bilibili.com/video/1", want: false},
		{url: "", want: false},
	}
	for _, tc := range cases {
		if got := isBilibiliURL(tc.url); got != tc.want {
			t.Errorf("isBilibiliURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}

	if !isLikelyWebpageURL("https://b23.tv/abc123") {
		t.Fatal("expected b23.tv short URL to be treated as webpage URL")
	}
	if isLikelyWebpageURL("https://www.bilibili.com/live/playlist.m3u8") {
		t.Fatal("explicit .m3u8 on bilibili host must stay on native HLS path")
	}
}

func TestSanitizeBiliSessData(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "pure value", in: "  abc%2C123%2Cxyz  ", want: "abc%2C123%2Cxyz"},
		{name: "uppercase prefix", in: "SESSDATA=abc%2C123", want: "abc%2C123"},
		{name: "lowercase prefix", in: "sessdata=abc%2C123", want: "abc%2C123"},
		{name: "devtools cookie line", in: `SESSDATA="abc%2C123"; Path=/; HttpOnly`, want: "abc%2C123"},
		{name: "single quoted cookie", in: `SESSDATA='abc%2C123'; Domain=.bilibili.com`, want: "abc%2C123"},
		{name: "crlf nul tab injection", in: "SESSDATA=abc\r\n\t%2C123\x00; Path=/", want: "abc%2C123"},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: " \r\n\t ", want: ""},
		{name: "empty after prefix and semicolon", in: "SESSDATA= ; Path=/", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeBiliSessData(tc.in); got != tc.want {
				t.Fatalf("sanitizeBiliSessData(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCreateTempBiliCookieFile(t *testing.T) {
	for _, emptyIn := range []string{"", "   ", "SESSDATA= ; Path=/"} {
		path, err := createTempBiliCookieFile(emptyIn)
		if err != nil || path != "" {
			if path != "" {
				_ = os.Remove(path)
			}
			t.Fatalf("createTempBiliCookieFile(%q) = (%q, %v), want empty path and nil error", emptyIn, path, err)
		}
	}

	beforeUnix := time.Now().Unix()
	cookiePath, err := createTempBiliCookieFile(`SESSDATA="abc%2C123"; Path=/; HttpOnly`)
	if err != nil {
		t.Fatalf("createTempBiliCookieFile failed: %v", err)
	}
	if cookiePath == "" {
		t.Fatal("expected non-empty cookiePath")
	}
	defer os.Remove(cookiePath)

	raw, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("failed to read generated cookie file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in Netscape cookie file, got %d:\n%s", len(lines), string(raw))
	}
	if lines[0] != "# Netscape HTTP Cookie File" {
		t.Fatalf("unexpected Netscape header: %q", lines[0])
	}
	cols := strings.Split(lines[1], "\t")
	if len(cols) != 7 {
		t.Fatalf("expected 7 tab-separated Netscape cookie columns, got %d: %q", len(cols), lines[1])
	}
	if cols[0] != ".bilibili.com" || cols[1] != "TRUE" || cols[2] != "/" || cols[3] != "TRUE" || cols[5] != "SESSDATA" || cols[6] != "abc%2C123" {
		t.Fatalf("unexpected Netscape cookie columns: %v", cols)
	}
	expires, err := strconv.ParseInt(cols[4], 10, 64)
	if err != nil || expires <= beforeUnix {
		t.Fatalf("cookie expiration %q must be > %d (err=%v)", cols[4], beforeUnix, err)
	}
}

func TestBuildYtDlpArgsBilibiliAndIsolation(t *testing.T) {
	outDir := t.TempDir()
	cookiePath := filepath.Join(outDir, "bili_cookie.txt")

	hasFlagPair := func(args []string, flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}
	containsSubstring := func(args []string, substr string) bool {
		for _, a := range args {
			if strings.Contains(a, substr) {
				return true
			}
		}
		return false
	}

	// 1. Bilibili URL with RefererEnabled=true and cookie file
	biliArgs := buildYtDlpArgs(DownloadOptions{
		M3U8Url:        "https://www.bilibili.com/video/BV1xx411c7mD?p=2",
		OutputDir:      outDir,
		RefererEnabled: true,
		UserAgent:      "CustomUA/1.0",
		FFmpegPath:     filepath.Join(outDir, "ffmpeg.exe"),
	}, "", cookiePath)

	if slices.Contains(biliArgs, "--add-header") || slices.Contains(biliArgs, "--user-agent") || containsSubstring(biliArgs, "ani.gamer.com.tw") {
		t.Fatalf("Bilibili task must isolate Bahamut headers, got %v", biliArgs)
	}
	if !hasFlagPair(biliArgs, "--cookies", cookiePath) {
		t.Fatalf("expected --cookies %q in %v", cookiePath, biliArgs)
	}
	if !slices.Contains(biliArgs, "--no-playlist") {
		t.Fatalf("expected --no-playlist in %v", biliArgs)
	}
	if !hasFlagPair(biliArgs, "-S", "res,fps,acodec:aac,ext:mp4:m4a") {
		t.Fatalf("expected Bilibili format sort in %v", biliArgs)
	}

	// 2. YouTube URL with RefererEnabled=true
	ytArgs := buildYtDlpArgs(DownloadOptions{
		M3U8Url:        "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		OutputDir:      outDir,
		RefererEnabled: true,
		UserAgent:      "CustomUA/1.0",
	}, "", "")

	if slices.Contains(ytArgs, "--add-header") || slices.Contains(ytArgs, "--user-agent") || containsSubstring(ytArgs, "ani.gamer.com.tw") {
		t.Fatalf("YouTube task must isolate Bahamut headers, got %v", ytArgs)
	}
	if slices.Contains(ytArgs, "--cookies") || slices.Contains(ytArgs, "-S") {
		t.Fatalf("YouTube task must not include Bilibili --cookies or -S sort, got %v", ytArgs)
	}
	if !slices.Contains(ytArgs, "--no-playlist") {
		t.Fatalf("expected --no-playlist for YouTube in %v", ytArgs)
	}

	// 3. Non-third-party webpage URL with RefererEnabled=true keeps anti-leech headers
	for _, targetURL := range []string{
		"https://ani.gamer.com.tw/animeVideo.php?sn=12345",
		"https://custom-site.example.org/watch?v=99",
	} {
		args := buildYtDlpArgs(DownloadOptions{
			M3U8Url:        targetURL,
			OutputDir:      outDir,
			RefererEnabled: true,
			UserAgent:      "CustomUA/1.0",
		}, "", "")
		if !hasFlagPair(args, "--add-header", "Origin: "+AnimeOrigin) ||
			!hasFlagPair(args, "--add-header", "Referer: "+AnimeReferer) ||
			!hasFlagPair(args, "--user-agent", "CustomUA/1.0") {
			t.Fatalf("non-third-party URL %q must retain Bahamut anti-leech headers, got %v", targetURL, args)
		}
		if slices.Contains(args, "--cookies") || slices.Contains(args, "-S") {
			t.Fatalf("non-third-party URL %q must not include Bilibili flags, got %v", targetURL, args)
		}
	}
}

func TestRunExternalYtDlpBilibiliCookieCleanupAndNoSecretLeak(t *testing.T) {
	isoTmp := t.TempDir()
	t.Setenv("TEMP", isoTmp)
	t.Setenv("TMP", isoTmp)

	stubYt := filepath.Join(t.TempDir(), "yt-dlp.exe")
	if err := os.WriteFile(stubYt, []byte("not-a-valid-pe-executable"), 0755); err != nil {
		t.Fatal(err)
	}

	const rawSecret = "super_secret_sessdata_token_987"
	var logs []string
	_, err := runExternalYtDlp(context.Background(), DownloadOptions{
		M3U8Url:        "https://www.bilibili.com/video/BV1xx411c7mD?p=2",
		OutputDir:      t.TempDir(),
		RefererEnabled: true,
		BiliSessData:   fmt.Sprintf(`SESSDATA="%s"; Path=/; HttpOnly`, rawSecret),
		YtDlpPath:      stubYt,
	}, "", DownloadCallbacks{
		OnLog: func(tag, _, msg string) {
			logs = append(logs, tag+" "+msg)
		},
	})
	if err == nil {
		t.Fatal("expected error starting invalid stub yt-dlp.exe")
	}
	if strings.Contains(err.Error(), rawSecret) || strings.Contains(err.Error(), "SESSDATA=") {
		t.Fatalf("returned error leaked secret credential: %v", err)
	}

	var sawIsolationLog, sawCredLog bool
	for _, entry := range logs {
		if strings.Contains(entry, rawSecret) {
			t.Fatalf("console log leaked plaintext SESSDATA: %q", entry)
		}
		if strings.Contains(entry, "[路由]") && strings.Contains(entry, "已自動隔離巴哈防盜鏈標頭") {
			sawIsolationLog = true
		}
		if strings.Contains(entry, "[憑證]") && strings.Contains(entry, "臨時憑證") {
			sawCredLog = true
		}
	}
	if !sawIsolationLog {
		t.Fatalf("expected third-party header isolation log, got %v", logs)
	}
	if !sawCredLog {
		t.Fatalf("expected masked Bilibili credential log, got %v", logs)
	}

	leftovers, globErr := filepath.Glob(filepath.Join(isoTmp, "anidl_bili_cookie_*.txt"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary Bilibili cookie file was not cleaned up: %v", leftovers)
	}
}
