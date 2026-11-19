# AniDownloader (巴哈姆特動畫瘋 m3u8 影片下載器)

<div align="center">

![巴哈姆特動畫瘋](assets/logo.svg)

### 專為巴哈姆特動畫瘋設計的串流影片下載與 MP4 封裝工具
**Liquid Glass 無邊框毛玻璃視窗 · 內嵌 FFmpeg 與 yt-dlp · 免安裝開啟即用 · 單線程穩定下載**

[![Platform](https://img.shields.io/badge/Platform-Windows%2010%20%7C%2011%20x64-blue.svg)](#系統需求)
[![Go](https://img.shields.io/badge/Language-Go%201.20+-00ADD8.svg?logo=go)](https://go.dev/)
[![Engine](https://img.shields.io/badge/Core-yt--dlp%20%2B%20FFmpeg-F4962E.svg)](#下載核心)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

---

## ✨ 核心特色

1. **無邊框 Liquid Glass 現代介面 (840×510)**
   - 採用微軟官方 WebView2 核心，呈現高質感毛玻璃與動態光澤。
   - 支援滑鼠任意拖曳移動、原生最小化及關閉。
   - 頂部支援 **透光度 (15%～85%)** 即時調節。
   - 具備 **Downloader**、**Settings**、**Console** 三大面板。
   - 多語系支援：繁體中文、简体中文、English，設定自動持久化儲存。

2. **自帶防盜鏈標頭 (Anti-Hotlinking)**
   - 針對巴哈姆特動畫瘋 CDN 的限制，自動注入：
     * `Origin: https://ani.gamer.com.tw`
     * `Referer: https://ani.gamer.com.tw/`
     * `User-Agent: Mozilla/5.0...`
   - 提供視覺化黃色分段式 **ON / OFF 開關**，可視需求隨時切換。

3. **雙下載核心支援 (強制單線程並發 `CONCURRENCY = 1`)**
   - **YT-DLP 核心 (推薦)**：單線程分段穩定抓取，容錯率高，自帶重試機制，由內嵌 FFmpeg 自動合併轉存 MP4。
   - **FFMPEG 核心**：直接讀取串流並流式寫入 MP4 (`-c copy -bsf:a aac_adtstoasc`)。
   - 底部發光進度條即時顯示下載進度、下載速率與分片狀態。
   - 點擊執行中卡片可立即優雅終止任務，完整清理子程序。

4. **全自包含免安裝封裝**
   - 執行檔內已完整嵌入 `ffmpeg.exe` 與 `yt-dlp.exe`。
   - 下載單一 `動畫瘋m3u8下載器.exe` 雙擊即可直接運行，無需預先配置 Python 或系統 PATH。

---

## 🚀 快速下載使用

請前往 [**Releases 發布頁**](../../releases/latest) 下載最新打包成品：

* **`動畫瘋m3u8下載器.exe`**：單檔綠色版，雙擊直接運行。
* **`AniDownloader_v2.0_Windows_x64.zip`**：完整便攜包（內含獨立的 `ffmpeg.exe`、`yt-dlp.exe` 與使用說明）。

### 使用步驟
1. 打開瀏覽器進入動畫瘋影片播放頁面。
2. 按鍵盤 `F12` 打開開發者工具，切換至 **Network (網路)** 標籤，篩選 `m3u8`。
3. 播放影片後，右鍵複製帶有 token 的 `playlist.m3u8` 或 `chunklist.m3u8` 連結。
4. 打開本下載器，貼入連結，輸入自訂影片標題與儲存目錄，點擊 **YT-DLP** 或 **FFMPEG** 卡片即可高速下載！

---

## 🛠 本地編譯指南

### 前置要求
- [Go 1.20 或更高版本](https://go.dev/dl/)
- Windows 10 / 11 64 位元作業系統（或 Linux 交叉編譯環境）
- `rsrc` 工具（用於生成 Windows 資源與高解析度圖示）

### 編譯指令
```bash
# 1. 複製倉庫
git clone https://github.com/Salamancatuko/AniDownloader.git
cd AniDownloader

# 2. 生成 Windows 資源圖示 (可選)
rsrc -manifest manifest.xml -ico app.ico -o rsrc.syso

# 3. 編譯 Windows 64位元無終端視窗 EXE
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o "動畫瘋m3u8下載器.exe" .
```

---

## ⚖️ 免責聲明 (Disclaimer)

本專案僅供技術學習、研究交流與合法個人備份使用。請遵守巴哈姆特動畫瘋的使用者規範與版權宣告，請勿將本工具用於任何商業用途或侵犯他人版權之行為。

---

## 📄 License
MIT License.
