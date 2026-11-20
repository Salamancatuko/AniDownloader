//go:build windows

package main

import (
	"bytes"
	"context"
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
	ytdlpPctRegex        = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)
	ytdlpSpeedRegex      = regexp.MustCompile(`at\s+([0-9.]+\s*[KMGTP]?i?B/s)`)
	ytdlpFragRegex       = regexp.MustCompile(`\(frag\s+(\d+)/(\d+)\)`)
	ffmpegDurRegex       = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)
	ffmpegTimeRegex      = regexp.MustCompile(`time=(\d+):(\d+):(\d+(?:\.\d+)?)`)
	ffmpegSpeedRegex     = regexp.MustCompile(`speed=\s*([0-9.]+x)`)

	windowsReservedDeviceNames = map[string]struct{}{
		"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
		"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
		"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	}
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
	Engine         string // "ytdlp" or "ffmpeg"
	RefererEnabled bool
	UserAgent      string
	JA3            string
	Akamai         string
	Concurrency    int
	YtDlpPath      string
	FFmpegPath     string
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

func buildDownloadEnv(baseEnv []string) []string {
	env := make([]string, 0, len(baseEnv)+2)
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "YT_DLP_JA3=") || strings.HasPrefix(kv, "YT_DLP_AKAMAI=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8:replace",
	)
	return env
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

func buildYtDlpOutTemplate(outputDir, title string) string {
	escapedDir := strings.ReplaceAll(outputDir, "%", "%%")
	escapedTitle := strings.ReplaceAll(title, "%", "%%")
	return filepath.Join(escapedDir, escapedTitle+".%(ext)s")
}

func buildYtDlpArgs(opt DownloadOptions, ua, ja3, akamai, outTemplate string) []string {
	ua = sanitizeHeaderValue(ua)
	if ua == "" {
		ua = DefaultUserAgent
	}
	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	ffmpegDir := filepath.Dir(opt.FFmpegPath)

	args := make([]string, 0, 26)
	if opt.RefererEnabled {
		args = append(args,
			"--add-header", fmt.Sprintf("Origin: %s", AnimeOrigin),
			"--add-header", fmt.Sprintf("Referer: %s", AnimeReferer),
			"--user-agent", ua,
		)
		if ja3 != "" || akamai != "" {
			args = append(args, "--impersonate", "chrome")
		}
	}
	args = append(args,
		"--encoding", "utf-8",
		"--ffmpeg-location", ffmpegDir,
		"-N", strconv.Itoa(concurrency),
		"--no-part",
		"--no-mtime",
		"--force-overwrites",
		"--newline",
		"--remux-video", "mp4",
		"--merge-output-format", "mp4",
		"-o", outTemplate,
		opt.M3U8Url,
	)
	return args
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

	opt.Title = SanitizeFilename(opt.Title)
	if strings.TrimSpace(opt.OutputDir) == "" {
		opt.OutputDir = GetDefaultDownloadDir()
	}
	if err := os.MkdirAll(opt.OutputDir, 0755); err != nil {
		return "", fmt.Errorf("建立儲存目錄失敗: %w", err)
	}

	// Enforce 1-thread concurrency
	opt.Concurrency = DefaultConcurrency

	ua := sanitizeHeaderValue(opt.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	ja3 := strings.TrimSpace(opt.JA3)
	akamai := strings.TrimSpace(opt.Akamai)

	outFilePath := filepath.Join(opt.OutputDir, opt.Title+".mp4")

	var cmd *exec.Cmd
	var headerDesc string
	if opt.RefererEnabled {
		headerDesc = fmt.Sprintf(`Origin/Referer ON | UA="%s"`, ua)
	} else {
		headerDesc = "--no-headers"
	}

	engineUpper := "YT-DLP"
	accentColor := "#00B4DF"
	if opt.Engine == "ffmpeg" {
		engineUpper = "FFMPEG"
		accentColor = "#F4962E"
	}

	cb.OnLog("[提示]", "#38bdf8", fmt.Sprintf("啟動下載引擎 [%s] | threads=%d | %s", engineUpper, opt.Concurrency, headerDesc))
	if opt.Engine != "ffmpeg" && opt.RefererEnabled && (ja3 != "" || akamai != "") {
		cb.OnLog("[指紋]", "#38bdf8", "標準版 YT-DLP 已啟用 --impersonate chrome（自定義 JA3/Akamai 保留供後續原生引擎使用）")
	}

	env := buildDownloadEnv(os.Environ())

	if opt.Engine == "ffmpeg" {
		args := buildFFmpegArgs(opt, ua, outFilePath)
		cmd = exec.Command(opt.FFmpegPath, args...)
		cmd.Env = env
	} else {
		outTemplate := buildYtDlpOutTemplate(opt.OutputDir, opt.Title)
		args := buildYtDlpArgs(opt, ua, ja3, akamai, outTemplate)
		cmd = exec.Command(opt.YtDlpPath, args...)
		cmd.Env = env
	}

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
		return "", fmt.Errorf("啟動 %s 失敗: %w", engineUpper, err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	// Kill entire Windows process tree (yt-dlp + child ffmpeg) if cancelled
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

		if opt.Engine == "ffmpeg" {
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
		} else {
			// yt-dlp output parsing
			if strings.Contains(line, "[Merger]") || strings.Contains(line, "[FixupM3u8]") || strings.Contains(line, "[VideoRemuxer]") {
				cb.OnProgress(ProgressUpdate{
					Engine:  "ytdlp",
					Percent: 99.0,
					Merging: true,
				})
				cb.OnLog("[封裝]", "#38bdf8", line)
				return
			}
			if strings.HasPrefix(line, "[download]") {
				pct := -1.0
				if m := ytdlpPctRegex.FindStringSubmatch(line); len(m) > 1 {
					if v, err := strconv.ParseFloat(m[1], 64); err == nil {
						pct = v
					}
				} else if m := ytdlpFragRegex.FindStringSubmatch(line); len(m) == 3 {
					cur, _ := strconv.ParseFloat(m[1], 64)
					tot, _ := strconv.ParseFloat(m[2], 64)
					if tot > 0 {
						pct = (cur / tot) * 100.0
					}
				}
				speed := ""
				if sm := ytdlpSpeedRegex.FindStringSubmatch(line); len(sm) > 1 {
					speed = strings.TrimSpace(sm[1])
				}
				if pct >= 0 {
					cb.OnProgress(ProgressUpdate{
						Engine:  "ytdlp",
						Percent: pct,
						Speed:   speed,
					})
					cb.OnLog("[進度]", accentColor, line)
					return
				}
			}
			if strings.Contains(line, "ERROR:") {
				cb.OnLog("[錯誤]", "#f87171", line)
				return
			}
			cb.OnLog("[YT-DLP]", accentColor, line)
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

	if ctx.Err() == context.Canceled {
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
