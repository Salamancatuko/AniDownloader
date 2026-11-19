package main

import (
	"fmt"
	"unsafe"
)

func main() {
	srv := NewAppServer()
	if err := srv.Start(); err != nil {
		procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("啟動本機服務失敗: %v", err)))),
			uintptr(unsafe.Pointer(utf16Ptr("啟動錯誤"))),
			0x10, // MB_ICONERROR
		)
		return
	}
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
		procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("建立視窗失敗: %v", err)))),
			uintptr(unsafe.Pointer(utf16Ptr("啟動錯誤"))),
			0x10, // MB_ICONERROR
		)
	}
}
