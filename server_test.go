//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartDownloadTaskInvalidURLDoesNotInterruptActive(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	started := make(chan struct{})
	cancelled := atomic.Bool{}
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
		return "", context.Canceled
	}

	if err := srv.StartDownloadTask(downloadRequest{
		URL:    "https://example.com/live.m3u8",
		Engine: "hls",
	}); err != nil {
		t.Fatalf("initial StartDownloadTask failed: %v", err)
	}
	<-started

	for _, bad := range []string{"", "not-a-url", "ftp://example.com/a.m3u8", "https://example.com/a\r\nX:1"} {
		if err := srv.StartDownloadTask(downloadRequest{URL: bad, Engine: "hls"}); err == nil {
			t.Fatalf("expected error for invalid URL %q", bad)
		}
	}

	if cancelled.Load() {
		t.Fatal("invalid URL submission must NOT cancel active valid download")
	}
	if srv.ActiveEngine() != "hls" {
		t.Fatalf("ActiveEngine = %q, want hls", srv.ActiveEngine())
	}

	srv.Shutdown()
	if !cancelled.Load() {
		t.Fatal("expected active download to be cancelled on Shutdown")
	}
}

func TestStartDownloadTaskRapidRestartTaskID(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	var (
		mu          sync.Mutex
		events      []string
		runCount    atomic.Int32
		activeCount atomic.Int32
		maxOverlap  atomic.Int32
	)
	srv.SetNativeEmitFn(func(event string, payload string) {
		if event == "aborted" || event == "done" || event == "error_event" {
			mu.Lock()
			events = append(events, event+":"+payload)
			mu.Unlock()
		}
	})

	lastStarted := make(chan struct{}, 10)
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		cur := activeCount.Add(1)
		for {
			prev := maxOverlap.Load()
			if cur <= prev || maxOverlap.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer activeCount.Add(-1)

		n := runCount.Add(1)
		select {
		case lastStarted <- struct{}{}:
		default:
		}
		<-ctx.Done()
		// Simulate child process tree teardown delay
		if n < 6 {
			time.Sleep(25 * time.Millisecond)
		}
		return "", context.Canceled
	}

	for i := 1; i <= 6; i++ {
		err := srv.StartDownloadTask(downloadRequest{
			URL:    fmt.Sprintf("https://example.com/%d.m3u8", i),
			Engine: "hls",
			JA3:    "",
			Akamai: "",
		})
		if err != nil {
			t.Fatalf("restart #%d failed: %v", i, err)
		}
	}

	// Wait until 6th task enters runDownloadFn
	deadline := time.After(3 * time.Second)
	for runCount.Load() < 6 {
		select {
		case <-lastStarted:
		case <-deadline:
			t.Fatalf("timed out waiting for 6th task to start, count=%d", runCount.Load())
		}
	}

	if maxOverlap.Load() > 1 {
		t.Fatalf("overlapping download tasks detected: maxOverlap=%d, want 1", maxOverlap.Load())
	}

	// Give superseded tasks a moment and ensure none emitted aborted
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if len(events) != 0 {
		t.Fatalf("superseded tasks must not emit terminal events, got %v", events)
	}
	mu.Unlock()

	if srv.ActiveEngine() != "hls" {
		t.Fatalf("latest task should still be active, got %q", srv.ActiveEngine())
	}

	// Stop the latest task and verify it emits exactly one aborted event
	srv.StopActiveDownload()
	srv.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || !strings.HasPrefix(events[0], "aborted:") {
		t.Fatalf("expected exactly one aborted event for latest task, got %v", events)
	}
}

