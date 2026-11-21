//go:build windows

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	hlsParseM3U8Hook = parseM3U8
	hlsSelectBestHook = selectBestVariant
	hlsDecryptAES128Hook = decryptAES128CBC
	hlsBuildSeqIVHook = buildMediaSeqIV
}

type audioTrackChoice struct {
	uri       string
	isDefault bool
}

// parseM3U8Attributes scans KEY=VALUE attribute lists character-by-character so
// commas, equals signs, and escaped characters inside double quotes are preserved.
func parseM3U8Attributes(attrStr string) map[string]string {
	attrs := make(map[string]string)
	n := len(attrStr)
	i := 0

	for i < n {
		for i < n && (attrStr[i] == ' ' || attrStr[i] == '\t' || attrStr[i] == ',') {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && attrStr[i] != '=' && attrStr[i] != ',' {
			i++
		}
		key := strings.ToUpper(strings.TrimSpace(attrStr[keyStart:i]))
		if i >= n || attrStr[i] != '=' {
			if i < n && attrStr[i] == ',' {
				i++
			}
			continue
		}
		i++ // consume '='

		for i < n && (attrStr[i] == ' ' || attrStr[i] == '\t') {
			i++
		}

		var val string
		if i < n && attrStr[i] == '"' {
			i++ // consume opening quote
			var b strings.Builder
			for i < n {
				ch := attrStr[i]
				if ch == '\\' && i+1 < n {
					b.WriteByte(attrStr[i+1])
					i += 2
					continue
				}
				if ch == '"' {
					i++ // consume closing quote
					break
				}
				b.WriteByte(ch)
				i++
			}
			val = b.String()
			for i < n && attrStr[i] != ',' {
				i++
			}
			if i < n && attrStr[i] == ',' {
				i++
			}
		} else {
			valStart := i
			for i < n && attrStr[i] != ',' {
				i++
			}
			val = strings.TrimSpace(attrStr[valStart:i])
			if i < n && attrStr[i] == ',' {
				i++
			}
		}

		if key != "" {
			attrs[key] = val
		}
	}

	return attrs
}

// resolveM3U8URL resolves relative or absolute M3U8 URIs against baseStr.
func resolveM3U8URL(baseStr, refStr string) (string, error) {
	refClean := strings.TrimSpace(refStr)
	if refClean == "" {
		return "", fmt.Errorf("無效的 m3u8 參照網址：URI 為空")
	}

	ref, err := url.Parse(refClean)
	if err != nil {
		return "", fmt.Errorf("解析 m3u8 參照網址失敗 (%q): %w", refClean, err)
	}
	if ref.IsAbs() {
		return ref.String(), nil
	}

	baseClean := strings.TrimSpace(baseStr)
	if baseClean == "" {
		return "", fmt.Errorf("無法解析相對網址 %q：基底網址為空", refClean)
	}
	base, err := url.Parse(baseClean)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("無效的 m3u8 基底網址: %q", baseClean)
	}

	return base.ResolveReference(ref).String(), nil
}

func parseHLSByteRangeFlag(raw string, defaultOffset int64) (*hlsByteRange, int64, bool, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, 0, false, fmt.Errorf("無效的 BYTERANGE：值為空")
	}

	parts := strings.SplitN(s, "@", 2)
	length, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || length <= 0 {
		return nil, 0, false, fmt.Errorf("無效的 BYTERANGE 長度: %q", raw)
	}

	offset := defaultOffset
	explicitOffset := false
	if len(parts) == 2 {
		explicitOffset = true
		offset, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || offset < 0 {
			return nil, 0, false, fmt.Errorf("無效的 BYTERANGE 位移: %q", raw)
		}
	}

	br := &hlsByteRange{
		Length: length,
		Offset: offset,
	}
	return br, offset + length, explicitOffset, nil
}

func parseHLSByteRange(raw string, lastEndOffset int64) (*hlsByteRange, int64, error) {
	br, nextEnd, _, err := parseHLSByteRangeFlag(raw, lastEndOffset)
	return br, nextEnd, err
}

func parseHLSIV(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	if s == "" || len(s) > 32 {
		return nil, fmt.Errorf("無效的 AES-128 IV 格式: %q", raw)
	}
	if len(s) < 32 {
		s = strings.Repeat("0", 32-len(s)) + s
	}
	iv, err := hex.DecodeString(s)
	if err != nil || len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("無法解碼 AES-128 IV (%q): %w", raw, err)
	}
	return iv, nil
}

func parseResolution(res string) (int, int) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(res)), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil || w < 0 || h < 0 {
		return 0, 0
	}
	return w, h
}

