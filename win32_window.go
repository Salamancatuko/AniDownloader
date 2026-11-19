//go:build windows
// +build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	procGetModuleHandleW            = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW            = user32.NewProc("RegisterClassExW")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDefWindowProcW              = user32.NewProc("DefWindowProcW")
	procDestroyWindow               = user32.NewProc("DestroyWindow")
	procShowWindow                  = user32.NewProc("ShowWindow")
	procUpdateWindow                = user32.NewProc("UpdateWindow")
	procSetFocus                    = user32.NewProc("SetFocus")
	procGetMessageW                 = user32.NewProc("GetMessageW")
	procTranslateMessage            = user32.NewProc("TranslateMessage")
	procDispatchMessageW            = user32.NewProc("DispatchMessageW")
	procPostQuitMessage             = user32.NewProc("PostQuitMessage")
	procSendMessageW                = user32.NewProc("SendMessageW")
	procPostMessageW                = user32.NewProc("PostMessageW")
	procReleaseCapture              = user32.NewProc("ReleaseCapture")
	procSetWindowPos                = user32.NewProc("SetWindowPos")
	procSetWindowRgn                = user32.NewProc("SetWindowRgn")
	procGetSystemMetrics            = user32.NewProc("GetSystemMetrics")
	procLoadImageW                  = user32.NewProc("LoadImageW")
	procLoadCursorW                 = user32.NewProc("LoadCursorW")
	procSetProcessDPIAware          = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessCtx   = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForWindow             = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem             = user32.NewProc("GetDpiForSystem")
	procMessageBoxW                 = user32.NewProc("MessageBoxW")

	procCreateSolidBrush            = gdi32.NewProc("CreateSolidBrush")
	procCreateRoundRectRgn          = gdi32.NewProc("CreateRoundRectRgn")

	procDwmSetWindowAttribute       = dwmapi.NewProc("DwmSetWindowAttribute")

	procShellExecuteW               = shell32.NewProc("ShellExecuteW")
	procSHBrowseForFolderW          = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW        = shell32.NewProc("SHGetPathFromIDListW")

	procCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	procCoTaskMemFree               = ole32.NewProc("CoTaskMemFree")
	procCoUninitialize              = ole32.NewProc("CoUninitialize")
)

const (
	logicalWinWidth  = 840
	logicalWinHeight = 510
	logicalRadius    = 16

	WS_POPUP        = 0x80000000
	WS_CLIPCHILDREN = 0x02000000
	WS_SYSMENU      = 0x00080000
	WS_MINIMIZEBOX  = 0x00020000

	SW_SHOW     = 5
	SW_MINIMIZE = 6
	SW_SHOWNORMAL = 1

	WM_DESTROY       = 0x0002
	WM_MOVE          = 0x0003
	WM_SIZE          = 0x0005
	WM_ACTIVATE      = 0x0006
	WM_CLOSE         = 0x0010
	WM_NCLBUTTONDOWN = 0x00A1
	WM_MOVING        = 0x0216
	WM_DPICHANGED    = 0x02E0
	WM_USER          = 0x0400

	WM_APP_DRAG     = WM_USER + 10
	WM_APP_MINIMIZE = WM_USER + 11
	WM_APP_BROWSE   = WM_USER + 12

	HTCAPTION   = 2
	WA_INACTIVE = 0

	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010

	SM_CXSCREEN = 0
	SM_CYSCREEN = 1

	IMAGE_ICON     = 1
	LR_DEFAULTSIZE = 0x0040
	LR_SHARED      = 0x8000
	IDC_ARROW      = 32512

	DWMWA_WINDOW_CORNER_PREFERENCE = 33
	DWMWCP_ROUND                   = 2
	DWMWA_USE_IMMERSIVE_DARK_MODE  = 20
)

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type POINT struct {
	X int32
	Y int32
}

type RECT struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type MSG struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       POINT
	LPrivate uint32
}

type BROWSEINFO struct {
	HwndOwner      uintptr
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	_              uint32
	Lpfn           uintptr
	LParam         uintptr
	IMessage       int32
	_              uint32
}

type DesktopApp struct {
	hwnd             uintptr
	chromium         *edge.Chromium
	server           *AppServer
	win11RoundActive bool
	browseResCh      chan string
}

var gDesktop *DesktopApp

