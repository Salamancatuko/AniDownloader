//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Noooste/azuretls-client"
)

func init() {
	hlsNewClientHook = newAzureTLSClient
	hlsGetWithRetryHook = hlsGetWithRetry
}

type azureHLSClient struct {
	mu               sync.Mutex
	session          *azuretls.Session
	baseCtx          context.Context
	cfg              hlsClientConfig
	userAgent        string
	origin           string
	referer          string
	customJA3Applied bool
	closed           bool
	closeOnce        sync.Once
	doFn             func(sess *azuretls.Session, req *azuretls.Request) (*azuretls.Response, error)
}

// isTLSHandshakeError reports whether err represents a TLS/uTLS handshake rejection
// or extension/alert failure rather than a transient HTTP transfer error.
func isTLSHandshakeError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr != nil {
		if uerr.Err != nil {
			msg = uerr.Err.Error()
		} else if uerr.URL != "" {
			msg = strings.ReplaceAll(msg, uerr.URL, "")
		}
	}

	lower := strings.ToLower(msg)
	for _, pat := range []string{
		"tls: handshake failure",
		"remote error: tls:",
		"local error: tls:",
		"handshake failure",
		"unsupported_extension",
		"protocol_version",
		"tls alert",
		"utls",
		"tls: ",
		"alert",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// sanitizeHLSHeaderValue removes CR, LF, and NUL bytes and trims surrounding whitespace
// to prevent HTTP header CRLF injection without coupling to downloader.go.
func sanitizeHLSHeaderValue(s string) string {
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

func buildAzureSession(ctx context.Context, cfg hlsClientConfig, ua, origin, referer string, allowCustomJA3, emitLog bool) (*azuretls.Session, bool) {
	if ctx == nil {
		ctx = context.Background()
	}

	session := azuretls.NewSessionWithContext(ctx)
	session.Browser = azuretls.Chrome

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	session.SetTimeout(timeout)
	session.UserAgent = ua

	logFn := func(tag, color, msg string) {
		if emitLog && cfg.OnLog != nil {
			cfg.OnLog(tag, color, msg)
		}
	}

	customJA3Applied := false
	if cfg.RefererEnabled {
		tlsDesc := "Chrome 預設"
		if allowCustomJA3 {
			if ja3 := strings.TrimSpace(cfg.JA3); ja3 != "" {
				if err := session.ApplyJa3(ja3, azuretls.Chrome); err != nil {
					logFn("[警告]", "#fbbf24", fmt.Sprintf("JA3 指紋格式無效，已安全降級為內建 Chrome TLS 指紋: %v", err))
				} else {
					tlsDesc = "自定義 JA3"
					customJA3Applied = true
				}
			}
		}

		h2Desc := "Chrome 預設"
		if akamai := strings.TrimSpace(cfg.Akamai); akamai != "" {
			if err := session.ApplyHTTP2(akamai); err != nil {
				logFn("[警告]", "#fbbf24", fmt.Sprintf("Akamai HTTP/2 指紋格式無效，已安全降級為預設設定: %v", err))
			} else {
				h2Desc = "自定義 Akamai"
			}
		}

		logFn("[指紋]", "#38bdf8", fmt.Sprintf("已啟用防盜鏈與指紋偽裝 | TLS=%s | HTTP/2=%s | Origin=%s | Referer=%s", tlsDesc, h2Desc, origin, referer))
	}

	return session, customJA3Applied
}

func newAzureTLSClient(ctx context.Context, cfg hlsClientConfig) (hlsHTTPClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	ua := sanitizeHLSHeaderValue(cfg.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	origin := sanitizeHLSHeaderValue(cfg.Origin)
	referer := sanitizeHLSHeaderValue(cfg.Referer)

	session, customJA3Applied := buildAzureSession(ctx, cfg, ua, origin, referer, true, true)

	return &azureHLSClient{
		session:          session,
		baseCtx:          ctx,
		cfg:              cfg,
		userAgent:        ua,
		origin:           origin,
		referer:          referer,
		customJA3Applied: customJA3Applied,
		doFn: func(sess *azuretls.Session, req *azuretls.Request) (*azuretls.Response, error) {
			return sess.Do(req)
		},
	}, nil
}

func (c *azureHLSClient) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		sess := c.session
		c.session = nil
		c.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
	})
}

func (c *azureHLSClient) downgradeToNativeTLS() (*azuretls.Session, bool) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false
	}
	if !c.customJA3Applied {
		sess := c.session
		c.mu.Unlock()
		return sess, false
	}

	newSess, _ := buildAzureSession(c.baseCtx, c.cfg, c.userAgent, c.origin, c.referer, false, false)
	if c.session != nil {
		jar := c.session.CookieJar
		c.session.Close()
		if jar != nil {
			newSess.CookieJar = jar
		}
	}
	c.session = newSess
	c.customJA3Applied = false
	c.mu.Unlock()
	return newSess, true
}

