//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Noooste/azuretls-client"
)

type capturedLog struct {
	tag   string
	color string
	msg   string
}

type mockHLSClient struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error)
}

func (m *mockHLSClient) Get(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	return m.fn(ctx, call, rawURL, extraHeaders)
}

func (m *mockHLSClient) Close() {}

func (m *mockHLSClient) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestAzureTLSDefaultFingerprintCompatibility(t *testing.T) {
	if hlsNewClientHook == nil || hlsGetWithRetryHook == nil {
		t.Fatal("expected init() to register hlsNewClientHook and hlsGetWithRetryHook")
	}

	var (
		mu   sync.Mutex
		logs []capturedLog
	)

	client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      DefaultUserAgent,
		JA3:            DefaultJA3,
		Akamai:         DefaultAkamai,
		OnLog: func(tag, color, msg string) {
			mu.Lock()
			logs = append(logs, capturedLog{tag: tag, color: color, msg: msg})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("newAzureTLSClient returned unexpected error: %v", err)
	}
	defer func() {
		client.Close()
		client.Close() // Verify Close() is safely idempotent
	}()

	mu.Lock()
	defer mu.Unlock()
	var hasFingerprintLog bool
	for _, entry := range logs {
		if entry.tag == "[警告]" {
			t.Fatalf("unexpected warning log for default fingerprints: %+v", entry)
		}
		if entry.tag == "[指紋]" {
			hasFingerprintLog = true
		}
	}
	if !hasFingerprintLog {
		t.Fatal("expected a [指紋] confirmation log entry")
	}
}

func TestAzureTLSInvalidFingerprintFallback(t *testing.T) {
	var (
		mu   sync.Mutex
		logs []capturedLog
	)

	client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      DefaultUserAgent,
		JA3:            "invalid-ja3",
		Akamai:         "bad-akamai",
		OnLog: func(tag, color, msg string) {
			mu.Lock()
			logs = append(logs, capturedLog{tag: tag, color: color, msg: msg})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("expected graceful fallback without error, got %v", err)
	}
	defer client.Close()

	mu.Lock()
	defer mu.Unlock()
	var warnCount int
	var hasJA3Warn, hasAkamaiWarn bool
	for _, entry := range logs {
		if entry.tag == "[警告]" {
			warnCount++
			if strings.Contains(entry.msg, "JA3") {
				hasJA3Warn = true
			}
			if strings.Contains(entry.msg, "Akamai") {
				hasAkamaiWarn = true
			}
		}
	}
	if warnCount != 2 || !hasJA3Warn || !hasAkamaiWarn {
		t.Fatalf("expected 2 warning logs (JA3 + Akamai), got count=%d logs=%+v", warnCount, logs)
	}
}

