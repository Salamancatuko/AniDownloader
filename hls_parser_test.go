//go:build windows

package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"strings"
	"testing"
)

func encryptAES128CBCPKCS7(t *testing.T, plaintext, key, iv []byte) []byte {
	t.Helper()
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher failed: %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	return ciphertext
}

func TestParseM3U8MasterAndSelectBestVariant(t *testing.T) {
	if hlsParseM3U8Hook == nil || hlsSelectBestHook == nil || hlsDecryptAES128Hook == nil || hlsBuildSeqIVHook == nil {
		t.Fatal("expected init() to register all four WF-2 parser/crypto hooks")
	}

	m3u8 := "\uFEFF#EXTM3U\r\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"Alt\",DEFAULT=NO,AUTOSELECT=YES,URI=\"audio/alt.m3u8\"\r\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"Main\",DEFAULT=YES,AUTOSELECT=YES,URI=\"audio/main.m3u8\"\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1500000,AVERAGE-BANDWIDTH=1200000,RESOLUTION=854x480,CODECS=\"avc1.4d401f,mp4a.40.2\",AUDIO=\"aac\"\r\n" +
		"480p/index.m3u8\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,AVERAGE-BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f,mp4a.40.2\",AUDIO=\"aac\"\r\n" +
		"720p/index.m3u8\r\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=6000000,AVERAGE-BANDWIDTH=5500000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\",AUDIO=\"aac\"\r\n" +
		"1080p/index.m3u8\r\n"

	pl, err := parseM3U8(m3u8, "https://cdn.example.com/anime/master.m3u8?token=xyz")
	if err != nil {
		t.Fatalf("parseM3U8 failed: %v", err)
	}
	if !pl.IsMaster {
		t.Fatal("expected IsMaster = true")
	}
	if len(pl.Variants) != 3 {
		t.Fatalf("len(Variants) = %d, want 3", len(pl.Variants))
	}

	best, err := selectBestVariant(pl.Variants)
	if err != nil {
		t.Fatalf("selectBestVariant failed: %v", err)
	}
	if best.Width != 1920 || best.Height != 1080 || best.Bandwidth != 6000000 {
		t.Fatalf("unexpected best variant selected: %+v", best)
	}
	if best.URI != "https://cdn.example.com/anime/1080p/index.m3u8" {
		t.Fatalf("best.URI = %q, want 1080p absolute URI", best.URI)
	}
	if best.AudioURI != "https://cdn.example.com/anime/audio/main.m3u8" {
		t.Fatalf("best.AudioURI = %q, want DEFAULT=YES audio absolute URI", best.AudioURI)
	}
}

func TestSelectBestVariantTieBreakers(t *testing.T) {
	if _, err := selectBestVariant(nil); err == nil {
		t.Fatal("expected error for empty variants slice")
	}

	// Tie on Bandwidth -> pick higher Width * Height
	variantsResTie := []hlsVariant{
		{URI: "https://cdn.example.com/720p.m3u8", Bandwidth: 4000000, Width: 1280, Height: 720, AvgBandwidth: 3900000},
		{URI: "https://cdn.example.com/1080p.m3u8", Bandwidth: 4000000, Width: 1920, Height: 1080, AvgBandwidth: 3500000},
	}
	best, err := selectBestVariant(variantsResTie)
	if err != nil {
		t.Fatalf("selectBestVariant failed: %v", err)
	}
	if best.URI != "https://cdn.example.com/1080p.m3u8" {
		t.Fatalf("expected resolution tie-breaker to choose 1080p, got %+v", best)
	}

	// Tie on Bandwidth and Resolution -> pick higher AvgBandwidth
	variantsAvgTie := []hlsVariant{
		{URI: "https://cdn.example.com/low_avg.m3u8", Bandwidth: 4000000, Width: 1920, Height: 1080, AvgBandwidth: 3200000},
		{URI: "https://cdn.example.com/high_avg.m3u8", Bandwidth: 4000000, Width: 1920, Height: 1080, AvgBandwidth: 3800000},
	}
	best, err = selectBestVariant(variantsAvgTie)
	if err != nil {
		t.Fatalf("selectBestVariant failed: %v", err)
	}
	if best.URI != "https://cdn.example.com/high_avg.m3u8" {
		t.Fatalf("expected AvgBandwidth tie-breaker to choose high_avg, got %+v", best)
	}
}

