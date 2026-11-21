//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errNonM3U8Webpage = errors.New("回應內容為網頁而非直接 m3u8 串流清單")

func looksLikeHTMLWebpage(body []byte) bool {
	trimmed := bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) > 512 {
		trimmed = trimmed[:512]
	}
	lower := strings.ToLower(string(trimmed))
	return strings.Contains(lower, "<!doctype html") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<head") ||
		strings.Contains(lower, "<meta")
}

func init() {
	hlsRunNativeDownloadFn = RunNativeHLSDownload
}

type hlsRunnerDeps struct {
	newClient     func(ctx context.Context, cfg hlsClientConfig) (hlsHTTPClient, error)
	getWithRetry  func(ctx context.Context, client hlsHTTPClient, rawURL string, extraHeaders map[string]string, maxRetries int, onLog func(tag, color, msg string)) (*hlsResponse, error)
	parseM3U8     func(content string, baseURL string) (*hlsPlaylist, error)
	selectBest    func(variants []hlsVariant) (*hlsVariant, error)
	decryptAES128 func(ciphertext, key, iv []byte) ([]byte, error)
	buildSeqIV    func(seq uint64) []byte
	remuxFn       func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error
}

type downloadedTrackInput struct {
	inputPath string
	isConcat  bool
	isFMP4    bool
}

type speedSample struct {
	start time.Time
	end   time.Time
	bytes int64
}

type speedTracker struct {
	window  time.Duration
	samples []speedSample
}

func newSpeedTracker(window time.Duration) *speedTracker {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &speedTracker{window: window}
}

func (t *speedTracker) prune(now time.Time) {
	cutoff := now.Add(-t.window)
	keep := 0
	for keep < len(t.samples) && t.samples[keep].end.Before(cutoff) {
		keep++
	}
	if keep > 0 {
		copy(t.samples, t.samples[keep:])
		t.samples = t.samples[:len(t.samples)-keep]
	}
}

func (t *speedTracker) AddSpan(n int64, start, end time.Time) {
	if end.Before(start) {
		start = end
	}
	t.prune(end)
	if n > 0 {
		t.samples = append(t.samples, speedSample{start: start, end: end, bytes: n})
	}
}

func (t *speedTracker) Add(n int64, now time.Time) {
	t.AddSpan(n, now, now)
}

func (t *speedTracker) SpeedBps(now time.Time) float64 {
	t.prune(now)
	if len(t.samples) == 0 {
		return 0
	}

	var total int64
	earliestStart := t.samples[0].start
	for _, s := range t.samples {
		total += s.bytes
		if s.start.Before(earliestStart) {
			earliestStart = s.start
		}
	}
	if total <= 0 {
		return 0
	}

	elapsed := now.Sub(earliestStart)
	if elapsed < 50*time.Millisecond {
		elapsed = 50 * time.Millisecond
	}

	bps := float64(total) / elapsed.Seconds()
	if math.IsNaN(bps) || math.IsInf(bps, 0) || bps < 0 {
		return 0
	}
	return bps
}

