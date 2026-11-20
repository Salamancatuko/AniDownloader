//go:build windows

package main

import "fmt"

func main() {
	srv := NewAppServer()
	defer srv.Shutdown()

	// Unpack / locate bundled ffmpeg.exe & yt-dlp.exe asynchronously so the UI opens immediately
	go func() {
		info, err := EnsureBinaries(func(tag, color, msg string) {
			srv.AddLog(tag, color, msg)
		})
		if err != nil {
			srv.SetToolsFailed(err)
			return
		}
		srv.SetToolsReady(info)
	}()

	if err := RunDesktopWindow(srv); err != nil {
		showErrorMessageBox("啟動錯誤", fmt.Sprintf("建立視窗失敗: %v", err))
	}
}