func (c *azureHLSClient) Get(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	headers := make(azuretls.OrderedHeaders, 0, 8+len(extraHeaders))
	if c.cfg.RefererEnabled {
		if c.origin != "" {
			headers.Set("Origin", c.origin)
		}
		if c.referer != "" {
			headers.Set("Referer", c.referer)
		}
		headers.Set("User-Agent", c.userAgent)
		headers.Set("Accept", "*/*")
		headers.Set("Accept-Language", "zh-TW,zh;q=0.9,en-US;q=0.8,en;q=0.7")
		headers.Set("Sec-Fetch-Dest", "empty")
		headers.Set("Sec-Fetch-Mode", "cors")
		headers.Set("Sec-Fetch-Site", "cross-site")
	}

	if len(extraHeaders) > 0 {
		keys := make([]string, 0, len(extraHeaders))
		for k := range extraHeaders {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			cleanKey := sanitizeHLSHeaderValue(k)
			cleanVal := sanitizeHLSHeaderValue(extraHeaders[k])
			if cleanKey != "" && cleanVal != "" {
				headers.Set(cleanKey, cleanVal)
			}
		}
	}

	buildReq := func() *azuretls.Request {
		req := &azuretls.Request{
			Method: "GET",
			Url:    rawURL,
		}
		if len(headers) > 0 {
			req.OrderedHeaders = headers
			// Also populate Header so azuretls redirect copier preserves headers across 301/302 hops.
			req.Header = headers.ToHeader()
		}
		req.SetContext(ctx)
		return req
	}

	c.mu.Lock()
	if c.closed || c.session == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("HTTP 客戶端已關閉")
	}
	sess := c.session
	usedCustomJA3 := c.customJA3Applied
	doFn := c.doFn
	c.mu.Unlock()

	if doFn == nil {
		doFn = func(s *azuretls.Session, r *azuretls.Request) (*azuretls.Response, error) {
			return s.Do(r)
		}
	}

	resp, err := doFn(sess, buildReq())
	if err != nil && ctx.Err() == nil && usedCustomJA3 && isTLSHandshakeError(err) {
		retrySess, performed := c.downgradeToNativeTLS()
		if performed && c.cfg.OnLog != nil {
			c.cfg.OnLog("[警告]", "#fbbf24", "自定義 JA3 遭遠端伺服器拒絕 TLS 握手，已自動降級為內建 Chrome TLS 1.3 指紋並重試")
		}
		if retrySess != nil {
			resp, err = doFn(retrySess, buildReq())
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("空白的 HTTP 回應")
	}

	finalURL := strings.TrimSpace(resp.Url)
	if finalURL == "" && resp.HttpResponse != nil && resp.HttpResponse.Request != nil && resp.HttpResponse.Request.URL != nil {
		finalURL = resp.HttpResponse.Request.URL.String()
	}
	if finalURL == "" {
		finalURL = rawURL
	}

	return &hlsResponse{
		StatusCode: resp.StatusCode,
		FinalURL:   finalURL,
		Body:       resp.Body,
	}, nil
}

// redactURLForLog removes credentials, fragments, and query string values from URLs
// before writing them into console logs or user-facing error messages.
func redactURLForLog(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || u == nil {
		if q := strings.IndexByte(trimmed, '?'); q >= 0 {
			return trimmed[:q] + "?<redacted>"
		}
		return trimmed
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = "<redacted>"
	}
	return u.String()
}

func hlsGetWithRetry(ctx context.Context, client hlsHTTPClient, rawURL string, extraHeaders map[string]string, maxRetries int, onLog func(tag, color, msg string)) (*hlsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return nil, fmt.Errorf("HTTP 客戶端未初始化")
	}
	if maxRetries < 0 {
		maxRetries = 4
	}

	safeURL := redactURLForLog(rawURL)
	logFn := func(tag, color, msg string) {
		if onLog != nil {
			onLog(tag, color, msg)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if attempt > 0 {
			shift := attempt - 1
			if shift > 5 {
				shift = 5
			}
			delay := time.Duration(1<<shift) * time.Second
			logFn("[警告]", "#fbbf24", fmt.Sprintf("HTTP 請求失敗 (%v)，等待 %d 秒後進行第 %d/%d 次重試: %s", lastErr, int(delay.Seconds()), attempt, maxRetries, safeURL))

			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		resp, err := client.Get(ctx, rawURL, extraHeaders)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.Canceled) {
				return nil, context.Canceled
			}
			if isTLSHandshakeError(err) {
				return nil, fmt.Errorf("TLS 握手失敗：伺服器拒絕當前 TLS 指紋（建議於設定中清空自定義 JA3 以使用原生 Chrome 指紋）(%s): %w", safeURL, err)
			}
			lastErr = err
			continue
		}
		if resp == nil {
			lastErr = fmt.Errorf("空白的 HTTP 回應")
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		switch resp.StatusCode {
		case 403:
			return nil, fmt.Errorf("HTTP 403 存取被拒：請檢查防盜鏈設定（Referer/指紋）或連結 Token 是否已過期 (%s)", safeURL)
		case 404, 410:
			return nil, fmt.Errorf("HTTP %d 資源不存在：串流分片不存在或連結已失效 (%s)", resp.StatusCode, safeURL)
		case 400, 401:
			return nil, fmt.Errorf("HTTP %d 請求無效或未授權：請檢查串流連結是否有效 (%s)", resp.StatusCode, safeURL)
		case 408, 429, 500, 502, 503, 504:
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		default:
			return nil, fmt.Errorf("HTTP %d 請求失敗 (%s)", resp.StatusCode, safeURL)
		}
	}

	return nil, fmt.Errorf("HTTP 請求於重試 %d 次後仍然失敗 (%s): %w", maxRetries, safeURL, lastErr)
}