func TestHLSClientHeaderSanitizationAndRefererToggle(t *testing.T) {
	var (
		mu          sync.Mutex
		lastHeaders http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastHeaders = r.Header.Clone()
		mu.Unlock()

		if rng := r.Header.Get("Range"); rng == "bytes=0-99" {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("partial-ok"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Run("RefererEnabled_True_WithCRLFInjection", func(t *testing.T) {
		client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
			RefererEnabled: true,
			Origin:         AnimeOrigin,
			Referer:        AnimeReferer,
			UserAgent:      "CustomUA/1.0\r\nX-Evil: 1",
		})
		if err != nil {
			t.Fatalf("newAzureTLSClient error: %v", err)
		}
		defer client.Close()

		resp, err := client.Get(context.Background(), srv.URL+"/test", nil)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		mu.Lock()
		hdr := lastHeaders
		mu.Unlock()

		if got := hdr.Get("Origin"); got != AnimeOrigin {
			t.Fatalf("Origin = %q, want %q", got, AnimeOrigin)
		}
		if got := hdr.Get("Referer"); got != AnimeReferer {
			t.Fatalf("Referer = %q, want %q", got, AnimeReferer)
		}
		ua := hdr.Get("User-Agent")
		if strings.ContainsAny(ua, "\r\n\x00") || ua != "CustomUA/1.0X-Evil: 1" {
			t.Fatalf("unexpected sanitized User-Agent: %q", ua)
		}
		if got := hdr.Get("X-Evil"); got != "" {
			t.Fatalf("CRLF injection created X-Evil header: %q", got)
		}
		if got := hdr.Get("Accept"); got != "*/*" {
			t.Fatalf("Accept = %q, want */*", got)
		}
		if got := hdr.Get("Sec-Fetch-Mode"); got != "cors" {
			t.Fatalf("Sec-Fetch-Mode = %q, want cors", got)
		}
	})

	t.Run("RefererEnabled_False", func(t *testing.T) {
		var warnCount atomic.Int32
		client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
			RefererEnabled: false,
			Origin:         AnimeOrigin,
			Referer:        AnimeReferer,
			UserAgent:      DefaultUserAgent,
			JA3:            "invalid-ja3",
			Akamai:         "bad-akamai",
			OnLog: func(tag, color, msg string) {
				if tag == "[警告]" {
					warnCount.Add(1)
				}
			},
		})
		if err != nil {
			t.Fatalf("newAzureTLSClient error: %v", err)
		}
		defer client.Close()

		if warnCount.Load() != 0 {
			t.Fatalf("expected 0 warnings when RefererEnabled=false, got %d", warnCount.Load())
		}

		resp, err := client.Get(context.Background(), srv.URL+"/no-referer", nil)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		mu.Lock()
		hdr := lastHeaders
		mu.Unlock()

		if got := hdr.Get("Origin"); got != "" {
			t.Fatalf("expected empty Origin when RefererEnabled=false, got %q", got)
		}
		if got := hdr.Get("Referer"); got != "" {
			t.Fatalf("expected empty Referer when RefererEnabled=false, got %q", got)
		}
	})

	t.Run("ExtraHeaders_Range_206", func(t *testing.T) {
		client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
			RefererEnabled: true,
			Origin:         AnimeOrigin,
			Referer:        AnimeReferer,
		})
		if err != nil {
			t.Fatalf("newAzureTLSClient error: %v", err)
		}
		defer client.Close()

		resp, err := client.Get(context.Background(), srv.URL+"/seg", map[string]string{
			"Range":    "bytes=0-99",
			"X-Custom": "val\r\nX-Injected: bad",
		})
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}

		mu.Lock()
		hdr := lastHeaders
		mu.Unlock()

		if got := hdr.Get("Range"); got != "bytes=0-99" {
			t.Fatalf("Range = %q, want bytes=0-99", got)
		}
		if got := hdr.Get("X-Injected"); got != "" {
			t.Fatalf("CRLF injection in extraHeaders created X-Injected: %q", got)
		}
		if got := hdr.Get("X-Custom"); got != "valX-Injected: bad" {
			t.Fatalf("X-Custom = %q, want %q", got, "valX-Injected: bad")
		}
	})
}

func TestHLSClientCookieJarPersistence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist.m3u8":
			http.SetCookie(w, &http.Cookie{
				Name:  "ani_auth",
				Value: "ok",
				Path:  "/",
			})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1,\n/seg1.ts\n"))
		case "/seg1.ts":
			c, err := r.Cookie("ani_auth")
			if err != nil || c.Value != "ok" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("seg1-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
	})
	if err != nil {
		t.Fatalf("newAzureTLSClient error: %v", err)
	}
	defer client.Close()

	playlistResp, err := client.Get(context.Background(), srv.URL+"/playlist.m3u8", nil)
	if err != nil {
		t.Fatalf("playlist request failed: %v", err)
	}
	if playlistResp.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", playlistResp.StatusCode)
	}

	segResp, err := client.Get(context.Background(), srv.URL+"/seg1.ts", nil)
	if err != nil {
		t.Fatalf("segment request failed: %v", err)
	}
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status = %d, want 200 (cookie jar failed to persist ani_auth)", segResp.StatusCode)
	}
	if string(segResp.Body) != "seg1-bytes" {
		t.Fatalf("segment body = %q, want seg1-bytes", string(segResp.Body))
	}
}