func formatByteSpeed(bps float64) string {
	if math.IsNaN(bps) || math.IsInf(bps, 0) || bps <= 0 {
		return "0.00 B/s"
	}
	switch {
	case bps >= 1024*1024:
		return fmt.Sprintf("%.2f MB/s", bps/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.2f KB/s", bps/1024)
	default:
		return fmt.Sprintf("%.2f B/s", bps)
	}
}

func RunNativeHLSDownload(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
	deps := hlsRunnerDeps{
		newClient:     hlsNewClientHook,
		getWithRetry:  hlsGetWithRetryHook,
		parseM3U8:     hlsParseM3U8Hook,
		selectBest:    hlsSelectBestHook,
		decryptAES128: hlsDecryptAES128Hook,
		buildSeqIV:    hlsBuildSeqIVHook,
		remuxFn:       runLocalFFmpegRemux,
	}
	return runNativeHLSDownloadWithDeps(ctx, opt, cb, deps)
}

func validateRunnerDeps(deps hlsRunnerDeps) error {
	switch {
	case deps.newClient == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊 HTTP 客戶端模組 (hlsNewClientHook)")
	case deps.getWithRetry == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊重試請求模組 (hlsGetWithRetryHook)")
	case deps.parseM3U8 == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊清單解析模組 (hlsParseM3U8Hook)")
	case deps.selectBest == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊變體選軌模組 (hlsSelectBestHook)")
	case deps.decryptAES128 == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊 AES-128 解密模組 (hlsDecryptAES128Hook)")
	case deps.buildSeqIV == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊 IV 生成模組 (hlsBuildSeqIVHook)")
	case deps.remuxFn == nil:
		return fmt.Errorf("原生 HLS 引擎尚未註冊 FFmpeg 封裝模組")
	default:
		return nil
	}
}

func runNativeHLSDownloadWithDeps(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks, deps hlsRunnerDeps) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cb.OnLog == nil {
		cb.OnLog = func(string, string, string) {}
	}
	if cb.OnProgress == nil {
		cb.OnProgress = func(ProgressUpdate) {}
	}

	if err := validateRunnerDeps(deps); err != nil {
		return "", err
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

	ua := sanitizeHeaderValue(opt.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}

	headerDesc := "--no-headers"
	if opt.RefererEnabled {
		headerDesc = fmt.Sprintf(`Origin/Referer ON | UA="%s"`, ua)
	}
	cb.OnLog("[提示]", "#38bdf8", fmt.Sprintf("啟動下載引擎 [HLS-CORE] | threads=1 | %s", headerDesc))

	tmpDir, err := os.MkdirTemp(opt.OutputDir, ".anidl_tmp_*")
	if err != nil {
		tmpDir, err = os.MkdirTemp("", "anidl_tmp_*")
		if err != nil {
			return "", fmt.Errorf("建立暫存目錄失敗: %w", err)
		}
	}
	defer os.RemoveAll(tmpDir)

	client, err := deps.newClient(ctx, hlsClientConfig{
		RefererEnabled: opt.RefererEnabled,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      ua,
		JA3:            opt.JA3,
		Akamai:         opt.Akamai,
		Timeout:        60 * time.Second,
		OnLog:          cb.OnLog,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
			cb.OnLog("[警告]", "#fbbf24", "使用者手動中止任務。")
			return "", context.Canceled
		}
		return "", fmt.Errorf("初始化 HTTP 客戶端失敗: %w", err)
	}
	defer client.Close()

	outPath, err := executeNativeHLSPipeline(ctx, opt, cb, deps, client, tmpDir)
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
			cb.OnLog("[警告]", "#fbbf24", "使用者手動中止任務。")
			return "", context.Canceled
		}
		return "", err
	}

	return outPath, nil
}