func parseM3U8(content string, baseURL string) (*hlsPlaylist, error) {
	cleaned := strings.TrimPrefix(content, "\uFEFF")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")

	rawLines := strings.Split(cleaned, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}

	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return nil, fmt.Errorf("無效的 m3u8 清單：缺少 #EXTM3U 標頭")
	}

	// First pass: collect #EXT-X-MEDIA AUDIO groups so variants can match them regardless of tag order.
	audioGroups := make(map[string]audioTrackChoice)
	for _, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attrs := parseM3U8Attributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if !strings.EqualFold(strings.TrimSpace(attrs["TYPE"]), "AUDIO") {
			continue
		}
		groupID := strings.TrimSpace(attrs["GROUP-ID"])
		rawURI := strings.TrimSpace(attrs["URI"])
		if groupID == "" || rawURI == "" {
			continue
		}
		absURI, err := resolveM3U8URL(baseURL, rawURI)
		if err != nil {
			return nil, fmt.Errorf("解析音軌群組 %q URI 失敗: %w", groupID, err)
		}
		isDefault := strings.EqualFold(strings.TrimSpace(attrs["DEFAULT"]), "YES")
		existing, exists := audioGroups[groupID]
		if !exists || (!existing.isDefault && isDefault) {
			audioGroups[groupID] = audioTrackChoice{
				uri:       absURI,
				isDefault: isDefault,
			}
		}
	}

	playlist := &hlsPlaylist{}
	var (
		currentKey            *hlsKeyInfo
		currentMap            *hlsMapInfo
		pendingDuration       float64
		pendingByteRange      *hlsByteRange
		pendingExplicitOffset bool
		pendingDiscontinuity  bool
		lastEndOffsetByURI    = make(map[string]int64)
	)

	for i := 1; i < len(lines); i++ {
		line := lines[i]

		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			playlist.IsMaster = true
			attrs := parseM3U8Attributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))

			bw, _ := strconv.ParseInt(strings.TrimSpace(attrs["BANDWIDTH"]), 10, 64)
			avgBW, _ := strconv.ParseInt(strings.TrimSpace(attrs["AVERAGE-BANDWIDTH"]), 10, 64)
			res := strings.TrimSpace(attrs["RESOLUTION"])
			w, h := parseResolution(res)
			audioGroup := strings.TrimSpace(attrs["AUDIO"])

			uriIdx := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "#EXT-X-STREAM-INF:") {
					return nil, fmt.Errorf("無效的主清單：#EXT-X-STREAM-INF 缺少對應的變體網址")
				}
				if !strings.HasPrefix(lines[j], "#") {
					uriIdx = j
					break
				}
			}
			if uriIdx == -1 {
				return nil, fmt.Errorf("無效的主清單：#EXT-X-STREAM-INF 缺少對應的變體網址")
			}

			absVariantURI, err := resolveM3U8URL(baseURL, lines[uriIdx])
			if err != nil {
				return nil, fmt.Errorf("解析串流變體網址失敗: %w", err)
			}

			variant := hlsVariant{
				URI:          absVariantURI,
				Bandwidth:    bw,
				AvgBandwidth: avgBW,
				Width:        w,
				Height:       h,
				Resolution:   res,
				Codecs:       strings.TrimSpace(attrs["CODECS"]),
				AudioGroup:   audioGroup,
			}
			if audioGroup != "" {
				if track, ok := audioGroups[audioGroup]; ok {
					variant.AudioURI = track.uri
				}
			}
			playlist.Variants = append(playlist.Variants, variant)
			i = uriIdx
			continue
		}

		if strings.HasPrefix(line, "#") {
			switch {
			case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
				valStr := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
				seq, err := strconv.ParseUint(valStr, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("無效的 #EXT-X-MEDIA-SEQUENCE 值: %q", valStr)
				}
				playlist.MediaSequence = seq

			case line == "#EXT-X-ENDLIST":
				playlist.IsVOD = true

			case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
				ptype := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-PLAYLIST-TYPE:"))
				if strings.EqualFold(ptype, "VOD") {
					playlist.IsVOD = true
				}

			case strings.HasPrefix(line, "#EXT-X-KEY:"):
				attrs := parseM3U8Attributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
				method := strings.ToUpper(strings.TrimSpace(attrs["METHOD"]))
				keyFormat := strings.ToLower(strings.TrimSpace(attrs["KEYFORMAT"]))
				if keyFormat != "" && keyFormat != "identity" {
					return nil, fmt.Errorf("不支援的加密格式 (%s / KEYFORMAT=%s)，此串流可能受 DRM 保護", method, attrs["KEYFORMAT"])
				}

				switch method {
				case "NONE":
					currentKey = nil
				case "AES-128":
					rawKeyURI := strings.TrimSpace(attrs["URI"])
					if rawKeyURI == "" {
						return nil, fmt.Errorf("AES-128 金鑰標籤缺少 URI")
					}
					absKeyURI, err := resolveM3U8URL(baseURL, rawKeyURI)
					if err != nil {
						return nil, fmt.Errorf("解析 AES-128 金鑰網址失敗: %w", err)
					}
					var ivBytes []byte
					if rawIV := strings.TrimSpace(attrs["IV"]); rawIV != "" {
						ivBytes, err = parseHLSIV(rawIV)
						if err != nil {
							return nil, err
						}
					}
					// Always allocate a new hlsKeyInfo so previous segments retain their original key metadata.
					currentKey = &hlsKeyInfo{
						Method: "AES-128",
						URI:    absKeyURI,
						IV:     ivBytes,
					}
				default:
					return nil, fmt.Errorf("不支援的加密格式 (%s)，此串流可能受 DRM 保護", method)
				}

			case strings.HasPrefix(line, "#EXT-X-MAP:"):
				playlist.IsFMP4 = true
				attrs := parseM3U8Attributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
				rawMapURI := strings.TrimSpace(attrs["URI"])
				if rawMapURI == "" {
					return nil, fmt.Errorf("#EXT-X-MAP 標籤缺少 URI")
				}
				absMapURI, err := resolveM3U8URL(baseURL, rawMapURI)
				if err != nil {
					return nil, fmt.Errorf("解析 #EXT-X-MAP 初始化分片網址失敗: %w", err)
				}
				var mapBR *hlsByteRange
				if rawBR := strings.TrimSpace(attrs["BYTERANGE"]); rawBR != "" {
					mapBR, _, err = parseHLSByteRange(rawBR, 0)
					if err != nil {
						return nil, err
					}
					lastEndOffsetByURI[absMapURI] = mapBR.Offset + mapBR.Length
				}
				currentMap = &hlsMapInfo{
					URI:       absMapURI,
					ByteRange: mapBR,
				}

			case strings.HasPrefix(line, "#EXTINF:"):
				val := strings.TrimPrefix(line, "#EXTINF:")
				if comma := strings.IndexByte(val, ','); comma >= 0 {
					val = val[:comma]
				}
				dur, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil || dur < 0 {
					return nil, fmt.Errorf("無效的 #EXTINF 時長: %q", line)
				}
				pendingDuration = dur

			case line == "#EXT-X-DISCONTINUITY":
				playlist.HasDiscontinuity = true
				pendingDiscontinuity = true

			case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
				rawBR := strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
				br, _, explicit, err := parseHLSByteRangeFlag(rawBR, 0)
				if err != nil {
					return nil, err
				}
				pendingByteRange = br
				pendingExplicitOffset = explicit
			}
			continue
		}

		if playlist.IsMaster {
			continue
		}

		absSegURI, err := resolveM3U8URL(baseURL, line)
		if err != nil {
			return nil, fmt.Errorf("解析媒體分片網址失敗: %w", err)
		}

		if pendingByteRange != nil {
			if !pendingExplicitOffset {
				pendingByteRange.Offset = lastEndOffsetByURI[absSegURI]
			}
			lastEndOffsetByURI[absSegURI] = pendingByteRange.Offset + pendingByteRange.Length
		}

		idx := len(playlist.Segments)
		seg := hlsSegment{
			Index:         idx,
			SeqNum:        playlist.MediaSequence + uint64(idx),
			URI:           absSegURI,
			Duration:      pendingDuration,
			Key:           currentKey,
			Map:           currentMap,
			ByteRange:     pendingByteRange,
			Discontinuity: pendingDiscontinuity,
		}
		playlist.Segments = append(playlist.Segments, seg)
		playlist.TotalDuration += pendingDuration

		pendingDuration = 0
		pendingByteRange = nil
		pendingExplicitOffset = false
		pendingDiscontinuity = false
	}

	if playlist.IsMaster {
		if len(playlist.Variants) == 0 {
			return nil, fmt.Errorf("主清單中找不到任何可用的串流變體")
		}
		return playlist, nil
	}

	if len(playlist.Segments) == 0 {
		return nil, fmt.Errorf("m3u8 清單中找不到任何可下載的媒體分片")
	}

	return playlist, nil
}