func TestStartDownloadTaskConcurrentPreemptionNoOverlap(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	var (
		activeCount atomic.Int32
		maxOverlap  atomic.Int32
		runCount    atomic.Int32
	)
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		cur := activeCount.Add(1)
		for {
			prev := maxOverlap.Load()
			if cur <= prev || maxOverlap.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer activeCount.Add(-1)

		runCount.Add(1)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		return "", context.Canceled
	}

	const workers = 10
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startGate
			errs <- srv.StartDownloadTask(downloadRequest{
				URL:    fmt.Sprintf("https://example.com/concurrent-%d.m3u8", idx),
				Engine: "hls",
			})
		}(i)
	}
	close(startGate)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent StartDownloadTask failed: %v", err)
		}
	}

	if maxOverlap.Load() != 1 {
		t.Fatalf("maxOverlap = %d, want 1 (strictly serialized)", maxOverlap.Load())
	}

	// Wait briefly for the final spawned goroutine to enter runDownloadFn before stopping it
	deadline := time.Now().Add(2 * time.Second)
	for runCount.Load() < workers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runCount.Load() != workers {
		t.Fatalf("runCount = %d, want %d", runCount.Load(), workers)
	}

	srv.StopActiveDownload()
	srv.Shutdown()
	if activeCount.Load() != 0 {
		t.Fatalf("expected 0 active downloads after Shutdown, got %d", activeCount.Load())
	}
}

func TestShutdownWaitsForChildCleanup(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	started := make(chan struct{})
	cleaned := atomic.Bool{}
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(120 * time.Millisecond)
		cleaned.Store(true)
		return "", context.Canceled
	}

	if err := srv.StartDownloadTask(downloadRequest{URL: "https://example.com/a.m3u8", Engine: "ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	<-started

	srv.Shutdown()
	if !cleaned.Load() {
		t.Fatal("Shutdown returned before active download goroutine finished cleaning up")
	}
}

func TestStartDownloadTaskPreservesEmptyFingerprintsAndNormalizesLegacyEngine(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	gotOpt := make(chan DownloadOptions, 1)
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		gotOpt <- opt
		return "out.mp4", nil
	}

	err := srv.StartDownloadTask(downloadRequest{
		URL:            "https://example.com/a.m3u8",
		Engine:         "ytdlp",
		RefererEnabled: true,
		UserAgent:      "",
		JA3:            "",
		Akamai:         "",
	})
	if err != nil {
		t.Fatal(err)
	}

	opt := <-gotOpt
	if opt.Engine != "hls" {
		t.Fatalf("expected legacy ytdlp engine to normalize to hls, got %q", opt.Engine)
	}
	if opt.JA3 != "" || opt.Akamai != "" || opt.UserAgent != "" {
		t.Fatalf("expected empty request fingerprints not to be backfilled from config, got %+v", opt)
	}
	srv.Shutdown()
}

func TestFallbackAPIProtectionAndClearLogs(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start failed: %v", err)
	}
	defer srv.Shutdown()

	baseURL := srv.GetBaseURL()
	client := &http.Client{Timeout: 3 * time.Second}

	// 1. POST without X-Ani-Client -> 403
	req1, _ := http.NewRequest(http.MethodPost, baseURL+"/api/stop", nil)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without X-Ani-Client status = %d, want 403", resp1.StatusCode)
	}

	// 2. POST with wrong Origin -> 403
	req2, _ := http.NewRequest(http.MethodPost, baseURL+"/api/stop", nil)
	req2.Header.Set("X-Ani-Client", "1")
	req2.Header.Set("Origin", "https://evil.example")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with cross-origin Origin status = %d, want 403", resp2.StatusCode)
	}

	// 3. Request with mismatched Host -> 403
	req3, _ := http.NewRequest(http.MethodGet, baseURL+"/api/init", nil)
	req3.Host = "evil.local:9999"
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("GET with mismatched Host status = %d, want 403", resp3.StatusCode)
	}

	// 4. Add log, verify via /api/init, clear via POST /api/logs/clear, verify cleared
	srv.AddLog("[提示]", "#38bdf8", "test log line")
	req4, _ := http.NewRequest(http.MethodGet, baseURL+"/api/init", nil)
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	if resp4.StatusCode != http.StatusOK {
		resp4.Body.Close()
		t.Fatalf("GET /api/init status = %d, want 200", resp4.StatusCode)
	}
	var stateBefore struct {
		Logs []LogEntry `json:"logs"`
	}
	err = json.NewDecoder(resp4.Body).Decode(&stateBefore)
	resp4.Body.Close()
	if err != nil {
		t.Fatalf("failed to decode /api/init response: %v", err)
	}
	if len(stateBefore.Logs) == 0 {
		t.Fatal("expected at least 1 log before clear")
	}

	req5, _ := http.NewRequest(http.MethodPost, baseURL+"/api/logs/clear", nil)
	req5.Header.Set("X-Ani-Client", "1")
	req5.Header.Set("Origin", baseURL)
	resp5, err := client.Do(req5)
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/logs/clear status = %d, want 200", resp5.StatusCode)
	}

	stateAfter := srv.GetInitState()
	logsAfter, _ := stateAfter["logs"].([]LogEntry)
	if len(logsAfter) != 0 {
		t.Fatalf("expected 0 logs after ClearLogs, got %d", len(logsAfter))
	}
}