type webMsg struct {
	Action string `json:"action"`
}

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func enableDPIAwareness() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (DPI_AWARENESS_CONTEXT)-4
	if procSetProcessDpiAwarenessCtx.Find() == nil {
		r, _, _ := procSetProcessDpiAwarenessCtx.Call(^uintptr(3))
		if r != 0 {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
}

func getDPI(hwnd uintptr) int32 {
	if hwnd != 0 && procGetDpiForWindow.Find() == nil {
		dpi, _, _ := procGetDpiForWindow.Call(hwnd)
		if dpi >= 96 {
			return int32(dpi)
		}
	}
	if procGetDpiForSystem.Find() == nil {
		dpi, _, _ := procGetDpiForSystem.Call()
		if dpi >= 96 {
			return int32(dpi)
		}
	}
	return 96
}

func loadAppIcon(hInstance uintptr) uintptr {
	// akavel/rsrc assigns ID 2 when -manifest is also present, or ID 1 otherwise
	for _, id := range []uintptr{2, 1} {
		h, _, _ := procLoadImageW.Call(hInstance, id, IMAGE_ICON, 0, 0, LR_DEFAULTSIZE|LR_SHARED)
		if h != 0 {
			return h
		}
	}
	h, _, _ := procLoadImageW.Call(0, 32512, IMAGE_ICON, 0, 0, LR_DEFAULTSIZE|LR_SHARED)
	return h
}

func (a *DesktopApp) applyRoundedCorners(w, h, dpi int32) {
	// Enable dark frame attributes where supported
	dark := uint32(1)
	if procDwmSetWindowAttribute.Find() == nil {
		procDwmSetWindowAttribute.Call(a.hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, uintptr(unsafe.Pointer(&dark)), 4)

		pref := uint32(DWMWCP_ROUND)
		hr, _, _ := procDwmSetWindowAttribute.Call(a.hwnd, DWMWA_WINDOW_CORNER_PREFERENCE, uintptr(unsafe.Pointer(&pref)), 4)
		if hr == 0 {
			a.win11RoundActive = true
			return
		}
	}

	// Windows 10 fallback: crisp rounded region matching 16px logical radius
	r := int32(logicalRadius * dpi / 96)
	if r < 12 {
		r = 12
	}
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(h+1), uintptr(r*2), uintptr(r*2))
	if rgn != 0 {
		procSetWindowRgn.Call(a.hwnd, rgn, 1)
	}
}

func desktopWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	app := gDesktop
	if app != nil && hwnd == app.hwnd {
		switch msg {
		case WM_APP_DRAG:
			procReleaseCapture.Call()
			procSendMessageW.Call(hwnd, WM_NCLBUTTONDOWN, HTCAPTION, 0)
			return 0

		case WM_APP_MINIMIZE:
			procShowWindow.Call(hwnd, SW_MINIMIZE)
			return 0

		case WM_APP_BROWSE:
			selected := showFolderDialog(hwnd)
			select {
			case app.browseResCh <- selected:
			default:
			}
			return 0

		case WM_MOVE, WM_MOVING:
			if app.chromium != nil {
				_ = app.chromium.NotifyParentWindowPositionChanged()
			}

		case WM_SIZE:
			if app.chromium != nil {
				app.chromium.Resize()
			}
			return 0

		case WM_DPICHANGED:
			newDPI := int32(wparam & 0xFFFF)
			if newDPI >= 96 {
				suggested := (*RECT)(unsafe.Pointer(lparam))
				physW := int32(logicalWinWidth * newDPI / 96)
				physH := int32(logicalWinHeight * newDPI / 96)
				procSetWindowPos.Call(
					hwnd, 0,
					uintptr(suggested.Left), uintptr(suggested.Top),
					uintptr(physW), uintptr(physH),
					SWP_NOZORDER|SWP_NOACTIVATE,
				)
				if !app.win11RoundActive {
					app.applyRoundedCorners(physW, physH, newDPI)
				}
				if app.chromium != nil {
					app.chromium.Resize()
				}
			}
			return 0

		case WM_ACTIVATE:
			if wparam != WA_INACTIVE && app.chromium != nil {
				app.chromium.Focus()
			}

		case WM_CLOSE:
			if app.server != nil {
				app.server.StopActiveDownload()
			}
			procDestroyWindow.Call(hwnd)
			return 0

		case WM_DESTROY:
			procPostQuitMessage.Call(0)
			return 0
		}
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

func showFolderDialog(owner uintptr) string {
	hr, _, _ := procCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED
	if hr == 0 || hr == 1 {
		defer procCoUninitialize.Call()
	}

	var bi BROWSEINFO
	bi.HwndOwner = owner
	bi.LpszTitle = utf16Ptr("請選擇動畫瘋影片儲存目錄")
	bi.UlFlags = 0x0001 | 0x0040 // BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE

	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)

	var pathBuf [260]uint16
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(pathBuf[:])
}

