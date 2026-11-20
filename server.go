//go:build windows

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed assets/index.html
var embeddedIndexHTML []byte

var errToolsNotReady = errors.New("內建工具尚未準備就緒，請稍候...")

type LogEntry struct {
	TS      string `json:"ts"`
	Tag     string `json:"tag"`
	Color   string `json:"color"`
	Message string `json:"message"`
}

type sseMessage struct {
	Event string
	Data  string
}

type AppServer struct {
	startMu      sync.Mutex
	mu           sync.Mutex
	config       AppConfig
	tools        ToolInfo
	toolsReady   bool
	toolsFailed  bool
	logs         []LogEntry
	activeTaskID uint64
	activeCancel context.CancelFunc
	activeEngine string
	activeDone   chan struct{}
	downloadWG   sync.WaitGroup

	sseMu      sync.Mutex
	sseClients map[chan sseMessage]struct{}

	browseFolderFn func() string
	nativeEmitFn   func(event string, jsonPayload string)
	runDownloadFn  func(context.Context, DownloadOptions, DownloadCallbacks) (string, error)

	listener   net.Listener
	listenHost string
	server     *http.Server
	BaseURL    string
}

func NewAppServer() *AppServer {
	return &AppServer{
		config:        LoadConfig(),
		sseClients:    make(map[chan sseMessage]struct{}),
		runDownloadFn: RunDownload,
	}
}

func (s *AppServer) SetNativeEmitFn(fn func(event string, jsonPayload string)) {
	s.mu.Lock()
	s.nativeEmitFn = fn
	s.mu.Unlock()
}

func (s *AppServer) SetBrowseFolderFn(fn func() string) {
	s.mu.Lock()
	s.browseFolderFn = fn
	s.mu.Unlock()
}

func (s *AppServer) GetConfig() AppConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

func (s *AppServer) ActiveEngine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeEngine
}

func (s *AppServer) GetBaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BaseURL
}

func (s *AppServer) ClearLogs() {
	s.mu.Lock()
	s.logs = nil
	s.mu.Unlock()
}

func (s *AppServer) isCurrentTask(taskID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTaskID == taskID
}

func (s *AppServer) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/init", s.protectAPI(s.handleInit))
	mux.HandleFunc("/api/config", s.protectAPI(s.handleConfig))
	mux.HandleFunc("/api/browse", s.protectAPI(s.handleBrowse))
	mux.HandleFunc("/api/download", s.protectAPI(s.handleDownload))
	mux.HandleFunc("/api/stop", s.protectAPI(s.handleStop))
	mux.HandleFunc("/api/logs/clear", s.protectAPI(s.handleClearLogs))
	mux.HandleFunc("/api/events", s.protectAPI(s.handleEvents))
	return mux
}

func (s *AppServer) Start() error {
	s.mu.Lock()
	if s.server != nil {
		s.mu.Unlock()
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.mu.Unlock()
		return err
	}
	host := ln.Addr().String()
	srv := &http.Server{Handler: s.buildHandler()}
	s.listener = ln
	s.listenHost = host
	s.BaseURL = "http://" + host
	s.server = srv
	s.mu.Unlock()

	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

func (s *AppServer) Shutdown() {
	s.StopActiveDownload()

	waitDone := make(chan struct{})
	go func() {
		s.downloadWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
	}

	s.mu.Lock()
	srv := s.server
	s.server = nil
	s.listener = nil
	s.listenHost = ""
	s.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func (s *AppServer) protectAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		allowedHost := s.listenHost
		baseURL := s.BaseURL
		s.mu.Unlock()

		if allowedHost != "" && !strings.EqualFold(r.Host, allowedHost) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !strings.EqualFold(origin, baseURL) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if r.Header.Get("X-Ani-Client") != "1" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}

		next(w, r)
	}
}

