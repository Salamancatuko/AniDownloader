//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	AnimeOrigin        = "https://ani.gamer.com.tw"
	AnimeReferer       = "https://ani.gamer.com.tw/"
	DefaultConcurrency = 1 // Enforced 1-thread segment concurrency per specification
)

var (
	invalidFilenameRegex = regexp.MustCompile(`[\\/:*?"<>|]`)
	ffmpegDurRegex       = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)
	ffmpegTimeRegex      = regexp.MustCompile(`time=(\d+):(\d+):(\d+(?:\.\d+)?)`)
	ffmpegSpeedRegex     = regexp.MustCompile(`speed=\s*([0-9.]+x)`)

	windowsReservedDeviceNames = map[string]struct{}{
		"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
		"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
		"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	}

	ytdlpPctRegex   = regexp.MustCompile(`\[download\]\s+(\d+(?:\.\d+)?)%`)
	ytdlpSpeedRegex = regexp.MustCompile(`at\s+~?\s*([0-9.]+\s*[KMGTP]?i?B/s)`)
	ytdlpFragRegex  = regexp.MustCompile(`\(frag\s+(\d+)/(\d+)\)`)

	knownWebpageDomains = []string{
		"youtube.com",
		"youtu.be",
		"music.youtube.com",
		"bilibili.com",
		"b23.tv",
		"twitch.tv",
		"nicovideo.jp",
		"twitter.com",
		"x.com",
		"tiktok.com",
		"vimeo.com",
		"dailymotion.com",
		"ani.gamer.com.tw",
	}

	runExternalYtDlpFn = runExternalYtDlp
)

func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sanitizeHeaderValue strips CR, LF, NUL and surrounding whitespace to prevent HTTP header CRLF injection.
func sanitizeHeaderValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\r' || r == '\n' || r == '\x00' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// validateDownloadURL checks that the input URL is non-empty, contains no control characters,
// and parses as an http or https URL with a non-empty host.
func validateDownloadURL(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", fmt.Errorf("m3u8 網址包含非法控制字元")
	}
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", fmt.Errorf("請輸入有效的 m3u8 串聯鏈接/網址")
	}
	for _, r := range u {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("m3u8 網址包含非法控制字元")
		}
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("m3u8 網址格式錯誤，必須為有效的 http:// 或 https:// 網址")
	}
	return u, nil
}

// SanitizeFilename cleans illegal Windows path characters, control characters,
// duplicate .mp4 suffixes, trailing dots/spaces, overlong names, and reserved Windows device names.
func SanitizeFilename(name string) string {
	clean := stripControlChars(name)
	clean = strings.TrimSpace(clean)
	for strings.HasSuffix(strings.ToLower(clean), ".mp4") {
		clean = strings.TrimSpace(clean[:len(clean)-4])
	}
	clean = invalidFilenameRegex.ReplaceAllString(clean, "_")
	clean = strings.TrimLeft(clean, " \t")
	clean = strings.TrimRight(clean, ". \t")

	runes := []rune(clean)
	if len(runes) > 180 {
		clean = string(runes[:180])
		clean = strings.TrimRight(clean, ". \t")
	}

	if clean == "" {
		clean = fmt.Sprintf("巴哈動畫瘋_%s", time.Now().Format("20060102_150405"))
	}

	base := strings.SplitN(clean, ".", 2)[0]
	base = strings.ToUpper(strings.TrimRight(base, " \t"))
	if _, reserved := windowsReservedDeviceNames[base]; reserved {
		clean = "_" + clean
	}
	return clean
}

type DownloadOptions struct {
	M3U8Url        string
	Title          string
	OutputDir      string
	Engine         string // "hls" or "ffmpeg"
	RefererEnabled bool
	UserAgent      string
	JA3            string
	Akamai         string
	Concurrency    int
	FFmpegPath     string
	YtDlpPath      string
}