func executeNativeHLSPipeline(
	ctx context.Context,
	opt DownloadOptions,
	cb DownloadCallbacks,
	deps hlsRunnerDeps,
	client hlsHTTPClient,
	tmpDir string,
) (string, error) {
	rootResp, err := deps.getWithRetry(ctx, client, opt.M3U8Url, nil, 4, cb.OnLog)
	if err != nil {
		return "", err
	}
	rootBase := rootResp.FinalURL
	if strings.TrimSpace(rootBase) == "" {
		rootBase = opt.M3U8Url
	}

	rootPL, err := deps.parseM3U8(string(rootResp.Body), rootBase)
	if err != nil {
		if looksLikeHTMLWebpage(rootResp.Body) {
			return "", fmt.Errorf("%w: 解析 M3U8 清單失敗: %v", errNonM3U8Webpage, err)
		}
		return "", fmt.Errorf("解析 M3U8 清單失敗: %w", err)
	}

	videoPL := rootPL
	var audioPL *hlsPlaylist

	if rootPL.IsMaster {
		best, err := deps.selectBest(rootPL.Variants)
		if err != nil {
			return "", fmt.Errorf("選取最佳畫質變體失敗: %w", err)
		}
		resLabel := best.Resolution
		if resLabel == "" {
			if best.Width > 0 && best.Height > 0 {
				resLabel = fmt.Sprintf("%dx%d", best.Width, best.Height)
			} else {
				resLabel = "最高碼率"
			}
		}
		cb.OnLog("[解析]", "#38bdf8", fmt.Sprintf("偵測到 Master 清單，選取最高畫質 %s (%d kbps)", resLabel, best.Bandwidth/1000))

		vResp, err := deps.getWithRetry(ctx, client, best.URI, nil, 4, cb.OnLog)
		if err != nil {
			return "", err
		}
		vBase := vResp.FinalURL
		if strings.TrimSpace(vBase) == "" {
			vBase = best.URI
		}
		videoPL, err = deps.parseM3U8(string(vResp.Body), vBase)
		if err != nil {
			return "", fmt.Errorf("解析視訊媒體清單失敗: %w", err)
		}
		if videoPL.IsMaster {
			return "", fmt.Errorf("不支援多層巢狀 Master 清單，請提供有效的串流網址")
		}

		if strings.TrimSpace(best.AudioURI) != "" {
			cb.OnLog("[解析]", "#38bdf8", "偵測到獨立音軌，將自動下載並合併")
			aResp, err := deps.getWithRetry(ctx, client, best.AudioURI, nil, 4, cb.OnLog)
			if err != nil {
				return "", err
			}
			aBase := aResp.FinalURL
			if strings.TrimSpace(aBase) == "" {
				aBase = best.AudioURI
			}
			audioPL, err = deps.parseM3U8(string(aResp.Body), aBase)
			if err != nil {
				return "", fmt.Errorf("解析獨立音軌清單失敗: %w", err)
			}
			if audioPL.IsMaster {
				return "", fmt.Errorf("不支援多層巢狀 Master 音軌清單，請提供有效的串流網址")
			}
		}
	}

	if !videoPL.IsVOD {
		cb.OnLog("[警告]", "#fbbf24", "視訊清單缺少 #EXT-X-ENDLIST，可能為直播或未完結串流；原生引擎目前僅支援 VOD 點播下載。")
		return "", fmt.Errorf("不支援直播或未完結串流（視訊清單缺少 #EXT-X-ENDLIST）")
	}
	if audioPL != nil && !audioPL.IsVOD {
		cb.OnLog("[警告]", "#fbbf24", "音軌清單缺少 #EXT-X-ENDLIST，可能為直播或未完結串流；原生引擎目前僅支援 VOD 點播下載。")
		return "", fmt.Errorf("不支援直播或未完結串流（音軌清單缺少 #EXT-X-ENDLIST）")
	}

	keyCache := make(map[string][]byte)
	tracker := newSpeedTracker(5 * time.Second)

	videoEndPct := 98.0
	if audioPL != nil {
		videoEndPct = 86.0
	}

	videoInput, err := downloadHLSTrack(ctx, client, deps, videoPL, tmpDir, "video", "視訊", 0.0, videoEndPct, keyCache, tracker, cb)
	if err != nil {
		return "", err
	}

	var audioInput *downloadedTrackInput
	if audioPL != nil {
		aIn, err := downloadHLSTrack(ctx, client, deps, audioPL, tmpDir, "audio", "音軌", 86.0, 98.0, keyCache, tracker, cb)
		if err != nil {
			return "", err
		}
		audioInput = &aIn
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	cb.OnProgress(ProgressUpdate{
		Engine:  "hls",
		Percent: 99.0,
		Merging: true,
	})
	cb.OnLog("[封裝]", "#38bdf8", "分片下載完成，正在使用本機 FFMPEG 無損封裝 MP4...")

	tmpOutMP4 := filepath.Join(tmpDir, "output.mp4")
	remuxArgs := buildHLSRemuxArgs(videoInput, audioInput, tmpOutMP4)
	if err := deps.remuxFn(ctx, opt.FFmpegPath, remuxArgs, cb.OnLog); err != nil {
		return "", err
	}

	if !fileExistsNonEmpty(tmpOutMP4) {
		cb.OnLog("[錯誤]", "#f87171", fmt.Sprintf("FFMPEG 封裝完成但找不到有效輸出檔案: %s", tmpOutMP4))
		return "", fmt.Errorf("FFMPEG 封裝完成但未產生有效的 MP4 輸出檔案")
	}

	outFilePath := filepath.Join(opt.OutputDir, opt.Title+".mp4")
	if err := moveFileOverwrite(tmpOutMP4, outFilePath); err != nil {
		return "", fmt.Errorf("寫入最終輸出檔案失敗: %w", err)
	}

	cb.OnProgress(ProgressUpdate{
		Engine:  "hls",
		Percent: 100.0,
	})
	cb.OnLog("[完成]", "#34d399", fmt.Sprintf("任務完成，已輸出檔案至 %s", outFilePath))
	return outFilePath, nil
}

func mapCacheKey(m *hlsMapInfo) string {
	if m == nil {
		return ""
	}
	if m.ByteRange != nil {
		return fmt.Sprintf("%s#%d:%d", m.URI, m.ByteRange.Offset, m.ByteRange.Length)
	}
	return m.URI
}

func byteRangeHeaders(br *hlsByteRange) map[string]string {
	if br == nil {
		return nil
	}
	return map[string]string{
		"Range": fmt.Sprintf("bytes=%d-%d", br.Offset, br.Offset+br.Length-1),
	}
}

// sliceByteRangePayload extracts the requested sub-range if a server ignored the Range
// header and returned a full 200 OK body, preventing file bloat and corruption.
func sliceByteRangePayload(resp *hlsResponse, br *hlsByteRange) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("空白的 HTTP 回應")
	}
	if br == nil || resp.StatusCode == 206 {
		return resp.Body, nil
	}
	if resp.StatusCode == 200 {
		start := br.Offset
		end := start + br.Length
		if start < 0 || br.Length <= 0 || end > int64(len(resp.Body)) {
			return nil, fmt.Errorf("伺服器未套用 Range 且回傳內容長度不足（預期區間 %d-%d，實際 %d bytes）", start, end-1, len(resp.Body))
		}
		return resp.Body[start:end], nil
	}
	return resp.Body, nil
}

