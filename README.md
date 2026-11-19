# AniDownloader (巴哈姆特動畫瘋 m3u8 影片下載器)

##  核心特色

1. **自帶防盜鏈標頭 (Anti-Hotlinking)**
   - 針對巴哈姆特動畫瘋 CDN 的限制，自動注入：
     * `Origin: https://ani.gamer.com.tw`
     * `Referer: https://ani.gamer.com.tw/`
   - 提供視覺化黃色分段式 **ON / OFF 開關**，可視需求隨時切換。

2. **雙下載核心支援 (強制單線程並發 `CONCURRENCY = 1`)**
   - **YT-DLP 核心 (推薦)**：單線程分段穩定抓取，容錯率高，自帶重試機制，由內嵌 FFmpeg 自動合併轉存 MP4。
   - **FFMPEG 核心**：直接讀取串流並流式寫入 MP4 (`-c copy -bsf:a aac_adtstoasc`)。
   - 底部發光進度條即時顯示下載進度、下載速率與分片狀態。
   - 點擊執行中卡片可立即優雅終止任務，完整清理子程序。

3. **全自包含免安裝封裝**
   - 執行檔內已完整嵌入 `ffmpeg.exe` 與 `yt-dlp.exe`。
   - 下載單一 `AniDownloader_v1.0.0.exe` 雙擊即可直接運行，無需預先配置 Python 或系統 PATH。

---

##  快速下載使用

請前往 [**Releases 發布頁**](../../releases/latest) 下載最新打包成品：

* **`AniDownloader_v1.0.0.exe`**：單檔綠色版，雙擊直接運行。
* **`AniDownloader_v1.0.0_Windows_x64.zip`**：完整便攜包（內含獨立的 `ffmpeg.exe`、`yt-dlp.exe` 與使用說明 `README.txt`）。

### 使用步驟

動畫發：
1. 進入動畫瘋影片播放頁面按下F12，網絡抓取m3u8連結（多數為playlist開頭）
2. 貼入連結，輸入自訂影片標題與儲存目錄，必須打開防盜鏈標頭，點擊 **YT-DLP** 或 **FFMPEG** 卡片即可

Youtube：
1. 複製視頻URL
2. 關閉防盜鏈標頭，點擊 **YT-DLP** 或 **FFMPEG** 卡片即可。
---

##  本地編譯指南

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
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o "AniDownloader.exe" .
```

---

##  免責聲明 (Disclaimer)

本專案僅供技術學習、研究交流與合法個人備份使用。請遵守巴哈姆特動畫瘋的使用者規範與版權宣告，請勿將本工具用於任何商業用途或侵犯他人版權之行為。

---

## 📄 License
MIT License.
