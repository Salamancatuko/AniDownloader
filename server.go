package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed assets/index.html
var embeddedIndexHTML []byte

//go:embed assets/logo.svg
var embeddedLogoSVG []byte

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
	mu           sync.Mutex
	config       AppConfig
	tools        ToolInfo
	toolsReady   bool
	toolsFailed  bool
	logs         []LogEntry
	activeCancel context.CancelFunc
	activeEngine string

	sseMu      sync.Mutex
	sseClients map[chan sseMessage]struct{}

	BrowseFolderFn func() string

	listener net.Listener
	server   *http.Server
	BaseURL  string
}

func NewAppServer() *AppServer {
	return &AppServer{
		config:     LoadConfig(),
		sseClients: make(map[chan sseMessage]struct{}),
	}
}

func (s *AppServer) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.listener = ln
	s.BaseURL = fmt.Sprintf("http://%s", ln.Addr().String())

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/logo.svg", s.handleLogo)
	mux.HandleFunc("/api/init", s.handleInit)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/browse", s.handleBrowse)
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/api/events", s.handleEvents)

	s.server = &http.Server{Handler: mux}
	go func() {
		_ = s.server.Serve(ln)
	}()
	return nil
}

func (s *AppServer) Shutdown() {
	s.StopActiveDownload()
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.server.Shutdown(ctx)
	}
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
	msg := sseMessage{Event: event, Data: string(b)}

	s.sseMu.Lock()
	for ch := range s.sseClients {
		select {
		case ch <- msg:
		default:
			// Drop if client is slow so backend never blocks
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

func (s *AppServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(embeddedIndexHTML)
}

func (s *AppServer) handleLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(embeddedLogoSVG)
}

func (s *AppServer) handleInit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	resp := map[string]interface{}{
		"config":        s.config,
		"toolsReady":    s.toolsReady,
		"toolsFailed":   s.toolsFailed,
		"ffmpegVersion": s.tools.FFmpegVersion,
		"ytdlpVersion":  s.tools.YtDlpVersion,
		"activeEngine":  s.activeEngine,
		"logs":          append([]LogEntry(nil), s.logs...),
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
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
	s.mu.Lock()
	if incoming.Lang == "zh-TW" || incoming.Lang == "zh-CN" || incoming.Lang == "en" {
		s.config.Lang = incoming.Lang
	}
	if incoming.Opacity >= 15 && incoming.Opacity <= 85 {
		s.config.Opacity = incoming.Opacity
	}
	if incoming.Blur >= 0 && incoming.Blur <= 40 {
		s.config.Blur = incoming.Blur
	}
	s.config.RefererEnabled = incoming.RefererEnabled
	if incoming.SaveDir != "" {
		s.config.SaveDir = incoming.SaveDir
	}
	cfgCopy := s.config
	s.mu.Unlock()

	SaveConfig(cfgCopy)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *AppServer) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	selected := ""
	if s.BrowseFolderFn != nil {
		selected = s.BrowseFolderFn()
	}
	if selected != "" {
		s.mu.Lock()
		s.config.SaveDir = selected
		cfgCopy := s.config
		s.mu.Unlock()
		SaveConfig(cfgCopy)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": selected})
}

type downloadRequest struct {
	URL            string `json:"url"`
	Title          string `json:"title"`
	SaveDir        string `json:"saveDir"`
	Engine         string `json:"engine"`
	RefererEnabled bool   `json:"refererEnabled"`
	Concurrency    int    `json:"concurrency"`
}

func (s *AppServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	s.mu.Lock()
	if !s.toolsReady {
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"ok": false, "error": "內建工具尚未準備就緒，請稍候..."})
		return
	}
	if s.activeCancel != nil {
		s.activeCancel()
		s.activeCancel = nil
	}
	engine := "ytdlp"
	if req.Engine == "ffmpeg" {
		engine = "ffmpeg"
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.activeCancel = cancel
	s.activeEngine = engine
	tools := s.tools
	s.mu.Unlock()

	opts := DownloadOptions{
		M3U8Url:        req.URL,
		Title:          req.Title,
		OutputDir:      req.SaveDir,
		Engine:         engine,
		RefererEnabled: req.RefererEnabled,
		Concurrency:    DefaultConcurrency, // Always 1 thread
		YtDlpPath:      tools.YtDlpPath,
		FFmpegPath:     tools.FFmpegPath,
	}

	go func() {
		outPath, err := RunDownload(ctx, opts, DownloadCallbacks{
			OnLog: func(tag, color, msg string) {
				s.AddLog(tag, color, msg)
			},
			OnProgress: func(p ProgressUpdate) {
				s.Broadcast("progress", p)
			},
		})

		s.mu.Lock()
		if s.activeEngine == engine {
			s.activeCancel = nil
			s.activeEngine = ""
		}
		s.mu.Unlock()

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

func (s *AppServer) handleEvents(w http.ResponseWriter, r *http.Request) {
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

	// Send initial comment to flush headers immediately
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