func downloadHLSTrack(
	ctx context.Context,
	client hlsHTTPClient,
	deps hlsRunnerDeps,
	pl *hlsPlaylist,
	tmpDir string,
	trackName string,
	trackLabel string,
	startPct float64,
	endPct float64,
	keyCache map[string][]byte,
	tracker *speedTracker,
	cb DownloadCallbacks,
) (downloadedTrackInput, error) {
	if len(pl.Segments) == 0 {
		return downloadedTrackInput{}, fmt.Errorf("%s清單沒有可下載的分片", trackLabel)
	}

	var activeFile *os.File
	defer func() {
		if activeFile != nil {
			_ = activeFile.Close()
		}
	}()

	closeActiveFile := func() error {
		if activeFile == nil {
			return nil
		}
		err := activeFile.Close()
		activeFile = nil
		return err
	}

	var (
		result          downloadedTrackInput
		trackDir        string
		singlePath      string
		concatLines     []string
		concatMode      bool
		partIdx         int
		activeMapKey    string
		activeInitBytes []byte
		segmentsSynced  int
	)

	rotateFMP4Part := func() error {
		if err := closeActiveFile(); err != nil {
			return err
		}
		if !concatMode {
			trackDir = filepath.Join(tmpDir, trackName)
			if err := os.MkdirAll(trackDir, 0755); err != nil {
				return fmt.Errorf("建立%s fMP4 暫存子目錄失敗: %w", trackLabel, err)
			}
			firstPartName := fmt.Sprintf("part_%06d.m4s", partIdx)
			if err := os.Rename(singlePath, filepath.Join(trackDir, firstPartName)); err != nil {
				return err
			}
			concatLines = append(concatLines, fmt.Sprintf("file '%s'", firstPartName))
			partIdx++
			concatMode = true
		}
		nextPartName := fmt.Sprintf("part_%06d.m4s", partIdx)
		f, err := os.Create(filepath.Join(trackDir, nextPartName))
		if err != nil {
			return fmt.Errorf("建立%s fMP4 分段檔失敗: %w", trackLabel, err)
		}
		activeFile = f
		concatLines = append(concatLines, fmt.Sprintf("file '%s'", nextPartName))
		partIdx++
		return nil
	}

	result.isFMP4 = pl.IsFMP4

	if !pl.IsFMP4 && pl.HasDiscontinuity {
		trackDir = filepath.Join(tmpDir, trackName)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return downloadedTrackInput{}, fmt.Errorf("建立%s暫存子目錄失敗: %w", trackLabel, err)
		}
		concatMode = true
	} else if !pl.IsFMP4 {
		singlePath = filepath.Join(tmpDir, trackName+".ts")
		f, err := os.Create(singlePath)
		if err != nil {
			return downloadedTrackInput{}, fmt.Errorf("建立%s暫存檔失敗: %w", trackLabel, err)
		}
		activeFile = f
	} else {
		singlePath = filepath.Join(tmpDir, trackName+".m4s")
	}

	var (
		lastUIAt      time.Time
		lastLogAt     time.Time
		lastLogBucket = -1
		total         = len(pl.Segments)
	)

	for i, seg := range pl.Segments {
		if err := ctx.Err(); err != nil {
			return downloadedTrackInput{}, context.Canceled
		}

		if pl.IsFMP4 {
			mKey := mapCacheKey(seg.Map)
			if mKey != "" && mKey != activeMapKey {
				initStart := time.Now()
				initResp, err := deps.getWithRetry(ctx, client, seg.Map.URI, byteRangeHeaders(seg.Map.ByteRange), 4, cb.OnLog)
				if err != nil {
					return downloadedTrackInput{}, err
				}
				initBody, err := sliceByteRangePayload(initResp, seg.Map.ByteRange)
				if err != nil {
					return downloadedTrackInput{}, fmt.Errorf("擷取%s初始化分片區間失敗: %w", trackLabel, err)
				}
				tracker.AddSpan(int64(len(initBody)), initStart, time.Now())
				activeInitBytes = initBody

				if segmentsSynced == 0 {
					if activeFile == nil {
						f, err := os.Create(singlePath)
						if err != nil {
							return downloadedTrackInput{}, fmt.Errorf("建立%s fMP4 暫存檔失敗: %w", trackLabel, err)
						}
						activeFile = f
					}
				} else {
					if err := rotateFMP4Part(); err != nil {
						return downloadedTrackInput{}, err
					}
				}

				if _, err := activeFile.Write(activeInitBytes); err != nil {
					return downloadedTrackInput{}, fmt.Errorf("寫入%s初始化分片失敗: %w", trackLabel, err)
				}
				activeMapKey = mKey
			} else if seg.Discontinuity && segmentsSynced > 0 {
				if err := rotateFMP4Part(); err != nil {
					return downloadedTrackInput{}, err
				}
				if len(activeInitBytes) > 0 {
					if _, err := activeFile.Write(activeInitBytes); err != nil {
						return downloadedTrackInput{}, fmt.Errorf("寫入%s初始化分片失敗: %w", trackLabel, err)
					}
				}
			} else if activeFile == nil {
				f, err := os.Create(singlePath)
				if err != nil {
					return downloadedTrackInput{}, fmt.Errorf("建立%s fMP4 暫存檔失敗: %w", trackLabel, err)
				}
				activeFile = f
			}
		}

		segStart := time.Now()
		segResp, err := deps.getWithRetry(ctx, client, seg.URI, byteRangeHeaders(seg.ByteRange), 4, cb.OnLog)
		if err != nil {
			return downloadedTrackInput{}, err
		}
		payload, err := sliceByteRangePayload(segResp, seg.ByteRange)
		if err != nil {
			return downloadedTrackInput{}, fmt.Errorf("擷取%s第 %d/%d 分片區間失敗: %w", trackLabel, i+1, total, err)
		}

		if seg.Key != nil && strings.EqualFold(seg.Key.Method, "AES-128") {
			key, ok := keyCache[seg.Key.URI]
			if !ok {
				keyResp, err := deps.getWithRetry(ctx, client, seg.Key.URI, nil, 4, cb.OnLog)
				if err != nil {
					return downloadedTrackInput{}, err
				}
				if len(keyResp.Body) != 16 {
					return downloadedTrackInput{}, fmt.Errorf("AES-128 金鑰長度無效: %d bytes（預期 16 bytes）", len(keyResp.Body))
				}
				key = keyResp.Body
				keyCache[seg.Key.URI] = key
			}

			iv := seg.Key.IV
			if len(iv) != 16 {
				iv = deps.buildSeqIV(seg.SeqNum)
			}

			decrypted, err := deps.decryptAES128(payload, key, iv)
			if err != nil {
				return downloadedTrackInput{}, fmt.Errorf("解密%s第 %d/%d 分片失敗: %w", trackLabel, i+1, total, err)
			}
			payload = decrypted
		}

		if !pl.IsFMP4 && pl.HasDiscontinuity {
			segName := fmt.Sprintf("seg_%06d.ts", i+1)
			segPath := filepath.Join(trackDir, segName)
			if err := os.WriteFile(segPath, payload, 0644); err != nil {
				return downloadedTrackInput{}, fmt.Errorf("寫入%s分片 %s 失敗: %w", trackLabel, segName, err)
			}
			concatLines = append(concatLines, fmt.Sprintf("file '%s'", segName))
		} else {
			if _, err := activeFile.Write(payload); err != nil {
				return downloadedTrackInput{}, fmt.Errorf("寫入%s分片失敗: %w", trackLabel, err)
			}
		}
		segmentsSynced++

		now := time.Now()
		tracker.AddSpan(int64(len(payload)), segStart, now)
		speedStr := formatByteSpeed(tracker.SpeedBps(now))
		pct := startPct + (endPct-startPct)*(float64(i+1)/float64(total))
		detail := fmt.Sprintf("frag %d/%d", i+1, total)
		isLast := i == total-1

		if isLast || lastUIAt.IsZero() || now.Sub(lastUIAt) >= 250*time.Millisecond {
			lastUIAt = now
			cb.OnProgress(ProgressUpdate{
				Engine:  "hls",
				Percent: pct,
				Speed:   speedStr,
				Detail:  detail,
			})
		}

		bucket := int(pct) / 10
		if isLast || lastLogAt.IsZero() || now.Sub(lastLogAt) >= 2*time.Second || bucket > lastLogBucket {
			lastLogAt = now
			lastLogBucket = bucket
			cb.OnLog("[進度]", "#00B4DF", fmt.Sprintf("[%s] %.1f%% | %s | %s", trackLabel, pct, detail, speedStr))
		}
	}

	if err := closeActiveFile(); err != nil {
		return downloadedTrackInput{}, fmt.Errorf("關閉%s暫存檔失敗: %w", trackLabel, err)
	}

	if concatMode {
		concatPath := filepath.Join(trackDir, "concat.txt")
		concatBody := strings.Join(concatLines, "\n") + "\n"
		if err := os.WriteFile(concatPath, []byte(concatBody), 0644); err != nil {
			return downloadedTrackInput{}, fmt.Errorf("寫入%s concat 清單失敗: %w", trackLabel, err)
		}
		result.inputPath = concatPath
		result.isConcat = true
		return result, nil
	}

	result.inputPath = singlePath
	result.isConcat = false
	return result, nil
}

