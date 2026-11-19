package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	AnimeOrigin        = "https://ani.gamer.com.tw"
	AnimeReferer       = "https://ani.gamer.com.tw/"
	AnimeUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
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
)

// SanitizeFilename cleans illegal Windows path characters and removes duplicate .mp4 suffix.
func SanitizeFilename(name string) string {
	clean := strings.TrimSpace(name)
	if strings.HasSuffix(strings.ToLower(clean), ".mp4") {
		clean = clean[:len(clean)-4]
	}
	clean = invalidFilenameRegex.ReplaceAllString(clean, "_")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		clean = fmt.Sprintf("巴哈動畫瘋_%s", time.Now().Format("20060102_150405"))
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

func RunDownload(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
	opt.M3U8Url = strings.TrimSpace(opt.M3U8Url)
	if opt.M3U8Url == "" {
		return "", fmt.Errorf("請輸入有效的 m3u8 串聯鏈接/網址")
	}
	if !strings.HasPrefix(opt.M3U8Url, "http://") && !strings.HasPrefix(opt.M3U8Url, "https://") {
		return "", fmt.Errorf("m3u8 網址格式錯誤，必須以 http:// 或 https:// 開頭")
	}

	opt.Title = SanitizeFilename(opt.Title)
	if strings.TrimSpace(opt.OutputDir) == "" {
		opt.OutputDir = GetDefaultDownloadDir()
	}
	if err := os.MkdirAll(opt.OutputDir, 0755); err != nil {
		return "", fmt.Errorf("建立儲存目錄失敗: %w", err)
	}

	// Enforce 1-thread concurrency
	opt.Concurrency = DefaultConcurrency

	ua := strings.TrimSpace(opt.UserAgent)
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
	if opt.RefererEnabled {
		if ja3 != "" {
			cb.OnLog("[指紋]", "#38bdf8", fmt.Sprintf("JA3=%s", ja3))
		}
		if akamai != "" {
			cb.OnLog("[指紋]", "#38bdf8", fmt.Sprintf("Akamai=%s", akamai))
		}
	}

	baseEnv := append(os.Environ(),
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8:replace",
	)

	if opt.Engine == "ffmpeg" {
		args := make([]string, 0, 16)
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
		cmd = exec.Command(opt.FFmpegPath, args...)
		cmd.Env = baseEnv
	} else {
		outTemplate := filepath.Join(opt.OutputDir, opt.Title+".%(ext)s")
		ffmpegDir := filepath.Dir(opt.FFmpegPath)

		args := make([]string, 0, 30)
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
			"-N", strconv.Itoa(opt.Concurrency),
			"--concurrent-fragments", strconv.Itoa(opt.Concurrency),
			"--no-part",
			"--no-mtime",
			"--newline",
			"--merge-output-format", "mp4",
			"-o", outTemplate,
			opt.M3U8Url,
		)
		cmd = exec.Command(opt.YtDlpPath, args...)
		env := baseEnv
		if opt.RefererEnabled {
			if ja3 != "" {
				env = append(env, "YT_DLP_JA3="+ja3)
			}
			if akamai != "" {
				env = append(env, "YT_DLP_AKAMAI="+akamai)
			}
		}
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

	// Kill entire Windows process tree (yt-dlp + child ffmpeg) if cancelled
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				killProcessTree(cmd.Process.Pid)
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

	if ctx.Err() == context.Canceled {
		cb.OnLog("[警告]", "#fbbf24", "使用者手動中止任務。")
		return "", context.Canceled
	}

	if waitErr != nil {
		cb.OnLog("[錯誤]", "#f87171", fmt.Sprintf("下載程序異常結束: %v", waitErr))
		return "", fmt.Errorf("下載失敗，請檢查 m3u8 網址或 Console 日誌: %w", waitErr)
	}

	cb.OnLog("[完成]", "#34d399", fmt.Sprintf("任務完成，已輸出檔案至 %s", outFilePath))
	return outFilePath, nil
}

func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	tk := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
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
