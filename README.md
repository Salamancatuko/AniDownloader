# AniDownloader (巴哈姆特動畫瘋 m3u8 / 混合串流下載器)

## 核心架構與特色

1. **三段式混合下載架構**
   - **原生 `HLS-CORE` (`v2.0.0`)**：純 Go 原生 HLS 下載引擎，透過 `azuretls-client` 模擬 Chrome TLS (JA3) 與 HTTP/2 (Akamai) 指紋，支援 Master/Media M3U8 解析、AES-128 解密與強制單線程並發 (`CONCURRENCY = 1`)，搭配內嵌極簡 FFmpeg 無損封裝為 `.mp4`。
   - **內嵌極簡靜態 `FFMPEG` (`v7.1.1`，LGPL)**：以 MSYS2 MINGW64 純靜態組態編譯（採用 Windows 原生 Schannel TLS 與 w32threads，零外部 DLL 依賴），`bin_embed/ffmpeg.exe.gz` 壓縮體積僅約 **1.4 MiB**，首次啟動於 **0.2 秒內**解壓至 `%LOCALAPPDATA%\AniDownloader\bin`，並內建 SHA-256 自動比對與版本更新替換機制（亦可於程式同目錄或 `bin\` 子目錄放置自訂 `ffmpeg.exe` 覆蓋）。
   - **可選按需外部 `YT-DLP`**：主程式不內嵌 `yt-dlp.exe`（大幅縮減執行檔體積並避免誤報）；需要下載 YouTube 等一般影片網頁時，可至 **Settings -> 外部網頁引擎 (YT-DLP)** 一鍵從官方 GitHub Release 下載安裝或更新，並由內嵌極簡 FFmpeg 完成視音訊雙軌合併與 MP4 封裝。

2. **自帶防盜鏈標頭與 TLS / HTTP2 指紋自訂**
   - 開啟防盜鏈標頭時，自動注入巴哈姆特動畫瘋所需的 `Origin`、`Referer` 與 `User-Agent`。
   - 可於 **Settings** 頁面自訂 `User-Agent`、`JA3` 與 `Akamai` 指紋（留空即使用內建 Chrome TLS 指紋）。
   - 提供視覺化 **ON / OFF 開關**，點擊執行中卡片即可優雅中止任務並清理子程序。

3. **單一執行檔開箱即用**
   - 單一純 Go 建置之 `AniDownloader.exe` 雙擊即可直接運行（使用系統內建 WebView2 呈現介面，無需預先配置 Python 或系統 PATH）。

---

## 快速使用說明

### 巴哈姆特動畫瘋（`.m3u8` 串流）
1. 進入動畫瘋影片播放頁面按下 `F12`，於 Network 分頁擷取 `.m3u8` 串流連結（多數為 `playlist` 開頭）。
2. 貼入連結、輸入自訂影片標題（選填）與儲存目錄，保持 **防盜鏈標頭為 ON**。
3. 點擊 **原生 HLS 指紋下載**（推薦）或 **FFMPEG 直接轉錄** 卡片即可開始下載。

### YouTube / 一般影片網頁
1. 首次使用請至 **Settings -> 外部網頁引擎 (YT-DLP)** 點擊「下載安裝」取得官方 `yt-dlp.exe`（亦可自行將 `yt-dlp.exe` 放至主程式目錄或系統 `PATH`）。
2. 貼上影片網頁 URL，將 **防盜鏈標頭切換為 OFF**，點擊 **原生 HLS 指紋下載** 卡片即會自動路由至外部 `YT-DLP` 下載並由內嵌 `FFmpeg` 合併輸出 `.mp4`。
3. **YouTube 畫質與播放器相容性說明**：下載預設保留平台提供的最佳畫質組合（例如 AV1 / VP9 / H.264 視訊搭配 Opus / AAC 音訊封裝於 `.mp4`）。若 Windows 內建舊版播放器無法解碼 AV1 或 Opus 音軌，建議改用 **VLC**、**MPV**、**PotPlayer**，或於 Microsoft Store 安裝 **AV1 Video Extension** 觀看。

---

## 本地編譯指南

### 前置要求
- [Go 1.24 或更高版本](https://go.dev/dl/)（`go.mod` 目前設定為 Go `1.27.3`）
- Windows 10 / 11 64 位元作業系統
- [MSYS2 MINGW64](https://www.msys2.org/) 工具鏈（僅在需要重新編譯極簡 `bin_embed/ffmpeg.exe.gz` 或執行 `-race` 測試時需要）

### 1.（可選）重新編譯極簡靜態 `bin_embed/ffmpeg.exe.gz`
開啟 **MSYS2 MINGW64** 終端機（請勿使用 MSYS 或 CLANG 視窗），安裝編譯相依套件後執行腳本：

```bash
pacman -S --needed base-devel mingw-w64-x86_64-toolchain mingw-w64-x86_64-nasm mingw-w64-x86_64-pkgconf diffutils make gzip tar xz curl
bash ./scripts/build_min_ffmpeg_msys2.sh
```

腳本會自動下載 FFmpeg 7.1.1 原始碼、以純靜態 LGPL 組態（`--enable-schannel`、`--enable-w32threads`、`--disable-everything` 僅保留本專案所需協定/封裝器/解析器/位元流濾鏡）編譯並 `strip`，自動檢查無任何 MSYS2/MinGW 外部 DLL 依賴與能力齊全後，壓縮寫入 `bin_embed/ffmpeg.exe.gz`。

### 2. 自動化驗證與正式版建置
於 PowerShell 執行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_release.ps1
```

該腳本會依序執行：
1. `gofmt -l .` 格式檢查
2. `GOOS=windows GOARCH=amd64 go vet ./...`
3. `CGO_ENABLED=1 go test -race -count=1 ./...`（包含內嵌 FFmpeg 能力冒煙測試）
4. `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-H=windowsgui -s -w" -o AniDownloader.exe .`

> **提示**：為避免與執行期解壓機制疊加造成 Windows Defender 高度誤報，正式建置**不使用 UPX 加殼**。如需重新生成 Windows 圖示資源（修改 `app.ico` 後），可執行：
> `rsrc -manifest manifest.xml -ico app.ico -o rsrc_windows_amd64.syso`

---

## 免責聲明 (Disclaimer)

本專案僅供技術學習、研究交流與合法個人備份使用。請遵守巴哈姆特動畫瘋及各影音平台的使用者規範與版權宣告，請勿將本工具用於任何商業用途或侵犯他人版權之行為。

---

## 📄 授權與第三方聲明

- 本專案程式碼採用 [MIT License](LICENSE)。
- 內嵌之極簡靜態 FFmpeg 採用 **LGPL v2.1+**，其編譯組態、第三方套件授權與執行期替換說明請參閱 [THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt)。