func buildHLSRemuxArgs(videoIn downloadedTrackInput, audioIn *downloadedTrackInput, tmpOutMP4 string) []string {
	args := []string{"-hide_banner", "-loglevel", "error"}

	appendInput := func(in downloadedTrackInput) {
		if in.isConcat {
			args = append(args, "-f", "concat", "-safe", "0", "-i", in.inputPath)
		} else {
			args = append(args, "-i", in.inputPath)
		}
	}

	appendInput(videoIn)
	if audioIn != nil {
		appendInput(*audioIn)
		args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	} else {
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
	}

	args = append(args, "-dn", "-sn", "-c", "copy")
	audioIsTS := (!videoIn.isFMP4 && audioIn == nil) || (audioIn != nil && !audioIn.isFMP4)
	if audioIsTS {
		args = append(args, "-bsf:a", "aac_adtstoasc")
	}
	args = append(args, "-movflags", "+faststart", "-y", tmpOutMP4)
	return args
}

func runLocalFFmpegRemux(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
	if strings.TrimSpace(ffmpegPath) == "" {
		return fmt.Errorf("找不到本機 FFMPEG 執行檔路徑")
	}
	if onLog == nil {
		onLog = func(string, string, string) {}
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("建立 FFMPEG STDERR 管道失敗: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("啟動 FFMPEG 封裝失敗: %w", err)
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
		mu       sync.Mutex
		lastLine string
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanLinesAndReturns(stderr, func(line string) {
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			mu.Lock()
			lastLine = line
			mu.Unlock()
			onLog("[FFMPEG]", "#f87171", line)
		})
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(stopWatch)
	<-watchDone

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		mu.Lock()
		detail := lastLine
		mu.Unlock()
		if detail != "" {
			return fmt.Errorf("FFMPEG 封裝失敗 (%s): %w", detail, waitErr)
		}
		return fmt.Errorf("FFMPEG 封裝失敗: %w", waitErr)
	}
	return nil
}

// replaceFileSafe atomically replaces dst with src on the same filesystem,
// temporarily backing up an existing dst so a rename failure restores the original file.
func replaceFileSafe(src, dst string) error {
	_, statErr := os.Stat(dst)
	if os.IsNotExist(statErr) {
		return renameWithRetry(src, dst)
	}
	if statErr != nil {
		return statErr
	}

	bakPath := fmt.Sprintf("%s.bak.%d", dst, time.Now().UnixNano())
	if err := renameWithRetry(dst, bakPath); err != nil {
		return err
	}
	if err := renameWithRetry(src, dst); err != nil {
		_ = renameWithRetry(bakPath, dst)
		return err
	}
	_ = os.Remove(bakPath)
	return nil
}

func moveFileOverwrite(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := replaceFileSafe(src, dst); err == nil {
		return nil
	}
	return copyFileAtomicOverwrite(src, dst)
}

func copyFileAtomicOverwrite(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dstDir := filepath.Dir(dst)
	tmpFile, err := os.CreateTemp(dstDir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := io.Copy(tmpFile, in); err != nil {
		cleanup()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceFileSafe(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = in.Close()
	_ = os.Remove(src)
	return nil
}