func (s *AppServer) GetInitState() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"config":        s.config,
		"toolsReady":    s.toolsReady,
		"toolsFailed":   s.toolsFailed,
		"ffmpegVersion": s.tools.FFmpegVersion,
		"ytdlpVersion":  s.tools.YtDlpVersion,
		"activeEngine":  s.activeEngine,
		"logs":          append([]LogEntry(nil), s.logs...),
	}
}

func (s *AppServer) UpdateConfig(incoming AppConfig) {
	s.mu.Lock()
	if incoming.Lang == "zh-TW" || incoming.Lang == "zh-CN" || incoming.Lang == "en" {
		s.config.Lang = incoming.Lang
	}
	if incoming.Opacity >= 15 && incoming.Opacity <= 85 {
		s.config.Opacity = incoming.Opacity
	}
	s.config.RefererEnabled = incoming.RefererEnabled
	if incoming.SaveDir != "" {
		s.config.SaveDir = incoming.SaveDir
	}
	s.config.UserAgent = incoming.UserAgent
	s.config.JA3 = incoming.JA3
	s.config.Akamai = incoming.Akamai
	cfgCopy := s.config
	s.mu.Unlock()

	SaveConfig(cfgCopy)
}

func (s *AppServer) SetSaveDir(dir string) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	s.config.SaveDir = dir
	cfgCopy := s.config
	s.mu.Unlock()
	SaveConfig(cfgCopy)
}

func (s *AppServer) SetToolsReady(info ToolInfo) {
	s.mu.Lock()
	s.tools = info
	s.toolsReady = true
	s.toolsFailed = false
	s.mu.Unlock()

	s.Broadcast("ready", map[string]interface{}{
		"ready":         true,
		"failed":        false,
		"ffmpegVersion": info.FFmpegVersion,
		"ytdlpVersion":  info.YtDlpVersion,
	})
}

func (s *AppServer) SetToolsFailed(err error) {
	s.mu.Lock()
	s.toolsReady = false
	s.toolsFailed = true
	s.mu.Unlock()

	s.AddLog("[錯誤]", "#f87171", fmt.Sprintf("內建元件初始化失敗: %v", err))
	s.Broadcast("ready", map[string]interface{}{
		"ready":  false,
		"failed": true,
	})
}

func (s *AppServer) AddLog(tag, color, message string) {
	entry := LogEntry{
		TS:      time.Now().Format("15:04:05"),
		Tag:     tag,
		Color:   color,
		Message: message,
	}
	s.mu.Lock()
	s.logs = append(s.logs, entry)
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
	s.mu.Unlock()

	s.Broadcast("log", entry)
}

func (s *AppServer) Broadcast(event string, payload interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	dataStr := string(b)

	s.mu.Lock()
	emitFn := s.nativeEmitFn
	s.mu.Unlock()

	// Push to native WebView2 bridge if attached (without holding s.mu)
	if emitFn != nil {
		emitFn(event, dataStr)
	}

	// Push to browser SSE clients if any
	msg := sseMessage{Event: event, Data: dataStr}
	s.sseMu.Lock()
	for ch := range s.sseClients {
		select {
		case ch <- msg:
		default:
		}
	}
	s.sseMu.Unlock()
}