func TestHandleDownloadHTTPStatusMapping(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start failed: %v", err)
	}
	defer srv.Shutdown()

	baseURL := srv.GetBaseURL()
	client := &http.Client{Timeout: 3 * time.Second}

	type downloadAPIResp struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		ActiveEngine string `json:"activeEngine"`
	}

	postDownload := func(rawBody string) (int, downloadAPIResp) {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/api/download", strings.NewReader(rawBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Ani-Client", "1")
		req.Header.Set("Origin", baseURL)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var body downloadAPIResp
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode /api/download response (status=%d): %v", resp.StatusCode, err)
		}
		return resp.StatusCode, body
	}

	// 1. Before SetToolsReady -> 503 Service Unavailable
	status, body := postDownload(`{"url":"https://example.com/live.m3u8","engine":"hls"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/download before tools ready status = %d, want 503", status)
	}
	if body.OK || body.Error == "" || body.ActiveEngine != "" {
		t.Fatalf("unexpected 503 payload: %+v", body)
	}

	// Prepare ready tools and blocking stub downloader
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	initReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/init", nil)
	initResp, err := client.Do(initReq)
	if err != nil {
		t.Fatal(err)
	}
	var initPayload struct {
		CoreVersion   string `json:"coreVersion"`
		FFmpegVersion string `json:"ffmpegVersion"`
	}
	if err := json.NewDecoder(initResp.Body).Decode(&initPayload); err != nil {
		initResp.Body.Close()
		t.Fatal(err)
	}
	initResp.Body.Close()
	if initPayload.CoreVersion != NativeHLSEngineVersion || initPayload.FFmpegVersion != "v6.1.1" {
		t.Fatalf("unexpected /api/init versions: %+v", initPayload)
	}

	started := make(chan struct{})
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		close(started)
		<-ctx.Done()
		return "", context.Canceled
	}

	// 2. Malformed JSON -> 400 Bad Request
	status, body = postDownload(`{bad-json`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST /api/download malformed JSON status = %d, want 400", status)
	}
	if body.OK || body.Error == "" {
		t.Fatalf("unexpected 400 malformed JSON payload: %+v", body)
	}

	// 3. Valid download request -> 200 OK
	status, body = postDownload(`{"url":"https://example.com/live.m3u8","engine":"hls"}`)
	if status != http.StatusOK || !body.OK {
		t.Fatalf("POST /api/download valid status = %d, body = %+v, want 200 ok=true", status, body)
	}
	<-started

	// 4. Invalid URL while hls is active -> 400 Bad Request with activeEngine="hls"
	status, body = postDownload(`{"url":"ftp://bad","engine":"ffmpeg"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST /api/download invalid URL status = %d, want 400", status)
	}
	if body.OK || body.Error == "" || body.ActiveEngine != "hls" {
		t.Fatalf("expected activeEngine=hls on 400 error while running, got %+v", body)
	}

	srv.Shutdown()
}

func TestStartDownloadTaskFiltersStaleTaskLogs(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	staleStarted := make(chan struct{})
	releaseStale := make(chan struct{})
	var callNum atomic.Int32
	srv.runDownloadFn = func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error) {
		n := callNum.Add(1)
		if n == 1 {
			close(staleStarted)
			<-ctx.Done()
			// Wait until second task becomes active, then emit stale log
			<-releaseStale
			cb.OnLog("[舊任務]", "#fbbf24", "stale task log should be dropped")
			return "", context.Canceled
		}
		cb.OnLog("[新任務]", "#38bdf8", "current task log kept")
		<-ctx.Done()
		return "", context.Canceled
	}

	if err := srv.StartDownloadTask(downloadRequest{URL: "https://example.com/1.m3u8", Engine: "hls"}); err != nil {
		t.Fatal(err)
	}
	<-staleStarted

	// Launch task 2 while blocking task 1's OnLog emission via releaseStale after preemption starts
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(releaseStale)
	}()
	if err := srv.StartDownloadTask(downloadRequest{URL: "https://example.com/2.m3u8", Engine: "hls"}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	srv.Shutdown()

	state := srv.GetInitState()
	logs, _ := state["logs"].([]LogEntry)
	var sawNew bool
	for _, entry := range logs {
		if strings.Contains(entry.Message, "stale task log") {
			t.Fatalf("stale task log leaked into active console logs: %+v", entry)
		}
		if strings.Contains(entry.Message, "current task log") {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("expected current task log to be recorded, got %+v", logs)
	}
}

func TestServerInitStateYtDlpOptionalAndUpdateProtection(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := NewAppServer()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// yt-dlp is strictly optional: toolsReady succeeds without yt-dlp.exe
	srv.SetToolsReady(ToolInfo{FFmpegVersion: "v6.1.1", CoreVersion: NativeHLSEngineVersion})

	srv.mu.Lock()
	baseURL := srv.baseURL
	srv.mu.Unlock()

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Check /api/init returns ytdlpInstalled=false, ytdlpVersion="", toolsReady=true
	initReq, _ := http.NewRequest(http.MethodGet, baseURL+"/api/init", nil)
	initResp, err := client.Do(initReq)
	if err != nil {
		t.Fatal(err)
	}
	var initBody struct {
		ToolsReady     bool   `json:"toolsReady"`
		YtDlpInstalled bool   `json:"ytdlpInstalled"`
		YtDlpVersion   string `json:"ytdlpVersion"`
	}
	if err := json.NewDecoder(initResp.Body).Decode(&initBody); err != nil {
		initResp.Body.Close()
		t.Fatal(err)
	}
	initResp.Body.Close()
	if !initBody.ToolsReady || initBody.YtDlpInstalled || initBody.YtDlpVersion != "" {
		t.Fatalf("unexpected /api/init state without yt-dlp: %+v", initBody)
	}

	// 2. POST /api/ytdlp/update without X-Ani-Client: 1 must return 403 Forbidden
	unauthReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/ytdlp/update", nil)
	unauthResp, err := client.Do(unauthReq)
	if err != nil {
		t.Fatal(err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /api/ytdlp/update without X-Ani-Client status = %d, want 403", unauthResp.StatusCode)
	}

	// 3. POST /api/ytdlp/update with X-Ani-Client: 1 updates server tools state
	doneCh := make(chan struct{})
	srv.installYtDlpFn = func(ctx context.Context, logFn func(tag, color, msg string)) (ToolInfo, error) {
		defer close(doneCh)
		return ToolInfo{
			CoreVersion:  NativeHLSEngineVersion,
			YtDlpPath:    `C:\mock\yt-dlp.exe`,
			YtDlpVersion: "2026.03.13",
		}, nil
	}

	authReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/ytdlp/update", nil)
	authReq.Header.Set("X-Ani-Client", "1")
	authResp, err := client.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/ytdlp/update status = %d, want 200", authResp.StatusCode)
	}

	<-doneCh
	// Wait briefly for StartYtDlpUpdate goroutine to store updated tools
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := srv.GetInitState()
		if installed, _ := state["ytdlpInstalled"].(bool); installed {
			if ver, _ := state["ytdlpVersion"].(string); ver != "2026.03.13" {
				t.Fatalf("ytdlpVersion = %q, want 2026.03.13", ver)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for StartYtDlpUpdate to populate ytdlpInstalled=true")
}
