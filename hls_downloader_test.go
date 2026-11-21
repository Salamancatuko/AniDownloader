//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDownloadClient struct {
	mu       sync.Mutex
	requests []string
	headers  []map[string]string
	closed   atomic.Bool
	getFn    func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error)
}

func (c *fakeDownloadClient) Get(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, rawURL)
	var hdrCopy map[string]string
	if extraHeaders != nil {
		hdrCopy = make(map[string]string, len(extraHeaders))
		for k, v := range extraHeaders {
			hdrCopy[k] = v
		}
	}
	c.headers = append(c.headers, hdrCopy)
	c.mu.Unlock()
	return c.getFn(ctx, rawURL, extraHeaders)
}

func (c *fakeDownloadClient) Close() {
	c.closed.Store(true)
}

func baseMockDeps(client hlsHTTPClient, playlists map[string]*hlsPlaylist) hlsRunnerDeps {
	return hlsRunnerDeps{
		newClient: func(ctx context.Context, cfg hlsClientConfig) (hlsHTTPClient, error) {
			return client, nil
		},
		getWithRetry: func(ctx context.Context, cl hlsHTTPClient, rawURL string, extraHeaders map[string]string, maxRetries int, onLog func(tag, color, msg string)) (*hlsResponse, error) {
			return cl.Get(ctx, rawURL, extraHeaders)
		},
		parseM3U8: func(content string, baseURL string) (*hlsPlaylist, error) {
			if pl, ok := playlists[content]; ok {
				return pl, nil
			}
			if pl, ok := playlists[baseURL]; ok {
				return pl, nil
			}
			return nil, fmt.Errorf("unexpected playlist key %q / %q", content, baseURL)
		},
		selectBest: func(variants []hlsVariant) (*hlsVariant, error) {
			if len(variants) == 0 {
				return nil, errors.New("empty variants")
			}
			best := variants[len(variants)-1]
			return &best, nil
		},
		decryptAES128: func(ciphertext, key, iv []byte) ([]byte, error) {
			// Reversible XOR test cipher so unit tests do not depend on WF-2
			out := make([]byte, len(ciphertext))
			for i, b := range ciphertext {
				out[i] = b ^ key[i%len(key)] ^ iv[i%len(iv)]
			}
			return out, nil
		},
		buildSeqIV: func(seq uint64) []byte {
			iv := make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], seq)
			return iv
		},
	}
}

func assertNoTempDirs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) failed: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".anidl_tmp_") {
			t.Fatalf("leftover temporary directory found: %s", entry.Name())
		}
	}
}

func extractInputPathsFromArgs(args []string) []string {
	var inputs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" {
			inputs = append(inputs, args[i+1])
		}
	}
	return inputs
}