func (s *AppServer) StopActiveDownload() {
	s.mu.Lock()
	cancel := s.activeCancel
	s.activeCancel = nil
	s.activeEngine = ""
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

type downloadRequest struct {
	URL            string `json:"url"`
	Title          string `json:"title"`
	SaveDir        string `json:"saveDir"`
	Engine         string `json:"engine"`
	RefererEnabled bool   `json:"refererEnabled"`
	UserAgent      string `json:"userAgent"`
	JA3            string `json:"ja3"`
	Akamai         string `json:"akamai"`
	Concurrency    int    `json:"concurrency"`
}

func (s *AppServer) StartDownloadTask(req downloadRequest) error {
	cleanURL, err := validateDownloadURL(req.URL)
	if err != nil {
		return err
	}

	s.startMu.Lock()
	defer s.startMu.Unlock()

	s.mu.Lock()
	if !s.toolsReady {
		s.mu.Unlock()
		return errToolsNotReady
	}

	// Cancel and wait for any active or still-exiting download task before launching a new one
	for s.activeCancel != nil || s.activeDone != nil {
		s.activeTaskID++
		oldCancel := s.activeCancel
		oldDone := s.activeDone
		s.activeCancel = nil
		s.activeEngine = ""
		s.activeDone = nil
		s.mu.Unlock()

		if oldCancel != nil {
			oldCancel()
		}
		if oldDone != nil {
			select {
			case <-oldDone:
			case <-time.After(3 * time.Second):
			}
		}

		s.mu.Lock()
		if !s.toolsReady {
			s.mu.Unlock()
			return errToolsNotReady
		}
	}

	s.activeTaskID++
	myTaskID := s.activeTaskID

	engine := "ytdlp"
	if req.Engine == "ffmpeg" {
		engine = "ffmpeg"
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	s.activeCancel = cancel
	s.activeEngine = engine
	s.activeDone = doneCh
	tools := s.tools
	cfg := s.config
	runFn := s.runDownloadFn
	if runFn == nil {
		runFn = RunDownload
	}
	s.downloadWG.Add(1)
	s.mu.Unlock()

	saveDir := req.SaveDir
	if strings.TrimSpace(saveDir) == "" {
		saveDir = cfg.SaveDir
	}

	opts := DownloadOptions{
		M3U8Url:        cleanURL,
		Title:          req.Title,
		OutputDir:      saveDir,
		Engine:         engine,
		RefererEnabled: req.RefererEnabled,
		UserAgent:      req.UserAgent,
		JA3:            req.JA3,
		Akamai:         req.Akamai,
		Concurrency:    DefaultConcurrency, // Always 1 thread
		YtDlpPath:      tools.YtDlpPath,
		FFmpegPath:     tools.FFmpegPath,
	}

	go func() {
		defer func() {
			close(doneCh)
			s.downloadWG.Done()
		}()

		outPath, err := runFn(ctx, opts, DownloadCallbacks{
			OnLog: func(tag, color, msg string) {
				s.AddLog(tag, color, msg)
			},
			OnProgress: func(p ProgressUpdate) {
				if s.isCurrentTask(myTaskID) {
					s.Broadcast("progress", p)
				}
			},
		})

		s.mu.Lock()
		isCurrent := s.activeTaskID == myTaskID
		if isCurrent {
			s.activeCancel = nil
			s.activeEngine = ""
			if s.activeDone == doneCh {
				s.activeDone = nil
			}
		} else if s.activeDone == doneCh {
			s.activeDone = nil
		}
		s.mu.Unlock()

		if !isCurrent {
			return
		}

		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.Broadcast("aborted", map[string]interface{}{"engine": engine})
			} else {
				s.Broadcast("error_event", map[string]interface{}{"engine": engine, "message": err.Error()})
			}
			return
		}

		s.Broadcast("done", map[string]interface{}{"engine": engine, "path": outPath})
	}()

	return nil
}

func (s *AppServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(embeddedIndexHTML)
}

func (s *AppServer) handleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.GetInitState())
}

func (s *AppServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var incoming AppConfig
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	s.UpdateConfig(incoming)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *AppServer) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	browseFn := s.browseFolderFn
	s.mu.Unlock()

	selected := ""
	if browseFn != nil {
		selected = browseFn()
	}
	s.SetSaveDir(selected)
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": selected})
}

func (s *AppServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok":           false,
			"error":        err.Error(),
			"activeEngine": s.ActiveEngine(),
		})
		return
	}
	if err := s.StartDownloadTask(req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errToolsNotReady) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]interface{}{
			"ok":           false,
			"error":        err.Error(),
			"activeEngine": s.ActiveEngine(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *AppServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.StopActiveDownload()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *AppServer) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *AppServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan sseMessage, 64)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
	}()

	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
