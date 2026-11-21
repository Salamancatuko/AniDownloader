//go:build windows

package main

import (
	"context"
	"time"
)

// NativeHLSEngineVersion is reported in the UI sidebar badge after yt-dlp removal.
const NativeHLSEngineVersion = "v2.0.0"

// hlsResponse represents a buffered HTTP response returned by hlsHTTPClient.
type hlsResponse struct {
	StatusCode int
	FinalURL   string
	Body       []byte
}

// hlsHTTPClient abstracts fingerprinted HTTP operations for testability.
type hlsHTTPClient interface {
	Get(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error)
	Close()
}

// hlsClientConfig defines TLS/HTTP2 fingerprint and anti-leech header settings.
type hlsClientConfig struct {
	RefererEnabled bool
	Origin         string
	Referer        string
	UserAgent      string
	JA3            string
	Akamai         string
	Timeout        time.Duration
	OnLog          func(tag, color, msg string)
}

// hlsKeyInfo describes encryption metadata from #EXT-X-KEY.
type hlsKeyInfo struct {
	Method string // "NONE" or "AES-128"
	URI    string // Resolved absolute URL
	IV     []byte // 16-byte explicit IV, or nil when derived from media sequence
}

// hlsByteRange describes #EXT-X-BYTERANGE.
type hlsByteRange struct {
	Length int64
	Offset int64
}

// hlsMapInfo describes fMP4/CMAF initialization segment from #EXT-X-MAP.
type hlsMapInfo struct {
	URI       string // Resolved absolute URL
	ByteRange *hlsByteRange
}

// hlsSegment represents a single downloadable media segment.
type hlsSegment struct {
	Index         int
	SeqNum        uint64
	URI           string // Resolved absolute URL
	Duration      float64
	Key           *hlsKeyInfo
	Map           *hlsMapInfo
	ByteRange     *hlsByteRange
	Discontinuity bool
}

// hlsVariant represents a stream entry inside a Master M3U8 playlist.
type hlsVariant struct {
	URI          string // Resolved absolute URL
	Bandwidth    int64
	AvgBandwidth int64
	Width        int
	Height       int
	Resolution   string
	Codecs       string
	AudioGroup   string
	AudioURI     string // Resolved absolute URL of matched separate audio track, if any
}

// hlsPlaylist represents a parsed Master or Media M3U8 playlist.
type hlsPlaylist struct {
	IsMaster         bool
	IsVOD            bool
	MediaSequence    uint64
	TotalDuration    float64
	HasDiscontinuity bool
	IsFMP4           bool
	Variants         []hlsVariant
	Segments         []hlsSegment
}

// Module wiring hooks populated by each workstream's init() function.
// This allows WF-1, WF-2, WF-3, and WF-5 to compile and pass unit tests independently.
var (
	// Registered by WF-1 (hls_client.go)
	hlsNewClientHook    func(ctx context.Context, cfg hlsClientConfig) (hlsHTTPClient, error)
	hlsGetWithRetryHook func(ctx context.Context, client hlsHTTPClient, rawURL string, extraHeaders map[string]string, maxRetries int, onLog func(tag, color, msg string)) (*hlsResponse, error)

	// Registered by WF-2 (hls_parser.go)
	hlsParseM3U8Hook     func(content string, baseURL string) (*hlsPlaylist, error)
	hlsSelectBestHook    func(variants []hlsVariant) (*hlsVariant, error)
	hlsDecryptAES128Hook func(ciphertext, key, iv []byte) ([]byte, error)
	hlsBuildSeqIVHook    func(seq uint64) []byte

	// Registered by WF-3 (hls_downloader.go)
	hlsRunNativeDownloadFn func(ctx context.Context, opt DownloadOptions, cb DownloadCallbacks) (string, error)
)