func TestParseM3U8RelativeURLResolutionWithQueryToken(t *testing.T) {
	baseURL := "https://cdn.example.com/v1/1080p/playlist.m3u8?token=abc"
	m3u8 := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-KEY:METHOD=AES-128,URI="../key.bin"
#EXTINF:5.5,
seg_01.ts
#EXTINF:6.0,
/abs/seg_02.ts
#EXTINF:4.5,
https://other.cdn/seg_03.ts
#EXT-X-ENDLIST
`

	pl, err := parseM3U8(m3u8, baseURL)
	if err != nil {
		t.Fatalf("parseM3U8 failed: %v", err)
	}
	if pl.IsMaster {
		t.Fatal("expected IsMaster = false")
	}
	if !pl.IsVOD {
		t.Fatal("expected IsVOD = true")
	}
	if pl.MediaSequence != 10 {
		t.Fatalf("MediaSequence = %d, want 10", pl.MediaSequence)
	}
	if len(pl.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(pl.Segments))
	}
	if pl.TotalDuration != 16.0 {
		t.Fatalf("TotalDuration = %v, want 16.0", pl.TotalDuration)
	}

	wantURIs := []string{
		"https://cdn.example.com/v1/1080p/seg_01.ts",
		"https://cdn.example.com/v1/key.bin",
		"https://cdn.example.com/abs/seg_02.ts",
		"https://other.cdn/seg_03.ts",
	}
	if pl.Segments[0].Key == nil || pl.Segments[0].Key.URI != wantURIs[1] {
		t.Fatalf("Key URI = %v, want %q", pl.Segments[0].Key, wantURIs[1])
	}
	if pl.Segments[0].URI != wantURIs[0] || pl.Segments[1].URI != wantURIs[2] || pl.Segments[2].URI != wantURIs[3] {
		t.Fatalf("unexpected segment URIs: %+v", pl.Segments)
	}
	for i, seg := range pl.Segments {
		if seg.Index != i {
			t.Fatalf("seg[%d].Index = %d, want %d", i, seg.Index, i)
		}
		if seg.SeqNum != uint64(10+i) {
			t.Fatalf("seg[%d].SeqNum = %d, want %d", i, seg.SeqNum, 10+i)
		}
	}
}

func TestParseM3U8QuotedAttributesWithCommasAndKeyImmutability(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="https://example.com/key?a=1,b=2&c=3",IV=0x0000000000000000000000000000002a
#EXTINF:4.0,
seg0.ts
#EXT-X-KEY:METHOD=AES-128,URI="https://example.com/key2?x=\"1\",y=2",IV=0X2B
#EXTINF:4.0,
seg1.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:4.0,
seg2.ts
#EXT-X-PLAYLIST-TYPE:VOD
`

	pl, err := parseM3U8(m3u8, "https://example.com/stream/playlist.m3u8")
	if err != nil {
		t.Fatalf("parseM3U8 failed: %v", err)
	}
	if !pl.IsVOD {
		t.Fatal("expected IsVOD = true for #EXT-X-PLAYLIST-TYPE:VOD")
	}
	if len(pl.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(pl.Segments))
	}

	k0 := pl.Segments[0].Key
	if k0 == nil || k0.Method != "AES-128" || k0.URI != "https://example.com/key?a=1,b=2&c=3" {
		t.Fatalf("seg0 key mismatch: %+v", k0)
	}
	wantIV0 := make([]byte, 16)
	wantIV0[15] = 0x2a
	if !bytes.Equal(k0.IV, wantIV0) {
		t.Fatalf("seg0 IV = %x, want %x", k0.IV, wantIV0)
	}

	k1 := pl.Segments[1].Key
	if k1 == nil || k1 == k0 {
		t.Fatalf("expected seg1 to have a distinct *hlsKeyInfo instance, k0=%p k1=%p", k0, k1)
	}
	if k1.URI != `https://example.com/key2?x="1",y=2` {
		t.Fatalf("seg1 key URI = %q", k1.URI)
	}
	wantIV1 := make([]byte, 16)
	wantIV1[15] = 0x2b
	if !bytes.Equal(k1.IV, wantIV1) {
		t.Fatalf("seg1 IV = %x, want %x", k1.IV, wantIV1)
	}

	if pl.Segments[2].Key != nil {
		t.Fatalf("expected seg2 key to be nil after METHOD=NONE, got %+v", pl.Segments[2].Key)
	}
	// Re-check seg0 key was not mutated by later key changes
	if k0.URI != "https://example.com/key?a=1,b=2&c=3" || !bytes.Equal(k0.IV, wantIV0) {
		t.Fatalf("seg0 key was unexpectedly mutated: %+v", k0)
	}
}