func selectBestVariant(variants []hlsVariant) (*hlsVariant, error) {
	if len(variants) == 0 {
		return nil, fmt.Errorf("找不到任何可用的串流變體")
	}

	best := variants[0]
	for _, v := range variants[1:] {
		if v.Bandwidth != best.Bandwidth {
			if v.Bandwidth > best.Bandwidth {
				best = v
			}
			continue
		}

		vPixels := v.Width * v.Height
		bestPixels := best.Width * best.Height
		if vPixels != bestPixels {
			if vPixels > bestPixels {
				best = v
			}
			continue
		}

		if v.AvgBandwidth > best.AvgBandwidth {
			best = v
		}
	}

	return &best, nil
}

func buildMediaSeqIV(seq uint64) []byte {
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], seq)
	return iv
}

func decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("無效的 AES-128 金鑰長度: %d（預期 16 bytes）", len(key))
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("無效的 AES-128 IV 長度: %d（預期 16 bytes）", len(iv))
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("無效的 AES-128 密文長度: %d（必須為 16 的正整數倍）", len(ciphertext))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("建立 AES-128 解密器失敗: %w", err)
	}

	dst := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(dst, ciphertext)

	padLen := int(dst[len(dst)-1])
	if padLen < 1 || padLen > aes.BlockSize || padLen > len(dst) {
		return nil, fmt.Errorf("AES-128 解密填充驗證失敗（金鑰或 IV 可能不正確）")
	}
	for i := len(dst) - padLen; i < len(dst); i++ {
		if dst[i] != byte(padLen) {
			return nil, fmt.Errorf("AES-128 解密填充驗證失敗（金鑰或 IV 可能不正確）")
		}
	}

	return dst[:len(dst)-padLen], nil
}