func TestHLSGetWithRetry(t *testing.T) {
	t.Run("Retry_503_429_Then_200", func(t *testing.T) {
		mock := &mockHLSClient{
			fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
				switch call {
				case 1:
					return &hlsResponse{StatusCode: 503, FinalURL: rawURL}, nil
				case 2:
					return &hlsResponse{StatusCode: 429, FinalURL: rawURL}, nil
				default:
					return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("segment-data")}, nil
				}
			},
		}

		var (
			mu   sync.Mutex
			logs []capturedLog
		)
		resp, err := hlsGetWithRetry(context.Background(), mock, "https://example.com/seg.ts", nil, 4, func(tag, color, msg string) {
			mu.Lock()
			logs = append(logs, capturedLog{tag: tag, color: color, msg: msg})
			mu.Unlock()
		})
		if err != nil {
			t.Fatalf("expected retry to succeed, got err=%v", err)
		}
		if resp == nil || resp.StatusCode != 200 || string(resp.Body) != "segment-data" {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if mock.CallCount() != 3 {
			t.Fatalf("calls = %d, want 3", mock.CallCount())
		}

		mu.Lock()
		defer mu.Unlock()
		if len(logs) != 2 {
			t.Fatalf("expected 2 warning logs for 2 retries, got %d: %+v", len(logs), logs)
		}
	})

	t.Run("FailFast_403_And_404", func(t *testing.T) {
		for _, code := range []int{403, 404} {
			mock := &mockHLSClient{
				fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
					return &hlsResponse{StatusCode: code, FinalURL: rawURL}, nil
				},
			}
			_, err := hlsGetWithRetry(context.Background(), mock, "https://example.com/seg.ts", nil, 4, nil)
			if err == nil {
				t.Fatalf("expected error for HTTP %d", code)
			}
			if mock.CallCount() != 1 {
				t.Fatalf("HTTP %d should fail fast with 1 call, got %d", code, mock.CallCount())
			}
			if code == 403 && !strings.Contains(err.Error(), "403") {
				t.Fatalf("expected error message to contain 403, got %v", err)
			}
			if code == 404 && !strings.Contains(err.Error(), "404") {
				t.Fatalf("expected error message to contain 404, got %v", err)
			}
		}
	})

	t.Run("CancelDuringRetryBackoff", func(t *testing.T) {
		mock := &mockHLSClient{
			fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
				return &hlsResponse{StatusCode: 503, FinalURL: rawURL}, nil
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)

		start := time.Now()
		_, err := hlsGetWithRetry(ctx, mock, "https://example.com/seg.ts", nil, 4, nil)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if elapsed > 200*time.Millisecond {
			t.Fatalf("expected cancellation within 200ms, took %v", elapsed)
		}
		if mock.CallCount() != 1 {
			t.Fatalf("calls = %d, want 1", mock.CallCount())
		}
	})

	t.Run("ZeroMaxRetriesDisablesRetry", func(t *testing.T) {
		mock := &mockHLSClient{
			fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
				return &hlsResponse{StatusCode: 503, FinalURL: rawURL}, nil
			},
		}
		_, err := hlsGetWithRetry(context.Background(), mock, "https://example.com/seg.ts?token=secret123", nil, 0, nil)
		if err == nil {
			t.Fatal("expected error on 503 when maxRetries=0")
		}
		if mock.CallCount() != 1 {
			t.Fatalf("calls = %d, want 1 when maxRetries=0", mock.CallCount())
		}
		if strings.Contains(err.Error(), "secret123") {
			t.Fatalf("error leaked token query parameter: %v", err)
		}
	})

	t.Run("RedactURLTokenInLogsAndErrors", func(t *testing.T) {
		mock := &mockHLSClient{
			fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
				if call == 1 {
					return &hlsResponse{StatusCode: 503, FinalURL: rawURL}, nil
				}
				return &hlsResponse{StatusCode: 403, FinalURL: rawURL}, nil
			},
		}
		var logs []string
		_, err := hlsGetWithRetry(context.Background(), mock, "https://cdn.example.com/v1/seg.ts?token=secret123&exp=999", nil, 2, func(tag, color, msg string) {
			logs = append(logs, tag+" "+msg)
		})
		if err == nil {
			t.Fatal("expected 403 error")
		}
		if strings.Contains(err.Error(), "secret123") || !strings.Contains(err.Error(), "?<redacted>") {
			t.Fatalf("error did not properly redact URL query: %v", err)
		}
		for _, entry := range logs {
			if strings.Contains(entry, "secret123") || !strings.Contains(entry, "?<redacted>") {
				t.Fatalf("log entry leaked token or missed <redacted>: %q", entry)
			}
		}
	})
}