func TestDecryptAES128EndToEndAndInvalidPadding(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")

	plaintexts := [][]byte{
		[]byte("A"),
		[]byte("16-bytes-exactly"),
		[]byte("32-bytes-exact-plaintext-sample!"),
		[]byte("This is a 47-byte HLS MPEG-TS payload test case"),
	}

	for _, pt := range plaintexts {
		ct := encryptAES128CBCPKCS7(t, pt, key, iv)
		ctSnapshot := append([]byte(nil), ct...)

		got, err := decryptAES128CBC(ct, key, iv)
		if err != nil {
			t.Fatalf("decryptAES128CBC failed for len=%d: %v", len(pt), err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("decrypted mismatch for len=%d: got %q, want %q", len(pt), got, pt)
		}
		if !bytes.Equal(ct, ctSnapshot) {
			t.Fatal("decryptAES128CBC mutated caller ciphertext slice")
		}
	}

	ct := encryptAES128CBCPKCS7(t, []byte("secret segment payload"), key, iv)

	// Wrong key -> padding validation failure without panic
	wrongKey := []byte("ffffffffffffffff")
	if _, err := decryptAES128CBC(ct, wrongKey, iv); err == nil {
		t.Fatal("expected error for wrong key")
	}

	// Corrupted last byte -> padding validation failure without panic
	corrupted := append([]byte(nil), ct...)
	corrupted[len(corrupted)-1] ^= 0xff
	if _, err := decryptAES128CBC(corrupted, key, iv); err == nil {
		t.Fatal("expected error for corrupted padding")
	}

	// Invalid key/iv/ciphertext dimensions
	if _, err := decryptAES128CBC(ct, []byte("short"), iv); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := decryptAES128CBC(ct, key, []byte("short")); err == nil {
		t.Fatal("expected error for short iv")
	}
	if _, err := decryptAES128CBC([]byte{}, key, iv); err == nil {
		t.Fatal("expected error for empty ciphertext")
	}
	if _, err := decryptAES128CBC([]byte("not-16-bytes"), key, iv); err == nil {
		t.Fatal("expected error for unaligned ciphertext")
	}
}

func TestBuildMediaSeqIV(t *testing.T) {
	iv := buildMediaSeqIV(42)
	if len(iv) != 16 {
		t.Fatalf("len(iv) = %d, want 16", len(iv))
	}
	for i := 0; i < 8; i++ {
		if iv[i] != 0 {
			t.Fatalf("iv[%d] = %d, want 0", i, iv[i])
		}
	}
	if got := binary.BigEndian.Uint64(iv[8:]); got != 42 {
		t.Fatalf("big-endian seq = %d, want 42", got)
	}
}

func TestParseM3U8RejectsDRM(t *testing.T) {
	cases := []string{
		"#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://drm\"\n#EXTINF:4,\nseg.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://example.com/k\",KEYFORMAT=\"urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed\"\n#EXTINF:4,\nseg.ts\n",
	}

	for i, m3u8 := range cases {
		_, err := parseM3U8(m3u8, "https://example.com/playlist.m3u8")
		if err == nil {
			t.Fatalf("case %d: expected DRM error", i)
		}
		if !strings.Contains(err.Error(), "DRM") && !strings.Contains(err.Error(), "SAMPLE-AES") {
			t.Fatalf("case %d: expected error mentioning DRM or SAMPLE-AES, got %v", i, err)
		}
	}
}

func TestParseM3U8FMP4DiscontinuityAndByteRange(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@0"
#EXTINF:5.0,
#EXT-X-BYTERANGE:1000@720
main.mp4
#EXTINF:5.0,
#EXT-X-BYTERANGE:1200
main.mp4
#EXT-X-DISCONTINUITY
#EXTINF:5.0,
#EXT-X-BYTERANGE:800
main.mp4
#EXT-X-ENDLIST
`

	pl, err := parseM3U8(m3u8, "https://cdn.example.com/video/playlist.m3u8")
	if err != nil {
		t.Fatalf("parseM3U8 failed: %v", err)
	}
	if !pl.IsFMP4 {
		t.Fatal("expected IsFMP4 = true")
	}
	if !pl.HasDiscontinuity {
		t.Fatal("expected HasDiscontinuity = true")
	}
	if len(pl.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(pl.Segments))
	}

	seg0, seg1, seg2 := pl.Segments[0], pl.Segments[1], pl.Segments[2]
	if seg0.Map == nil || seg0.Map.URI != "https://cdn.example.com/video/init.mp4" {
		t.Fatalf("seg0.Map mismatch: %+v", seg0.Map)
	}
	if seg0.Map.ByteRange == nil || seg0.Map.ByteRange.Length != 720 || seg0.Map.ByteRange.Offset != 0 {
		t.Fatalf("seg0.Map.ByteRange mismatch: %+v", seg0.Map.ByteRange)
	}

	if seg0.ByteRange == nil || seg0.ByteRange.Offset != 720 || seg0.ByteRange.Length != 1000 {
		t.Fatalf("seg0.ByteRange mismatch: %+v", seg0.ByteRange)
	}
	if seg1.ByteRange == nil || seg1.ByteRange.Offset != 1720 || seg1.ByteRange.Length != 1200 {
		t.Fatalf("seg1.ByteRange mismatch: %+v", seg1.ByteRange)
	}
	if seg2.ByteRange == nil || seg2.ByteRange.Offset != 2920 || seg2.ByteRange.Length != 800 {
		t.Fatalf("seg2.ByteRange mismatch: %+v", seg2.ByteRange)
	}

	if seg0.Discontinuity || seg1.Discontinuity || !seg2.Discontinuity {
		t.Fatalf("discontinuity flags mismatch: seg0=%v seg1=%v seg2=%v", seg0.Discontinuity, seg1.Discontinuity, seg2.Discontinuity)
	}
}

func TestParseM3U8InvalidAndEmptyPlaylists(t *testing.T) {
	if _, err := parseM3U8("#EXTINF:5.0,\nseg0.ts\n", "https://example.com/a.m3u8"); err == nil {
		t.Fatal("expected error when #EXTM3U header is missing")
	}

	emptyMedia := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-ENDLIST
`
	if _, err := parseM3U8(emptyMedia, "https://example.com/a.m3u8"); err == nil {
		t.Fatal("expected error for 0-segment media playlist")
	}
}

func TestParseM3U8FMP4MapByteRangeAdvancesSegmentOffset(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MAP:URI="main.mp4",BYTERANGE="720@0"
#EXTINF:5.0,
#EXT-X-BYTERANGE:1000
main.mp4
#EXTINF:5.0,
#EXT-X-BYTERANGE:500
main.mp4
#EXT-X-ENDLIST
`
	pl, err := parseM3U8(m3u8, "https://cdn.example.com/video/playlist.m3u8")
	if err != nil {
		t.Fatalf("parseM3U8 failed: %v", err)
	}
	if len(pl.Segments) != 2 {
		t.Fatalf("len(Segments) = %d, want 2", len(pl.Segments))
	}
	if br0 := pl.Segments[0].ByteRange; br0 == nil || br0.Offset != 720 || br0.Length != 1000 {
		t.Fatalf("seg0.ByteRange = %+v, want Offset=720 Length=1000", br0)
	}
	if br1 := pl.Segments[1].ByteRange; br1 == nil || br1.Offset != 1720 || br1.Length != 500 {
		t.Fatalf("seg1.ByteRange = %+v, want Offset=1720 Length=500", br1)
	}
}