type ProgressUpdate struct {
	Engine  string  `json:"engine"`
	Percent float64 `json:"percent"`
	Speed   string  `json:"speed,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	Merging bool    `json:"merging,omitempty"`
}

type DownloadCallbacks struct {
	OnLog      func(tag, color, msg string)
	OnProgress func(p ProgressUpdate)
}

func buildFFmpegArgs(opt DownloadOptions, ua, outFilePath string) []string {
	ua = sanitizeHeaderValue(ua)
	if ua == "" {
		ua = DefaultUserAgent
	}
	args := make([]string, 0, 12)
	if opt.RefererEnabled {
		headers := fmt.Sprintf("Origin: %s\r\nReferer: %s\r\nUser-Agent: %s\r\n", AnimeOrigin, AnimeReferer, ua)
		args = append(args, "-headers", headers)
	}
	args = append(args,
		"-i", opt.M3U8Url,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-y",
		outFilePath,
	)
	return args
}

// isLikelyWebpageURL returns true if rawURL targets a known video platform webpage
// rather than an explicit .m3u8/.m3u playlist path.
func isLikelyWebpageURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil || parsed.Host == "" {
		return false
	}
	pathLower := strings.ToLower(parsed.Path)
	if strings.Contains(pathLower, ".m3u8") || strings.Contains(pathLower, ".m3u") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range knownWebpageDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func RunDownload(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
	if cb.OnLog == nil {
		cb.OnLog = func(string, string, string) {}
	}
	if cb.OnProgress == nil {
		cb.OnProgress = func(ProgressUpdate) {}
	}

	cleanURL, err := validateDownloadURL(opt.M3U8Url)
	if err != nil {
		return "", err
	}
	opt.M3U8Url = cleanURL

	rawTitle := strings.TrimSpace(opt.Title)
	if strings.TrimSpace(opt.OutputDir) == "" {
		opt.OutputDir = GetDefaultDownloadDir()
	}
	if err := os.MkdirAll(opt.OutputDir, 0755); err != nil {
		return "", fmt.Errorf("建立儲存目錄失敗: %w", err)
	}

	// Enforce 1-thread concurrency
	opt.Concurrency = DefaultConcurrency

	// Normalize legacy "ytdlp" or empty engine to "hls"
	if opt.Engine != "ffmpeg" {
		opt.Engine = "hls"
	}

	if opt.Engine == "hls" {
		if isLikelyWebpageURL(opt.M3U8Url) {
			cb.OnLog("[路由]", "#38bdf8", "偵測到影片網頁網址，自動切換外部 YT-DLP 引擎解析下載...")
			return runExternalYtDlpFn(ctx, opt, rawTitle, cb)
		}
		if hlsRunNativeDownloadFn == nil {
			return "", fmt.Errorf("原生 HLS 引擎尚未註冊")
		}
		nativeOpt := opt
		nativeOpt.Title = SanitizeFilename(rawTitle)
		outPath, err := hlsRunNativeDownloadFn(ctx, nativeOpt, cb)
		if err == nil {
			return outPath, nil
		}
		if errors.Is(err, errNonM3U8Webpage) {
			cb.OnLog("[路由]", "#38bdf8", "偵測到目標回應為 HTML 網頁而非 m3u8 串流，自動改由外部 YT-DLP 處理...")
			return runExternalYtDlpFn(ctx, opt, rawTitle, cb)
		}
		return "", err
	}

	opt.Title = SanitizeFilename(rawTitle)

	if strings.TrimSpace(opt.FFmpegPath) == "" {
		return "", fmt.Errorf("找不到本機 FFMPEG 執行檔路徑")
	}

	ua := sanitizeHeaderValue(opt.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}

	outFilePath := filepath.Join(opt.OutputDir, opt.Title+".mp4")

	headerDesc := "--no-headers"
	if opt.RefererEnabled {
		headerDesc = fmt.Sprintf(`Origin/Referer ON | UA="%s"`, ua)
	}

	const accentColor = "#F4962E"
	cb.OnLog("[提示]", "#38bdf8", fmt.Sprintf("啟動下載引擎 [FFMPEG] | threads=%d | %s", opt.Concurrency, headerDesc))

	args := buildFFmpegArgs(opt, ua, outFilePath)
	cmd := exec.Command(opt.FFmpegPath, args...)

	// Hide console window on Windows
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("建立 STDOUT 管道失敗: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("建立 STDERR 管道失敗: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("啟動 FFMPEG 失敗: %w", err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	// Kill entire Windows process tree if cancelled
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			if pid > 0 {
				killProcessTree(pid)
			}
		case <-stopWatch:
		}
	}()

	var (
		totalDurationSec float64
		lastProgressAt   time.Time
		mu               sync.Mutex
	)

	handleLine := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		if m := ffmpegDurRegex.FindStringSubmatch(line); len(m) == 4 {
			totalDurationSec = parseHMS(m[1], m[2], m[3])
		}
		if m := ffmpegTimeRegex.FindStringSubmatch(line); len(m) == 4 {
			curSec := parseHMS(m[1], m[2], m[3])
			speed := ""
			if sm := ffmpegSpeedRegex.FindStringSubmatch(line); len(sm) > 1 {
				speed = sm[1]
			}
			pct := -1.0
			detail := fmt.Sprintf("time=%s:%s:%s", m[1], m[2], m[3])
			if totalDurationSec > 0 {
				pct = (curSec / totalDurationSec) * 100.0
				if pct > 99.5 {
					pct = 99.5
				}
			}
			shouldEmit := time.Since(lastProgressAt) >= 250*time.Millisecond
			if shouldEmit {
				lastProgressAt = time.Now()
				cb.OnProgress(ProgressUpdate{
					Engine:  "ffmpeg",
					Percent: pct,
					Speed:   speed,
					Detail:  detail,
				})
			}
			// Throttle repetitive ffmpeg frame= / size= lines in console
			if strings.HasPrefix(line, "frame=") || strings.HasPrefix(line, "size=") {
				if shouldEmit {
					cb.OnLog("[進度]", accentColor, line)
				}
				return
			}
		}
		cb.OnLog("[FFMPEG]", accentColor, line)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanLinesAndReturns(stdout, handleLine)
	}()
	go func() {
		defer wg.Done()
		scanLinesAndReturns(stderr, handleLine)
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(stopWatch)
	<-watchDone

	if errors.Is(ctx.Err(), context.Canceled) {
		cb.OnLog("[警告]", "#fbbf24", "使用者手動中止任務。")
		return "", context.Canceled
	}

	if waitErr != nil {
		cb.OnLog("[錯誤]", "#f87171", fmt.Sprintf("下載程序異常結束: %v", waitErr))
		return "", fmt.Errorf("下載失敗，請檢查 m3u8 網址或 Console 日誌: %w", waitErr)
	}

	if !fileExistsNonEmpty(outFilePath) {
		cb.OnLog("[錯誤]", "#f87171", fmt.Sprintf("下載完成但找不到輸出檔案: %s", outFilePath))
		return "", fmt.Errorf("下載完成但未產生有效的 MP4 輸出檔案: %s", outFilePath)
	}

	cb.OnLog("[完成]", "#34d399", fmt.Sprintf("任務完成，已輸出檔案至 %s", outFilePath))
	return outFilePath, nil
}

func buildYtDlpOutTemplate(outputDir, rawTitle string) string {
	if strings.TrimSpace(rawTitle) != "" {
		return filepath.Join(outputDir, SanitizeFilename(rawTitle)+".%(ext)s")
	}
	return filepath.Join(outputDir, "%(title).120B.%(ext)s")
}

func buildYtDlpArgs(opt DownloadOptions, rawTitle string) []string {
	ua := sanitizeHeaderValue(opt.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}

	args := make([]string, 0, 26)
	if opt.RefererEnabled {
		args = append(args,
			"--add-header", "Origin: "+AnimeOrigin,
			"--add-header", "Referer: "+AnimeReferer,
			"--user-agent", ua,
		)
	}

	args = append(args, "--encoding", "utf-8")
	if strings.TrimSpace(opt.FFmpegPath) != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(opt.FFmpegPath))
	}
	args = append(args,
		"-N", strconv.Itoa(DefaultConcurrency),
		"--no-quiet",
		"--progress",
		"--no-part",
		"--no-mtime",
		"--newline",
		"--windows-filenames",
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"--print", "after_move:__ANIDL_OUTFILE__:%(filepath)s",
		"-o", buildYtDlpOutTemplate(opt.OutputDir, rawTitle),
		opt.M3U8Url,
	)
	return args
}

func buildYtDlpEnv() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		upperKey := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		switch upperKey {
		case "YT_DLP_JA3", "YT_DLP_AKAMAI", "PYTHONUTF8", "PYTHONIOENCODING", "PYTHONUNBUFFERED":
			continue
		default:
			env = append(env, entry)
		}
	}
	env = append(env, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8:replace", "PYTHONUNBUFFERED=1")
	return env
}

func parseYtDlpOutputLine(line string, lastProgressAt, lastLogAt *time.Time, cb DownloadCallbacks) (outFile string, isOutFile bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}

	emitProgress := func(p ProgressUpdate) {
		if cb.OnProgress != nil {
			cb.OnProgress(p)
		}
	}
	emitLog := func(tag, color, msg string) {
		if cb.OnLog != nil {
			cb.OnLog(tag, color, msg)
		}
	}

	const outPrefix = "__ANIDL_OUTFILE__:"
	if idx := strings.Index(line, outPrefix); idx >= 0 {
		return strings.TrimSpace(line[idx+len(outPrefix):]), true
	}

	if strings.Contains(line, "[Merger]") ||
		strings.Contains(line, "[VideoRemuxer]") ||
		strings.Contains(line, "[FixupM3u8]") ||
		strings.Contains(line, "[FixupM4a]") ||
		strings.Contains(line, "[Fixup") {
		emitProgress(ProgressUpdate{
			Engine:  "hls",
			Percent: 99.0,
			Merging: true,
		})
		emitLog("[封裝]", "#38bdf8", line)
		return "", false
	}

	if strings.HasPrefix(line, "[download]") {
		fm := ytdlpFragRegex.FindStringSubmatch(line)
		detail := ""
		if len(fm) == 3 {
			detail = fmt.Sprintf("frag %s/%s", fm[1], fm[2])
		}

		var (
			pct       float64
			parsedPct bool
		)
		if pm := ytdlpPctRegex.FindStringSubmatch(line); len(pm) > 1 {
			if p, err := strconv.ParseFloat(pm[1], 64); err == nil {
				pct = p
				parsedPct = true
			}
		} else if len(fm) == 3 {
			cur, errCur := strconv.ParseFloat(fm[1], 64)
			tot, errTot := strconv.ParseFloat(fm[2], 64)
			if errCur == nil && errTot == nil && tot > 0 {
				pct = (cur / tot) * 100.0
				parsedPct = true
			}
		}

		if parsedPct {
			if pct < 0 {
				pct = 0
			} else if pct > 100 {
				pct = 100
			}
			speed := ""
			if !strings.Contains(line, "Unknown B/s") {
				if sm := ytdlpSpeedRegex.FindStringSubmatch(line); len(sm) > 1 {
					speed = strings.Join(strings.Fields(sm[1]), "")
				}
			}

			now := time.Now()
			shouldEmitProg := pct >= 100.0 || lastProgressAt == nil || lastProgressAt.IsZero() || now.Sub(*lastProgressAt) >= 250*time.Millisecond
			if shouldEmitProg {
				if lastProgressAt != nil {
					*lastProgressAt = now
				}
				emitProgress(ProgressUpdate{
					Engine:  "hls",
					Percent: pct,
					Speed:   speed,
					Detail:  detail,
				})
			}

			shouldEmitLog := pct >= 100.0 || lastLogAt == nil || lastLogAt.IsZero() || now.Sub(*lastLogAt) >= time.Second
			if shouldEmitLog {
				if lastLogAt != nil {
					*lastLogAt = now
				}
				emitLog("[進度]", "#00B4DF", line)
			}
			return "", false
		}
	}

	if strings.Contains(line, "ERROR:") {
		emitLog("[錯誤]", "#f87171", line)
		return "", false
	}
	if strings.Contains(line, "WARNING:") {
		emitLog("[警告]", "#fbbf24", line)
		return "", false
	}

	emitLog("[YT-DLP]", "#00B4DF", line)
	return "", false
}

func runExternalYtDlp(ctx context.Context, opt DownloadOptions, rawTitle string, cb DownloadCallbacks) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	ytPath := strings.TrimSpace(opt.YtDlpPath)
	if ytPath == "" || !fileExistsNonEmpty(ytPath) {
		foundPath, _ := findOptionalYtDlpFn()
		ytPath = foundPath
	}
	if ytPath == "" {
		cb.OnLog("[警告]", "#fbbf24", "未偵測到外部 yt-dlp.exe，無法處理一般影片網頁連結。")
		return "", errors.New("此連結為影片網頁（如 YouTube），需搭配外部 yt-dlp.exe 解析；請至 Settings 點擊「安裝 / 更新 YT-DLP」，或將 yt-dlp.exe 放至程式目錄。")
	}

	headerDesc := "--no-headers"
	if opt.RefererEnabled {
		ua := sanitizeHeaderValue(opt.UserAgent)
		if ua == "" {
			ua = DefaultUserAgent
		}
		headerDesc = fmt.Sprintf(`Origin/Referer ON | UA="%s"`, ua)
	}
	cb.OnLog("[提示]", "#38bdf8", fmt.Sprintf("啟動外部混合引擎 [YT-DLP] | threads=%d | %s", DefaultConcurrency, headerDesc))

	args := buildYtDlpArgs(opt, rawTitle)
	cmd := exec.Command(ytPath, args...)
	cmd.Env = buildYtDlpEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("建立 YT-DLP STDOUT 管道失敗: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("建立 YT-DLP STDERR 管道失敗: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("啟動外部 YT-DLP 失敗: %w", err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			if pid > 0 {
				killProcessTree(pid)
			}
		case <-stopWatch:
		}
	}()

	var (
		mu             sync.Mutex
		lastProgressAt time.Time
		lastLogAt      time.Time
		actualOutPath  string
		lastErrLine    string
	)

	handleLine := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		outPath, isOut := parseYtDlpOutputLine(line, &lastProgressAt, &lastLogAt, cb)
		if isOut {
			if outPath != "" {
				actualOutPath = outPath
			}
			return
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lastErrLine = trimmed
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanLinesAndReturns(stdout, handleLine)
	}()
	go func() {
		defer wg.Done()
		scanLinesAndReturns(stderr, handleLine)
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(stopWatch)
	<-watchDone

	if errors.Is(ctx.Err(), context.Canceled) {
		cb.OnLog("[警告]", "#fbbf24", "使用者手動中止任務。")
		return "", context.Canceled
	}

	mu.Lock()
	outFile := actualOutPath
	detailErr := lastErrLine
	mu.Unlock()

	if waitErr != nil {
		cb.OnLog("[錯誤]", "#f87171", fmt.Sprintf("外部 YT-DLP 執行異常結束: %v", waitErr))
		if detailErr != "" {
			return "", fmt.Errorf("外部 YT-DLP 下載失敗 (%s): %w", detailErr, waitErr)
		}
		return "", fmt.Errorf("外部 YT-DLP 下載失敗，請檢查連結或 Console 日誌: %w", waitErr)
	}

	if outFile == "" && rawTitle != "" {
		cand := filepath.Join(opt.OutputDir, SanitizeFilename(rawTitle)+".mp4")
		if fileExistsNonEmpty(cand) {
			outFile = cand
		}
	}

	if outFile == "" || !fileExistsNonEmpty(outFile) {
		cb.OnLog("[錯誤]", "#f87171", "外部 YT-DLP 完成但找不到有效的 MP4 輸出檔案")
		return "", errors.New("外部 YT-DLP 下載完成但未產生有效的 MP4 輸出檔案")
	}

	cb.OnProgress(ProgressUpdate{Engine: "hls", Percent: 100, Detail: "封裝完成"})
	cb.OnLog("[完成]", "#34d399", fmt.Sprintf("任務完成，已輸出檔案至 %s", outFile))
	return outFile, nil
}

func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	tk := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = tk.Run()
}

func parseHMS(hStr, mStr, sStr string) float64 {
	h, _ := strconv.ParseFloat(hStr, 64)
	m, _ := strconv.ParseFloat(mStr, 64)
	s, _ := strconv.ParseFloat(sStr, 64)
	return h*3600 + m*60 + s
}

func scanLinesAndReturns(r io.Reader, handleLine func(string)) {
	buf := make([]byte, 4096)
	var lineBuf bytes.Buffer

	for {
		n, err := r.Read(buf)
		if n > 0 {
			for i := 0; i < n; i++ {
				b := buf[i]
				if b == '\r' || b == '\n' {
					if lineBuf.Len() > 0 {
						handleLine(decodeConsoleBytes(lineBuf.Bytes()))
						lineBuf.Reset()
					}
				} else {
					lineBuf.WriteByte(b)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if lineBuf.Len() > 0 {
		handleLine(decodeConsoleBytes(lineBuf.Bytes()))
	}
}

// GetDefaultDownloadDir returns the Windows user's Downloads folder.
func GetDefaultDownloadDir() string {
	userProfile := os.Getenv("USERPROFILE")
	if userProfile != "" {
		downloads := filepath.Join(userProfile, "Downloads")
		if info, err := os.Stat(downloads); err == nil && info.IsDir() {
			return downloads
		}
	}
	exePath, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(exePath), "Downloads")
	}
	return "."
}