func TestHLSClientRedactURLForLog(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "https://user:pass@cdn.example.com/a.m3u8?token=secret#frag", want: "https://cdn.example.com/a.m3u8?<redacted>"},
		{in: "https://cdn.example.com/a.ts", want: "https://cdn.example.com/a.ts"},
	}
	for _, tc := range cases {
		if got := redactURLForLog(tc.in); got != tc.want {
			t.Fatalf("redactURLForLog(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHLSClientNoDuplicateHeadersOnDirectAnd302Redirect(t *testing.T) {
	var (
		mu       sync.Mutex
		recorded []http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, r.Header.Clone())
		mu.Unlock()

		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("final-ok"))
	}))
	defer srv.Close()

	client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      "TestAgent/2.0",
	})
	if err != nil {
		t.Fatalf("newAzureTLSClient error: %v", err)
	}
	defer client.Close()

	resp, err := client.Get(context.Background(), srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("Get with redirect failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "final-ok" {
		t.Fatalf("unexpected response after redirect: %+v", resp)
	}
	if !strings.HasSuffix(resp.FinalURL, "/final") {
		t.Fatalf("FinalURL = %q, want suffix /final", resp.FinalURL)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 2 {
		t.Fatalf("expected 2 HTTP hops (/start -> /final), got %d", len(recorded))
	}
	for hopIdx, hdr := range recorded {
		for _, key := range []string{"Origin", "Referer", "User-Agent"} {
			vals := hdr.Values(key)
			if len(vals) != 1 {
				t.Fatalf("hop %d header %s had %d values %v, want exactly 1", hopIdx, key, len(vals), vals)
			}
		}
	}
}

func TestNewAzureTLSClientDefaultJA3WithDefaultAkamai(t *testing.T) {
	var logs []capturedLog
	client, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      DefaultUserAgent,
		JA3:            DefaultJA3,
		Akamai:         DefaultAkamai,
		OnLog: func(tag, color, msg string) {
			logs = append(logs, capturedLog{tag: tag, color: color, msg: msg})
		},
	})
	if err != nil {
		t.Fatalf("newAzureTLSClient failed: %v", err)
	}
	defer client.Close()

	az := client.(*azureHLSClient)
	if az.customJA3Applied {
		t.Fatal("expected customJA3Applied=false when JA3 is DefaultJA3 empty string")
	}

	var sawFingerprintLog bool
	for _, entry := range logs {
		if entry.tag == "[警告]" {
			t.Fatalf("unexpected warning log with DefaultJA3+DefaultAkamai: %+v", entry)
		}
		if entry.tag == "[指紋]" && strings.Contains(entry.msg, "TLS=Chrome 預設") {
			sawFingerprintLog = true
		}
	}
	if !sawFingerprintLog {
		t.Fatalf("expected fingerprint log mentioning TLS=Chrome 預設, got %+v", logs)
	}
}

func TestIsTLSHandshakeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "NilError", err: nil, want: false},
		{name: "RemoteIllegalParam", err: errors.New("remote error: tls: illegal parameter"), want: true},
		{name: "HandshakeFailure", err: errors.New("tls: handshake failure"), want: true},
		{name: "UnsupportedExtension", err: errors.New("unsupported_extension"), want: true},
		{name: "ProtocolVersion", err: errors.New("protocol_version"), want: true},
		{name: "WrappedURLTLS", err: &url.Error{Op: "Get", URL: "https://example.com/a.ts", Err: errors.New("remote error: tls: handshake failure")}, want: true},
		{name: "PlainIOEOF", err: io.EOF, want: false},
		{name: "TimeoutWithAlertInURLPath", err: &url.Error{Op: "Get", URL: "https://example.com/alert/seg.ts", Err: errors.New("dial tcp: i/o timeout")}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTLSHandshakeError(tc.err); got != tc.want {
				t.Fatalf("isTLSHandshakeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestHLSGetWithRetryFailFastOnTLSHandshakeError(t *testing.T) {
	mock := &mockHLSClient{
		fn: func(ctx context.Context, call int, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			return nil, errors.New("remote error: tls: handshake failure")
		},
	}

	_, err := hlsGetWithRetry(context.Background(), mock, "https://example.com/seg.ts?token=abc", nil, 4, nil)
	if err == nil || !strings.Contains(err.Error(), "TLS 握手失敗") {
		t.Fatalf("expected wrapped TLS handshake failure, got %v", err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("calls = %d, want 1 (Fail Fast without backoff retries)", mock.CallCount())
	}
}

func TestAzureHLSClientAutoDowngradeOnTLSHandshakeFailure(t *testing.T) {
	var logs []capturedLog
	rawClient, err := newAzureTLSClient(context.Background(), hlsClientConfig{
		RefererEnabled: true,
		Origin:         AnimeOrigin,
		Referer:        AnimeReferer,
		UserAgent:      DefaultUserAgent,
		JA3:            legacyDefaultJA3,
		Akamai:         DefaultAkamai,
		OnLog: func(tag, color, msg string) {
			logs = append(logs, capturedLog{tag: tag, color: color, msg: msg})
		},
	})
	if err != nil {
		t.Fatalf("newAzureTLSClient failed: %v", err)
	}
	defer rawClient.Close()

	az := rawClient.(*azureHLSClient)
	if !az.customJA3Applied {
		t.Fatal("expected customJA3Applied=true with legacyDefaultJA3")
	}
	initialSess := az.session

	var doCalls int
	var sessionsUsed []*azuretls.Session
	az.doFn = func(sess *azuretls.Session, req *azuretls.Request) (*azuretls.Response, error) {
		doCalls++
		sessionsUsed = append(sessionsUsed, sess)
		if doCalls == 1 {
			return nil, errors.New("remote error: tls: handshake failure")
		}
		return &azuretls.Response{
			StatusCode: http.StatusOK,
			Url:        "https://example.com/playlist.m3u8",
			Body:       []byte("#EXTM3U\n"),
		}, nil
	}

	resp, err := az.Get(context.Background(), "https://example.com/playlist.m3u8", nil)
	if err != nil {
		t.Fatalf("expected auto-downgraded retry to succeed, got %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "#EXTM3U\n" {
		t.Fatalf("unexpected response after downgrade: %+v", resp)
	}
	if doCalls != 2 {
		t.Fatalf("doCalls = %d, want 2", doCalls)
	}
	if sessionsUsed[0] != initialSess || sessionsUsed[1] == initialSess || sessionsUsed[1] == nil {
		t.Fatalf("expected session rebuild on auto-downgrade: sess0=%p sess1=%p initial=%p", sessionsUsed[0], sessionsUsed[1], initialSess)
	}
	if az.customJA3Applied {
		t.Fatal("expected customJA3Applied=false after downgrade")
	}

	var sawDowngradeWarn bool
	for _, entry := range logs {
		if entry.tag == "[警告]" && strings.Contains(entry.msg, "自動降級") {
			sawDowngradeWarn = true
		}
	}
	if !sawDowngradeWarn {
		t.Fatalf("expected auto-downgrade warning log, got %+v", logs)
	}
}