func TestNativeHLSStandardTSSingleTrack(t *testing.T) {
	if hlsRunNativeDownloadFn == nil {
		t.Fatal("expected init() to register hlsRunNativeDownloadFn")
	}

	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/ep01/playlist.m3u8"
	pl := &hlsPlaylist{
		IsVOD: true,
		Segments: []hlsSegment{
			{Index: 0, SeqNum: 0, URI: "https://cdn.example.com/ep01/seg0.ts", Duration: 4},
			{Index: 1, SeqNum: 1, URI: "https://cdn.example.com/ep01/seg1.ts", Duration: 4},
			{Index: 2, SeqNum: 2, URI: "https://cdn.example.com/ep01/seg2.ts", Duration: 4},
		},
	}

	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case playlistURL:
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("VOD_TS")}, nil
			case "https://cdn.example.com/ep01/seg0.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("[seg0]")}, nil
			case "https://cdn.example.com/ep01/seg1.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("[seg1]")}, nil
			case "https://cdn.example.com/ep01/seg2.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("[seg2]")}, nil
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	var (
		capturedArgs  []string
		capturedTrack []byte
		progresses    []ProgressUpdate
		mu            sync.Mutex
	)
	deps := baseMockDeps(client, map[string]*hlsPlaylist{"VOD_TS": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		capturedArgs = append([]string(nil), args...)
		inputs := extractInputPathsFromArgs(args)
		if len(inputs) != 1 {
			return fmt.Errorf("expected 1 input, got %v", inputs)
		}
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		capturedTrack = b
		outPath := args[len(args)-1]
		return os.WriteFile(outPath, append([]byte("MP4:"), b...), 0644)
	}

	// Verify nil-hook guard does not panic
	if _, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{M3U8Url: playlistURL}, DownloadCallbacks{}, hlsRunnerDeps{}); err == nil {
		t.Fatal("expected descriptive error when deps hooks are nil")
	}

	outPath, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:        playlistURL,
		Title:          "Episode 01",
		OutputDir:      outDir,
		RefererEnabled: true,
		FFmpegPath:     `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{
		OnProgress: func(p ProgressUpdate) {
			mu.Lock()
			progresses = append(progresses, p)
			mu.Unlock()
		},
	}, deps)
	if err != nil {
		t.Fatalf("runNativeHLSDownloadWithDeps failed: %v", err)
	}
	if !client.closed.Load() {
		t.Fatal("expected HTTP client to be closed")
	}

	expectedReqs := []string{
		playlistURL,
		"https://cdn.example.com/ep01/seg0.ts",
		"https://cdn.example.com/ep01/seg1.ts",
		"https://cdn.example.com/ep01/seg2.ts",
	}
	if !slices.Equal(client.requests, expectedReqs) {
		t.Fatalf("requests = %v, want %v", client.requests, expectedReqs)
	}
	if string(capturedTrack) != "[seg0][seg1][seg2]" {
		t.Fatalf("capturedTrack = %q, want [seg0][seg1][seg2]", string(capturedTrack))
	}
	if !slices.Contains(capturedArgs, "-bsf:a") || !slices.Contains(capturedArgs, "aac_adtstoasc") {
		t.Fatalf("expected -bsf:a aac_adtstoasc for TS input, got %v", capturedArgs)
	}
	if !slices.Contains(capturedArgs, "-movflags") || !slices.Contains(capturedArgs, "+faststart") {
		t.Fatalf("expected -movflags +faststart, got %v", capturedArgs)
	}

	finalData, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", outPath, err)
	}
	if string(finalData) != "MP4:[seg0][seg1][seg2]" {
		t.Fatalf("unexpected final output content: %q", string(finalData))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(progresses) < 3 {
		t.Fatalf("expected progress updates including merging and 100%%, got %+v", progresses)
	}
	prevPct := -1.0
	var sawMerging bool
	for _, p := range progresses {
		if p.Engine != "hls" {
			t.Fatalf("expected Engine=hls, got %q", p.Engine)
		}
		if p.Percent < prevPct {
			t.Fatalf("progress went backwards: %v -> %v", prevPct, p.Percent)
		}
		prevPct = p.Percent
		if p.Merging && p.Percent == 99.0 {
			sawMerging = true
		}
	}
	if !sawMerging || progresses[len(progresses)-1].Percent != 100.0 {
		t.Fatalf("expected 99%% Merging and final 100%%, got %+v", progresses)
	}

	assertNoTempDirs(t, outDir)
}

func TestNativeHLSAES128KeyCache(t *testing.T) {
	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/enc/playlist.m3u8"
	keyURL := "https://cdn.example.com/enc/aes.key"
	keyBytes := []byte("0123456789abcdef")
	explicitIV := []byte("explicit-iv-1234")

	pl := &hlsPlaylist{
		IsVOD:         true,
		MediaSequence: 10,
		Segments: []hlsSegment{
			{Index: 0, SeqNum: 10, URI: "https://cdn.example.com/enc/0.ts", Key: &hlsKeyInfo{Method: "AES-128", URI: keyURL}},
			{Index: 1, SeqNum: 11, URI: "https://cdn.example.com/enc/1.ts", Key: &hlsKeyInfo{Method: "AES-128", URI: keyURL}},
			{Index: 2, SeqNum: 12, URI: "https://cdn.example.com/enc/2.ts", Key: &hlsKeyInfo{Method: "AES-128", URI: keyURL, IV: explicitIV}},
		},
	}

	xorTransform := func(src, key, iv []byte) []byte {
		out := make([]byte, len(src))
		for i, b := range src {
			out[i] = b ^ key[i%len(key)] ^ iv[i%len(iv)]
		}
		return out
	}

	seqIV10 := make([]byte, 16)
	binary.BigEndian.PutUint64(seqIV10[8:], 10)
	seqIV11 := make([]byte, 16)
	binary.BigEndian.PutUint64(seqIV11[8:], 11)

	var keyHits atomic.Int32
	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case playlistURL:
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("ENC_PL")}, nil
			case keyURL:
				keyHits.Add(1)
				return &hlsResponse{StatusCode: 200, FinalURL: keyURL, Body: keyBytes}, nil
			case "https://cdn.example.com/enc/0.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: xorTransform([]byte("part0-"), keyBytes, seqIV10)}, nil
			case "https://cdn.example.com/enc/1.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: xorTransform([]byte("part1-"), keyBytes, seqIV11)}, nil
			case "https://cdn.example.com/enc/2.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: xorTransform([]byte("part2"), keyBytes, explicitIV)}, nil
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	deps := baseMockDeps(client, map[string]*hlsPlaylist{"ENC_PL": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		inputs := extractInputPathsFromArgs(args)
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		return os.WriteFile(args[len(args)-1], b, 0644)
	}

	outPath, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    playlistURL,
		Title:      "Encrypted",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, deps)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if keyHits.Load() != 1 {
		t.Fatalf("keyHits = %d, want 1 (key should be cached across segments)", keyHits.Load())
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != "part0-part1-part2" {
		t.Fatalf("decrypted content = %q, want part0-part1-part2", string(got))
	}
}

func TestNativeHLSFMP4StreamRemuxArgs(t *testing.T) {
	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/fmp4/playlist.m3u8"
	initURL := "https://cdn.example.com/fmp4/init.mp4"
	sharedMap := &hlsMapInfo{
		URI:       initURL,
		ByteRange: &hlsByteRange{Offset: 0, Length: 720},
	}
	pl := &hlsPlaylist{
		IsVOD:  true,
		IsFMP4: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/fmp4/main.mp4", Map: sharedMap, ByteRange: &hlsByteRange{Offset: 720, Length: 100}},
			{Index: 1, URI: "https://cdn.example.com/fmp4/main.mp4", Map: sharedMap, ByteRange: &hlsByteRange{Offset: 820, Length: 150}},
		},
	}

	var initHits atomic.Int32
	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case playlistURL:
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("FMP4_PL")}, nil
			case initURL:
				initHits.Add(1)
				if extraHeaders["Range"] != "bytes=0-719" {
					return nil, fmt.Errorf("unexpected init Range header: %q", extraHeaders["Range"])
				}
				return &hlsResponse{StatusCode: 206, FinalURL: rawURL, Body: []byte("FTYP_MOOV:")}, nil
			case "https://cdn.example.com/fmp4/main.mp4":
				switch extraHeaders["Range"] {
				case "bytes=720-819":
					return &hlsResponse{StatusCode: 206, FinalURL: rawURL, Body: []byte("MOOF1:")}, nil
				case "bytes=820-969":
					return &hlsResponse{StatusCode: 206, FinalURL: rawURL, Body: []byte("MOOF2")}, nil
				default:
					return nil, fmt.Errorf("unexpected segment Range header: %q", extraHeaders["Range"])
				}
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	var capturedArgs []string
	var capturedM4S []byte
	deps := baseMockDeps(client, map[string]*hlsPlaylist{"FMP4_PL": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		capturedArgs = append([]string(nil), args...)
		inputs := extractInputPathsFromArgs(args)
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		capturedM4S = b
		return os.WriteFile(args[len(args)-1], b, 0644)
	}

	_, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    playlistURL,
		Title:      "FMP4Video",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, deps)
	if err != nil {
		t.Fatalf("fMP4 download failed: %v", err)
	}

	if initHits.Load() != 1 {
		t.Fatalf("initHits = %d, want 1", initHits.Load())
	}
	if string(capturedM4S) != "FTYP_MOOV:MOOF1:MOOF2" {
		t.Fatalf("capturedM4S = %q, want FTYP_MOOV:MOOF1:MOOF2", string(capturedM4S))
	}
	if slices.Contains(capturedArgs, "-bsf:a") || slices.Contains(capturedArgs, "aac_adtstoasc") {
		t.Fatalf("fMP4 remux must NOT include -bsf:a aac_adtstoasc, got %v", capturedArgs)
	}
}

func TestNativeHLSDiscontinuityConcat(t *testing.T) {
	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/disc/playlist.m3u8"
	pl := &hlsPlaylist{
		IsVOD:            true,
		HasDiscontinuity: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/disc/a.ts"},
			{Index: 1, URI: "https://cdn.example.com/disc/b.ts", Discontinuity: true},
			{Index: 2, URI: "https://cdn.example.com/disc/c.ts"},
		},
	}

	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case playlistURL:
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("DISC_PL")}, nil
			case "https://cdn.example.com/disc/a.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("A")}, nil
			case "https://cdn.example.com/disc/b.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("B")}, nil
			case "https://cdn.example.com/disc/c.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("C")}, nil
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	var (
		capturedArgs   []string
		concatContents string
	)
	deps := baseMockDeps(client, map[string]*hlsPlaylist{"DISC_PL": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		capturedArgs = append([]string(nil), args...)
		inputs := extractInputPathsFromArgs(args)
		if len(inputs) != 1 {
			return fmt.Errorf("expected 1 concat input, got %v", inputs)
		}
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		concatContents = string(b)
		trackDir := filepath.Dir(inputs[0])
		for _, segName := range []string{"seg_000001.ts", "seg_000002.ts", "seg_000003.ts"} {
			if !fileExistsNonEmpty(filepath.Join(trackDir, segName)) {
				return fmt.Errorf("missing segment file %s", segName)
			}
		}
		return os.WriteFile(args[len(args)-1], []byte("MERGED_CONCAT"), 0644)
	}

	_, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    playlistURL,
		Title:      "Discontinuity",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, deps)
	if err != nil {
		t.Fatalf("discontinuity download failed: %v", err)
	}

	wantConcat := "file 'seg_000001.ts'\nfile 'seg_000002.ts'\nfile 'seg_000003.ts'\n"
	if concatContents != wantConcat {
		t.Fatalf("concat.txt = %q, want %q", concatContents, wantConcat)
	}
	if !slices.Contains(capturedArgs, "-f") || !slices.Contains(capturedArgs, "concat") || !slices.Contains(capturedArgs, "-safe") {
		t.Fatalf("expected -f concat -safe 0 in args %v", capturedArgs)
	}
}

func TestNativeHLSDualTrackVideoAndAudio(t *testing.T) {
	outDir := t.TempDir()
	masterURL := "https://cdn.example.com/master.m3u8"
	videoURL := "https://cdn.example.com/video/playlist.m3u8"
	audioURL := "https://cdn.example.com/audio/playlist.m3u8"

	masterPL := &hlsPlaylist{
		IsMaster: true,
		Variants: []hlsVariant{
			{URI: videoURL, Bandwidth: 6000000, Width: 1920, Height: 1080, Resolution: "1920x1080", AudioURI: audioURL},
		},
	}
	videoPL := &hlsPlaylist{
		IsVOD: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/video/v0.ts"},
			{Index: 1, URI: "https://cdn.example.com/video/v1.ts"},
		},
	}
	audioPL := &hlsPlaylist{
		IsVOD: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/audio/a0.ts"},
		},
	}

	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case masterURL:
				return &hlsResponse{StatusCode: 200, FinalURL: masterURL, Body: []byte("MASTER")}, nil
			case videoURL:
				return &hlsResponse{StatusCode: 200, FinalURL: videoURL, Body: []byte("VIDEO")}, nil
			case audioURL:
				return &hlsResponse{StatusCode: 200, FinalURL: audioURL, Body: []byte("AUDIO")}, nil
			case "https://cdn.example.com/video/v0.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("V0")}, nil
			case "https://cdn.example.com/video/v1.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("V1")}, nil
			case "https://cdn.example.com/audio/a0.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("A0")}, nil
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	var (
		capturedInputs [][]byte
		capturedArgs   []string
		progresses     []ProgressUpdate
		mu             sync.Mutex
	)
	deps := baseMockDeps(client, map[string]*hlsPlaylist{
		"MASTER": masterPL,
		"VIDEO":  videoPL,
		"AUDIO":  audioPL,
	})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		capturedArgs = append([]string(nil), args...)
		inputs := extractInputPathsFromArgs(args)
		if len(inputs) != 2 {
			return fmt.Errorf("expected 2 inputs (video+audio), got %v", inputs)
		}
		for _, inPath := range inputs {
			b, err := os.ReadFile(inPath)
			if err != nil {
				return err
			}
			capturedInputs = append(capturedInputs, b)
		}
		return os.WriteFile(args[len(args)-1], []byte("DUAL_MP4"), 0644)
	}

	_, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    masterURL,
		Title:      "DualTrack",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{
		OnProgress: func(p ProgressUpdate) {
			mu.Lock()
			progresses = append(progresses, p)
			mu.Unlock()
		},
	}, deps)
	if err != nil {
		t.Fatalf("dual track download failed: %v", err)
	}

	if len(capturedInputs) != 2 || string(capturedInputs[0]) != "V0V1" || string(capturedInputs[1]) != "A0" {
		t.Fatalf("unexpected track inputs: %q, args=%v", capturedInputs, capturedArgs)
	}

	mu.Lock()
	defer mu.Unlock()
	var saw86, saw98 bool
	for _, p := range progresses {
		if math.Abs(p.Percent-86.0) < 0.01 {
			saw86 = true
		}
		if math.Abs(p.Percent-98.0) < 0.01 {
			saw98 = true
		}
	}
	if !saw86 || !saw98 {
		t.Fatalf("expected progress milestones at 86%% (video done) and 98%% (audio done), got %+v", progresses)
	}
}

func TestNativeHLSCancelAndTempCleanup(t *testing.T) {
	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/cancel/playlist.m3u8"
	pl := &hlsPlaylist{
		IsVOD: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/cancel/0.ts"},
			{Index: 1, URI: "https://cdn.example.com/cancel/1.ts"},
			{Index: 2, URI: "https://cdn.example.com/cancel/2.ts"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case playlistURL:
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("CANCEL_PL")}, nil
			case "https://cdn.example.com/cancel/0.ts":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("seg0")}, nil
			case "https://cdn.example.com/cancel/1.ts":
				cancel()
				return nil, context.Canceled
			default:
				return nil, fmt.Errorf("should not reach %s after cancel", rawURL)
			}
		},
	}

	var remuxCalled atomic.Bool
	deps := baseMockDeps(client, map[string]*hlsPlaylist{"CANCEL_PL": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		remuxCalled.Store(true)
		return nil
	}

	outPath, err := runNativeHLSDownloadWithDeps(ctx, DownloadOptions{
		M3U8Url:    playlistURL,
		Title:      "CancelCheck",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if outPath != "" {
		t.Fatalf("expected empty outPath on cancel, got %q", outPath)
	}
	if remuxCalled.Load() {
		t.Fatal("remuxFn must not be called when canceled")
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "CancelCheck.mp4")); !os.IsNotExist(statErr) {
		t.Fatalf("final mp4 should not exist after cancel, statErr=%v", statErr)
	}
	assertNoTempDirs(t, outDir)
}

func TestSpeedTrackerFormatting(t *testing.T) {
	cases := []struct {
		bps  float64
		want string
	}{
		{bps: 0, want: "0.00 B/s"},
		{bps: -10, want: "0.00 B/s"},
		{bps: math.NaN(), want: "0.00 B/s"},
		{bps: math.Inf(1), want: "0.00 B/s"},
		{bps: 512.5, want: "512.50 B/s"},
		{bps: 1536, want: "1.50 KB/s"},
		{bps: 3 * 1024 * 1024, want: "3.00 MB/s"},
	}
	for _, tc := range cases {
		if got := formatByteSpeed(tc.bps); got != tc.want {
			t.Fatalf("formatByteSpeed(%v) = %q, want %q", tc.bps, got, tc.want)
		}
	}

	tracker := newSpeedTracker(2 * time.Second)
	t0 := time.Unix(1000, 0)
	if got := tracker.SpeedBps(t0); got != 0 {
		t.Fatalf("empty tracker speed = %v, want 0", got)
	}

	// Sample older than 2s window should be pruned
	tracker.Add(10_000_000, t0)
	tracker.Add(1024, t0.Add(2500*time.Millisecond))
	tracker.Add(1024, t0.Add(3500*time.Millisecond))
	bps := tracker.SpeedBps(t0.Add(3500 * time.Millisecond))
	if math.IsNaN(bps) || math.IsInf(bps, 0) || math.Abs(bps-2048.0) > 1.0 {
		t.Fatalf("unexpected sliding window speed = %v (want ~2048.0 B/s)", bps)
	}
}

func TestMoveFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	dst := filepath.Join(dir, "dst.mp4")

	if err := os.WriteFile(dst, []byte("old-content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new-content"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := moveFileOverwrite(src, dst); err != nil {
		t.Fatalf("moveFileOverwrite failed: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("expected source file to be removed, err=%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-content" {
		t.Fatalf("dst content = %q, want new-content", string(got))
	}

	// Test atomic copy fallback path
	src2 := filepath.Join(dir, "src2.mp4")
	if err := os.WriteFile(src2, []byte("copied-content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyFileAtomicOverwrite(src2, dst); err != nil {
		t.Fatalf("copyFileAtomicOverwrite failed: %v", err)
	}
	got2, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, []byte("copied-content")) {
		t.Fatalf("dst content after fallback = %q, want copied-content", string(got2))
	}
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Fatalf("expected src2 to be removed after copy fallback, err=%v", err)
	}
}

func TestNativeHLSBuildRemuxArgsMatrix(t *testing.T) {
	cases := []struct {
		name         string
		videoIn      downloadedTrackInput
		audioIn      *downloadedTrackInput
		wantMaps     []string
		wantADTSToAs bool
	}{
		{
			name:         "single_ts",
			videoIn:      downloadedTrackInput{inputPath: "v.ts", isFMP4: false},
			audioIn:      nil,
			wantMaps:     []string{"0:v:0", "0:a:0?"},
			wantADTSToAs: true,
		},
		{
			name:         "single_fmp4",
			videoIn:      downloadedTrackInput{inputPath: "v.m4s", isFMP4: true},
			audioIn:      nil,
			wantMaps:     []string{"0:v:0", "0:a:0?"},
			wantADTSToAs: false,
		},
		{
			name:         "ts_video_fmp4_audio",
			videoIn:      downloadedTrackInput{inputPath: "v.ts", isFMP4: false},
			audioIn:      &downloadedTrackInput{inputPath: "a.m4s", isFMP4: true},
			wantMaps:     []string{"0:v:0", "1:a:0"},
			wantADTSToAs: false,
		},
		{
			name:         "fmp4_video_ts_audio",
			videoIn:      downloadedTrackInput{inputPath: "v.m4s", isFMP4: true},
			audioIn:      &downloadedTrackInput{inputPath: "a.ts", isFMP4: false},
			wantMaps:     []string{"0:v:0", "1:a:0"},
			wantADTSToAs: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := buildHLSRemuxArgs(tc.videoIn, tc.audioIn, "out.mp4")
			if !slices.Contains(args, "-dn") || !slices.Contains(args, "-sn") {
				t.Fatalf("expected -dn and -sn in %v", args)
			}
			for _, m := range tc.wantMaps {
				idx := slices.Index(args, m)
				if idx <= 0 || args[idx-1] != "-map" {
					t.Fatalf("expected -map %s in %v", m, args)
				}
			}
			hasFilter := slices.Contains(args, "aac_adtstoasc")
			if hasFilter != tc.wantADTSToAs {
				t.Fatalf("has aac_adtstoasc = %v, want %v (args=%v)", hasFilter, tc.wantADTSToAs, args)
			}
		})
	}
}

func TestNativeHLSByteRange200SlicingAndShortBodyError(t *testing.T) {
	fullEntity := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 36 bytes

	// 1. Direct helper tests for short body error
	if _, err := sliceByteRangePayload(&hlsResponse{StatusCode: 200, Body: []byte("short")}, &hlsByteRange{Offset: 5, Length: 10}); err == nil {
		t.Fatal("expected error when 200 OK body is shorter than requested ByteRange")
	}

	// 2. End-to-end track download when server ignores Range and returns 200 OK full body
	outDir := t.TempDir()
	playlistURL := "https://cdn.example.com/br/playlist.m3u8"
	sharedMap := &hlsMapInfo{
		URI:       "https://cdn.example.com/br/main.mp4",
		ByteRange: &hlsByteRange{Offset: 0, Length: 5}, // "01234"
	}
	pl := &hlsPlaylist{
		IsVOD:  true,
		IsFMP4: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/br/main.mp4", Map: sharedMap, ByteRange: &hlsByteRange{Offset: 5, Length: 5}},  // "56789"
			{Index: 1, URI: "https://cdn.example.com/br/main.mp4", Map: sharedMap, ByteRange: &hlsByteRange{Offset: 10, Length: 4}}, // "ABCD"
		},
	}

	client := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			if rawURL == playlistURL {
				return &hlsResponse{StatusCode: 200, FinalURL: playlistURL, Body: []byte("BR_200_PL")}, nil
			}
			// Simulate CDN ignoring Range header and returning 200 OK full entity
			return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: fullEntity}, nil
		},
	}

	var capturedM4S []byte
	deps := baseMockDeps(client, map[string]*hlsPlaylist{"BR_200_PL": pl})
	deps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		inputs := extractInputPathsFromArgs(args)
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		capturedM4S = b
		return os.WriteFile(args[len(args)-1], b, 0644)
	}

	if _, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    playlistURL,
		Title:      "ByteRange200",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, deps); err != nil {
		t.Fatalf("download failed: %v", err)
	}

	if string(capturedM4S) != "0123456789ABCD" {
		t.Fatalf("sliced m4s = %q, want 0123456789ABCD", string(capturedM4S))
	}
}

func TestSpeedTrackerSlowSingleSampleAndFormatting(t *testing.T) {
	tracker := newSpeedTracker(5 * time.Second)
	tStart := time.Unix(2000, 0)
	tEnd := tStart.Add(2 * time.Second)

	// 1 MiB downloaded over 2 seconds -> ~512.00 KB/s (not 50ms spike)
	tracker.AddSpan(1024*1024, tStart, tEnd)
	bps := tracker.SpeedBps(tEnd)
	want := 512.0 * 1024.0
	if math.Abs(bps-want) > 1.0 {
		t.Fatalf("SpeedBps = %v, want %v (~512 KB/s)", bps, want)
	}
	if formatted := formatByteSpeed(bps); formatted != "512.00 KB/s" {
		t.Fatalf("formatByteSpeed = %q, want 512.00 KB/s", formatted)
	}
}

func TestMoveFileOverwritePreservesExistingOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "existing.mp4")
	if err := os.WriteFile(dst, []byte("precious-existing-video"), 0644); err != nil {
		t.Fatal(err)
	}

	// Non-existent source causes both replaceFileSafe and copyFileAtomicOverwrite to fail
	missingSrc := filepath.Join(dir, "does-not-exist.mp4")
	if err := moveFileOverwrite(missingSrc, dst); err == nil {
		t.Fatal("expected moveFileOverwrite to fail when source is missing")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("existing dst was removed on failure: %v", err)
	}
	if string(got) != "precious-existing-video" {
		t.Fatalf("existing dst was corrupted: %q", string(got))
	}
}

func TestNativeHLSFMP4DiscontinuitySameMapAndNestedMasterError(t *testing.T) {
	// 1. Nested Master playlist returns descriptive error
	outDir := t.TempDir()
	masterURL := "https://cdn.example.com/nested/master.m3u8"
	childURL := "https://cdn.example.com/nested/child.m3u8"
	nestedDeps := baseMockDeps(&fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			if rawURL == masterURL {
				return &hlsResponse{StatusCode: 200, FinalURL: masterURL, Body: []byte("TOP_MASTER")}, nil
			}
			return &hlsResponse{StatusCode: 200, FinalURL: childURL, Body: []byte("NESTED_MASTER")}, nil
		},
	}, map[string]*hlsPlaylist{
		"TOP_MASTER":    {IsMaster: true, Variants: []hlsVariant{{URI: childURL, Bandwidth: 5000000}}},
		"NESTED_MASTER": {IsMaster: true, Variants: []hlsVariant{{URI: "https://cdn.example.com/leaf.m3u8", Bandwidth: 3000000}}},
	})
	nestedDeps.remuxFn = func(context.Context, string, []string, func(string, string, string)) error { return nil }

	if _, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    masterURL,
		Title:      "NestedMaster",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, nestedDeps); err == nil || !strings.Contains(err.Error(), "巢狀 Master") {
		t.Fatalf("expected nested master error, got %v", err)
	}

	// 2. fMP4 with #EXT-X-DISCONTINUITY sharing the exact same #EXT-X-MAP splits into concat parts with init bytes
	discURL := "https://cdn.example.com/fmp4disc/playlist.m3u8"
	sharedMap := &hlsMapInfo{URI: "https://cdn.example.com/fmp4disc/init.mp4"}
	discPL := &hlsPlaylist{
		IsVOD:            true,
		IsFMP4:           true,
		HasDiscontinuity: true,
		Segments: []hlsSegment{
			{Index: 0, URI: "https://cdn.example.com/fmp4disc/s0.m4s", Map: sharedMap},
			{Index: 1, URI: "https://cdn.example.com/fmp4disc/s1.m4s", Map: sharedMap, Discontinuity: true},
		},
	}

	var initFetchCount atomic.Int32
	discClient := &fakeDownloadClient{
		getFn: func(ctx context.Context, rawURL string, extraHeaders map[string]string) (*hlsResponse, error) {
			switch rawURL {
			case discURL:
				return &hlsResponse{StatusCode: 200, FinalURL: discURL, Body: []byte("FMP4_DISC_PL")}, nil
			case sharedMap.URI:
				initFetchCount.Add(1)
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("INIT|")}, nil
			case "https://cdn.example.com/fmp4disc/s0.m4s":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("SEG0")}, nil
			case "https://cdn.example.com/fmp4disc/s1.m4s":
				return &hlsResponse{StatusCode: 200, FinalURL: rawURL, Body: []byte("SEG1")}, nil
			default:
				return nil, fmt.Errorf("unexpected URL: %s", rawURL)
			}
		},
	}

	var (
		concatText string
		part0Data  []byte
		part1Data  []byte
		remuxArgs  []string
	)
	discDeps := baseMockDeps(discClient, map[string]*hlsPlaylist{"FMP4_DISC_PL": discPL})
	discDeps.remuxFn = func(ctx context.Context, ffmpegPath string, args []string, onLog func(tag, color, msg string)) error {
		remuxArgs = append([]string(nil), args...)
		inputs := extractInputPathsFromArgs(args)
		b, err := os.ReadFile(inputs[0])
		if err != nil {
			return err
		}
		concatText = string(b)
		dir := filepath.Dir(inputs[0])
		part0Data, _ = os.ReadFile(filepath.Join(dir, "part_000000.m4s"))
		part1Data, _ = os.ReadFile(filepath.Join(dir, "part_000001.m4s"))
		return os.WriteFile(args[len(args)-1], []byte("FMP4_DISC_MP4"), 0644)
	}

	if _, err := runNativeHLSDownloadWithDeps(context.Background(), DownloadOptions{
		M3U8Url:    discURL,
		Title:      "FMP4Disc",
		OutputDir:  outDir,
		FFmpegPath: `C:\bin\ffmpeg.exe`,
	}, DownloadCallbacks{}, discDeps); err != nil {
		t.Fatalf("fMP4 discontinuity download failed: %v", err)
	}

	if initFetchCount.Load() != 1 {
		t.Fatalf("initFetchCount = %d, want 1 (should reuse cached activeInitBytes)", initFetchCount.Load())
	}
	if concatText != "file 'part_000000.m4s'\nfile 'part_000001.m4s'\n" {
		t.Fatalf("concat.txt = %q", concatText)
	}
	if string(part0Data) != "INIT|SEG0" || string(part1Data) != "INIT|SEG1" {
		t.Fatalf("parts mismatch: part0=%q part1=%q", string(part0Data), string(part1Data))
	}
	if !slices.Contains(remuxArgs, "-f") || !slices.Contains(remuxArgs, "concat") {
		t.Fatalf("expected concat input in remuxArgs: %v", remuxArgs)
	}
}