func (a *DesktopApp) BrowseFolder() string {
	if a.hwnd != 0 {
		// Drain stale result if any
		select {
		case <-a.browseResCh:
		default:
		}
		procPostMessageW.Call(a.hwnd, WM_APP_BROWSE, 0, 0)
		return <-a.browseResCh
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return showFolderDialog(0)
}

func RunDesktopWindow(srv *AppServer) error {
	runtime.LockOSThread()
	enableDPIAwareness()

	app := &DesktopApp{
		server:      srv,
		browseResCh: make(chan string, 1),
	}
	gDesktop = app
	srv.BrowseFolderFn = app.BrowseFolder

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	hIcon := loadAppIcon(hInstance)
	hCursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	// #060911 -> BGR = 0x00110906
	hBrush, _, _ := procCreateSolidBrush.Call(0x00110906)

	className := utf16Ptr("AniGamerLiquidGlassWindow")
	var wc WNDCLASSEXW
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.Style = 0x0002 | 0x0001 // CS_HREDRAW | CS_VREDRAW
	wc.LpfnWndProc = syscall.NewCallback(desktopWndProc)
	wc.HInstance = hInstance
	wc.HIcon = hIcon
	wc.HIconSm = hIcon
	wc.HCursor = hCursor
	wc.HbrBackground = hBrush
	wc.LpszClassName = className

	if ret, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return fmt.Errorf("註冊視窗類別失敗")
	}

	dpi := getDPI(0)
	physW := int32(logicalWinWidth * dpi / 96)
	physH := int32(logicalWinHeight * dpi / 96)

	screenW, _, _ := procGetSystemMetrics.Call(SM_CXSCREEN)
	screenH, _, _ := procGetSystemMetrics.Call(SM_CYSCREEN)
	posX := (int32(screenW) - physW) / 2
	posY := (int32(screenH) - physH) / 2
	if posX < 0 {
		posX = 40
	}
	if posY < 0 {
		posY = 40
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("動畫瘋 m3u8 影片下載器"))),
		uintptr(WS_POPUP|WS_MINIMIZEBOX|WS_SYSMENU|WS_CLIPCHILDREN),
		uintptr(posX), uintptr(posY), uintptr(physW), uintptr(physH),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("建立主視窗失敗")
	}
	app.hwnd = hwnd

	// Refine DPI with actual window handle
	actualDPI := getDPI(hwnd)
	if actualDPI != dpi {
		dpi = actualDPI
		physW = int32(logicalWinWidth * dpi / 96)
		physH = int32(logicalWinHeight * dpi / 96)
		posX = (int32(screenW) - physW) / 2
		posY = (int32(screenH) - physH) / 2
		procSetWindowPos.Call(hwnd, 0, uintptr(posX), uintptr(posY), uintptr(physW), uintptr(physH), SWP_NOZORDER|SWP_NOACTIVATE)
	}
	app.applyRoundedCorners(physW, physH, dpi)

	// Prepare WebView2 data directory under %LOCALAPPDATA%\AniDownloader\WebView2
	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "AniDownloader", "WebView2")
	if os.Getenv("LOCALAPPDATA") == "" {
		dataDir = filepath.Join(os.TempDir(), "AniDownloader", "WebView2")
	}
	_ = os.MkdirAll(dataDir, 0755)

	chromium := edge.NewChromium()
	chromium.DataPath = dataDir
	chromium.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)
	chromium.MessageCallback = func(raw string) {
		var m webMsg
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return
		}
		switch m.Action {
		case "drag":
			procPostMessageW.Call(hwnd, WM_APP_DRAG, 0, 0)
		case "minimize":
			procPostMessageW.Call(hwnd, WM_APP_MINIMIZE, 0, 0)
		case "close":
			procPostMessageW.Call(hwnd, WM_CLOSE, 0, 0)
		}
	}

	app.chromium = chromium

	if !chromium.Embed(hwnd) {
		// Graceful fallback if WebView2 Runtime is unavailable on the host system
		procDestroyWindow.Call(hwnd)
		app.hwnd = 0
		launchBrowserFallback(srv.BaseURL)
		return nil
	}

	// Set dark #060911 default WebView2 background to prevent any white flash
	if ctrl := chromium.GetController(); ctrl != nil {
		if ctrl2 := ctrl.GetICoreWebView2Controller2(); ctrl2 != nil {
			_ = ctrl2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 255, R: 6, G: 9, B: 17})
		}
	}

	if settings, err := chromium.GetSettings(); err == nil && settings != nil {
		_ = settings.PutAreDevToolsEnabled(false)
		_ = settings.PutIsStatusBarEnabled(false)
		_ = settings.PutIsZoomControlEnabled(false)
		_ = settings.PutIsPinchZoomEnabled(false)
		_ = settings.PutIsSwipeNavigationEnabled(false)
		_ = settings.PutAreDefaultContextMenusEnabled(true)
	}

	chromium.Resize()
	chromium.Navigate(srv.BaseURL)

	procShowWindow.Call(hwnd, SW_SHOW)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(hwnd)
	chromium.Focus()

	var msg MSG
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return nil
}

func launchBrowserFallback(url string) {
	// Try Microsoft Edge or Chrome in standalone --app window mode
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
	}
	for _, bin := range candidates {
		if fileExistsNonEmpty(bin) {
			cmd := exec.Command(bin, fmt.Sprintf("--app=%s", url), "--window-size=880,550")
			if err := cmd.Start(); err == nil {
				_ = cmd.Wait()
				return
			}
		}
	}
	// Ultimate fallback: default browser
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))), uintptr(unsafe.Pointer(utf16Ptr(url))), 0, 0, SW_SHOWNORMAL)
	select {}
}
