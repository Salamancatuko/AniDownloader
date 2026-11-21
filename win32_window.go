//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
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
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	procGetModuleHandleW           = kernel32.NewProc("GetModuleHandleW")
	procMultiByteToWideChar        = kernel32.NewProc("MultiByteToWideChar")
	procRtlMoveMemory              = kernel32.NewProc("RtlMoveMemory")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procDestroyWindow              = user32.NewProc("DestroyWindow")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procUpdateWindow               = user32.NewProc("UpdateWindow")
	procSetFocus                   = user32.NewProc("SetFocus")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procTranslateMessage           = user32.NewProc("TranslateMessage")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procSendMessageW               = user32.NewProc("SendMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procReleaseCapture             = user32.NewProc("ReleaseCapture")
	procSetWindowPos               = user32.NewProc("SetWindowPos")
	procSetWindowRgn               = user32.NewProc("SetWindowRgn")
	procGetSystemMetrics           = user32.NewProc("GetSystemMetrics")
	procLoadImageW                 = user32.NewProc("LoadImageW")
	procLoadCursorW                = user32.NewProc("LoadCursorW")
	procSetProcessDPIAware         = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessCtx  = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForWindow            = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem            = user32.NewProc("GetDpiForSystem")
	procMessageBoxW                = user32.NewProc("MessageBoxW")
	procSetWindowCompositionAttrib = user32.NewProc("SetWindowCompositionAttribute")

	procGetStockObject     = gdi32.NewProc("GetStockObject")
	procCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")

	procDwmSetWindowAttribute        = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmExtendFrameIntoClientArea = dwmapi.NewProc("DwmExtendFrameIntoClientArea")

	procShellExecuteW        = shell32.NewProc("ShellExecuteW")
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")

	procGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
)

const (
	logicalWinWidth  = 840
	logicalWinHeight = 510
	logicalRadius    = 16

	WS_POPUP        = 0x80000000
	WS_CLIPCHILDREN = 0x02000000
	WS_SYSMENU      = 0x00080000
	WS_MINIMIZEBOX  = 0x00020000

	SW_SHOWNORMAL = 1
	SW_SHOW       = 5
	SW_MINIMIZE   = 6

	MB_OK              = 0x00000000
	MB_ICONERROR       = 0x00000010
	MB_ICONINFORMATION = 0x00000040

	WM_DESTROY               = 0x0002
	WM_MOVE                  = 0x0003
	WM_SIZE                  = 0x0005
	WM_ACTIVATE              = 0x0006
	WM_CLOSE                 = 0x0010
	WM_NCLBUTTONDOWN         = 0x00A1
	WM_MOVING                = 0x0216
	WM_DPICHANGED            = 0x02E0
	WM_DWMCOMPOSITIONCHANGED = 0x031E
	WM_USER                  = 0x0400

	WM_APP_DRAG     = WM_USER + 10
	WM_APP_MINIMIZE = WM_USER + 11
	WM_APP_BROWSE   = WM_USER + 12
	WM_APP_EVAL     = WM_USER + 13
	WM_APP_EXPORT   = WM_USER + 15

	OFN_OVERWRITEPROMPT = 0x00000002
	OFN_NOCHANGEDIR     = 0x00000008
	OFN_PATHMUSTEXIST   = 0x00000800
	OFN_EXPLORER        = 0x00080000

	HTCAPTION   = 2
	WA_INACTIVE = 0

	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010

	SM_CXSCREEN = 0
	SM_CYSCREEN = 1

	BLACK_BRUSH = 4

	IMAGE_ICON     = 1
	LR_DEFAULTSIZE = 0x0040
	LR_SHARED      = 0x8000
	IDC_ARROW      = 32512

	DWMWA_USE_IMMERSIVE_DARK_MODE  = 20
	DWMWA_WINDOW_CORNER_PREFERENCE = 33
	DWMWCP_ROUND                   = 2
	DWMWA_SYSTEMBACKDROP_TYPE      = 38
	DWMSBT_NONE                    = 1

	WCA_ACCENT_POLICY                 = 19
	ACCENT_ENABLE_TRANSPARENTGRADIENT = 2

	CLSCTX_INPROC_SERVER = 0x1
	FOS_NOCHANGEDIR      = 0x00000008
	FOS_PICKFOLDERS      = 0x00000020
	FOS_FORCEFILESYSTEM  = 0x00000040
	SIGDN_FILESYSPATH    = 0x80058000
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = guid{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = guid{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

type iFileOpenDialogVtbl struct {
	QueryInterface      uintptr
	AddRef              uintptr
	Release             uintptr
	Show                uintptr
	SetFileTypes        uintptr
	SetFileTypeIndex    uintptr
	GetFileTypeIndex    uintptr
	Advise              uintptr
	Unadvise            uintptr
	SetOptions          uintptr
	GetOptions          uintptr
	SetDefaultFolder    uintptr
	SetFolder           uintptr
	GetFolder           uintptr
	GetCurrentSelection uintptr
	SetFileName         uintptr
	GetFileName         uintptr
	SetTitle            uintptr
	SetOkButtonLabel    uintptr
	SetFileNameLabel    uintptr
	GetResult           uintptr
}

type iFileOpenDialog struct {
	vtbl *iFileOpenDialogVtbl
}

type iShellItemVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	BindToHandler  uintptr
	GetParent      uintptr
	GetDisplayName uintptr
}

type iShellItem struct {
	vtbl *iShellItemVtbl
}

type comWebView2ControllerVtbl struct {
	QueryInterface                    uintptr
	AddRef                            uintptr
	Release                           uintptr
	GetIsVisible                      uintptr
	PutIsVisible                      uintptr
	GetBounds                         uintptr
	PutBounds                         uintptr
	GetZoomFactor                     uintptr
	PutZoomFactor                     uintptr
	AddZoomFactorChanged              uintptr
	RemoveZoomFactorChanged           uintptr
	SetBoundsAndZoomFactor            uintptr
	MoveFocus                         uintptr
	AddMoveFocusRequested             uintptr
	RemoveMoveFocusRequested          uintptr
	AddGotFocus                       uintptr
	RemoveGotFocus                    uintptr
	AddLostFocus                      uintptr
	RemoveLostFocus                   uintptr
	AddAcceleratorKeyPressed          uintptr
	RemoveAcceleratorKeyPressed       uintptr
	GetParentWindow                   uintptr
	PutParentWindow                   uintptr
	NotifyParentWindowPositionChanged uintptr
	Close                             uintptr
	GetCoreWebView2                   uintptr
}

type comWebView2Controller struct {
	vtbl *comWebView2ControllerVtbl
}

func closeWebViewController(ctrl *edge.ICoreWebView2Controller) {
	if ctrl == nil {
		return
	}
	raw := (*comWebView2Controller)(unsafe.Pointer(ctrl))
	if raw.vtbl == nil || raw.vtbl.Close == 0 {
		return
	}
	_, _, _ = syscall.SyscallN(raw.vtbl.Close, uintptr(unsafe.Pointer(ctrl)))
}

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

type MARGINS struct {
	CxLeftWidth    int32
	CxRightWidth   int32
	CyTopHeight    int32
	CyBottomHeight int32
}

type ACCENT_POLICY struct {
	AccentState   uint32
	AccentFlags   uint32
	GradientColor uint32
	AnimationId   uint32
}

type WINDOWCOMPOSITIONATTRIBDATA struct {
	Attrib uint32
	_      uint32
	PvData uintptr
	CbData uintptr
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

type OPENFILENAMEW struct {
	LStructSize       uint32
	HwndOwner         uintptr
	HInstance         uintptr
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        uintptr
	DwReserved        uint32
	FlagsEx           uint32
}

type DesktopApp struct {
	hwnd             uintptr
	chromium         *edge.Chromium
	server           *AppServer
	win11RoundActive bool
	pageReady        atomic.Bool
	closing          atomic.Bool
	browseResCh      chan string
	closedCh         chan struct{}
	closeOnce        sync.Once

	evalMu    sync.Mutex
	evalQueue []string

	exportMu      sync.Mutex
	exportContent string
}

var gDesktop *DesktopApp

type webMsg struct {
	Action         string     `json:"action"`
	Config         *AppConfig `json:"config,omitempty"`
	URL            string     `json:"url,omitempty"`
	Title          string     `json:"title,omitempty"`
	SaveDir        string     `json:"saveDir,omitempty"`
	Engine         string     `json:"engine,omitempty"`
	RefererEnabled bool       `json:"refererEnabled,omitempty"`
	UserAgent      string     `json:"userAgent,omitempty"`
	JA3            string     `json:"ja3,omitempty"`
	Akamai         string     `json:"akamai,omitempty"`
	Concurrency    int        `json:"concurrency,omitempty"`
	Content        string     `json:"content,omitempty"`
}

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var units []uint16
	for offset := uintptr(0); ; offset += 2 {
		ch := *(*uint16)(unsafe.Add(unsafe.Pointer(p), offset))
		if ch == 0 {
			break
		}
		units = append(units, ch)
	}
	return syscall.UTF16ToString(units)
}

func readRectFromLParam(lparam uintptr) RECT {
	var r RECT
	if procRtlMoveMemory.Find() == nil {
		procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&r)), lparam, unsafe.Sizeof(r))
	}
	return r
}

func showErrorMessageBox(title, msg string) {
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(msg))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		MB_OK|MB_ICONERROR,
	)
}

// decodeConsoleBytes ensures subprocess output from yt-dlp / ffmpeg on Windows
// (which may be UTF-8 or system ANSI code page such as GBK/CP936 or Big5/CP950)
// is always decoded cleanly to valid UTF-8 without garbled characters.
func decodeConsoleBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if procMultiByteToWideChar.Find() == nil {
		n, _, _ := procMultiByteToWideChar.Call(
			0, // CP_ACP (current Windows ANSI code page)
			0,
			uintptr(unsafe.Pointer(&b[0])),
			uintptr(len(b)),
			0,
			0,
		)
		if n > 0 {
			wbuf := make([]uint16, n)
			n2, _, _ := procMultiByteToWideChar.Call(
				0,
				0,
				uintptr(unsafe.Pointer(&b[0])),
				uintptr(len(b)),
				uintptr(unsafe.Pointer(&wbuf[0])),
				n,
			)
			if n2 > 0 {
				return syscall.UTF16ToString(wbuf[:n2])
			}
		}
	}
	return string(b)
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
	for _, id := range []uintptr{2, 1} {
		h, _, _ := procLoadImageW.Call(hInstance, id, IMAGE_ICON, 0, 0, LR_DEFAULTSIZE|LR_SHARED)
		if h != 0 {
			return h
		}
	}
	h, _, _ := procLoadImageW.Call(0, 32512, IMAGE_ICON, 0, 0, LR_DEFAULTSIZE|LR_SHARED)
	return h
}

func (a *DesktopApp) getHwnd() uintptr {
	a.evalMu.Lock()
	hwnd := a.hwnd
	a.evalMu.Unlock()
	return hwnd
}

func (a *DesktopApp) getChromium() *edge.Chromium {
	a.evalMu.Lock()
	chromium := a.chromium
	a.evalMu.Unlock()
	return chromium
}

func (a *DesktopApp) applyGlassBackdrop() {
	hwnd := a.getHwnd()
	if hwnd == 0 {
		return
	}

	// Ensure Windows 11 system backdrop is NONE so it does not paint an opaque grey fallback on WS_POPUP / unfocused windows
	if procDwmSetWindowAttribute.Find() == nil {
		none := uint32(DWMSBT_NONE)
		procDwmSetWindowAttribute.Call(hwnd, DWMWA_SYSTEMBACKDROP_TYPE, uintptr(unsafe.Pointer(&none)), 4)
	}

	// Extend DWM glass frame over the entire client area so WebView2 Alpha=0 is truly transparent
	if procDwmExtendFrameIntoClientArea.Find() == nil {
		margins := MARGINS{-1, -1, -1, -1}
		procDwmExtendFrameIntoClientArea.Call(hwnd, uintptr(unsafe.Pointer(&margins)))
	}

	// Pure crystal-clear transparent glass (NO blur, NO black background)
	if procSetWindowCompositionAttrib.Find() == nil {
		policy := ACCENT_POLICY{
			AccentState:   ACCENT_ENABLE_TRANSPARENTGRADIENT,
			AccentFlags:   2,
			GradientColor: 0x01000000,
		}
		data := WINDOWCOMPOSITIONATTRIBDATA{
			Attrib: WCA_ACCENT_POLICY,
			PvData: uintptr(unsafe.Pointer(&policy)),
			CbData: unsafe.Sizeof(policy),
		}
		procSetWindowCompositionAttrib.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	}
}

func (a *DesktopApp) applyRoundedCorners(w, h, dpi int32) {
	hwnd := a.getHwnd()
	if hwnd == 0 {
		return
	}
	dark := uint32(1)
	if procDwmSetWindowAttribute.Find() == nil {
		procDwmSetWindowAttribute.Call(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, uintptr(unsafe.Pointer(&dark)), 4)

		pref := uint32(DWMWCP_ROUND)
		hr, _, _ := procDwmSetWindowAttribute.Call(hwnd, DWMWA_WINDOW_CORNER_PREFERENCE, uintptr(unsafe.Pointer(&pref)), 4)
		if hr == 0 {
			a.win11RoundActive = true
			return
		}
	}

	r := int32(logicalRadius * dpi / 96)
	if r < 12 {
		r = 12
	}
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(h+1), uintptr(r*2), uintptr(r*2))
	if rgn != 0 {
		procSetWindowRgn.Call(hwnd, rgn, 1)
	}
}

func (a *DesktopApp) EvalJS(js string) {
	if a.closing.Load() {
		return
	}
	a.evalMu.Lock()
	hwnd := a.hwnd
	chromium := a.chromium
	if hwnd == 0 || chromium == nil || a.closing.Load() {
		a.evalMu.Unlock()
		return
	}
	a.evalQueue = append(a.evalQueue, js)
	a.evalMu.Unlock()

	procPostMessageW.Call(hwnd, WM_APP_EVAL, 0, 0)
}

func (a *DesktopApp) EmitToWeb(event string, jsonPayload string) {
	if a.closing.Load() || !a.pageReady.Load() {
		return
	}
	evBytes, _ := json.Marshal(event)
	js := fmt.Sprintf("window.__nativeEmit && window.__nativeEmit(%s, %s);", string(evBytes), jsonPayload)
	a.EvalJS(js)
}

func (a *DesktopApp) PushInitState() {
	if a.server == nil || a.closing.Load() {
		return
	}
	state := a.server.GetInitState()
	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	a.EmitToWeb("init", string(b))
}

func desktopWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	app := gDesktop
	if app != nil && hwnd == app.getHwnd() {
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
			if selected != "" && app.server != nil {
				app.server.SetSaveDir(selected)
				b, _ := json.Marshal(map[string]string{"path": selected})
				app.EmitToWeb("browse_result", string(b))
			}
			select {
			case app.browseResCh <- selected:
			default:
			}
			return 0

		case WM_APP_EXPORT:
			app.exportMu.Lock()
			content := app.exportContent
			app.exportContent = ""
			app.exportMu.Unlock()

			initialDir := ""
			if app.server != nil {
				initialDir = app.server.GetConfig().SaveDir
			}

			targetPath := showSaveLogDialog(hwnd, initialDir)
			if targetPath != "" {
				// Write UTF-8 with BOM so Windows Notepad & editors display Chinese without garbling
				data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(content)...)
				if err := os.WriteFile(targetPath, data, 0644); err != nil {
					b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					app.EmitToWeb("export_result", string(b))
				} else {
					b, _ := json.Marshal(map[string]any{"ok": true, "path": targetPath})
					app.EmitToWeb("export_result", string(b))
				}
			}
			return 0

		case WM_APP_EVAL:
			if app.closing.Load() {
				return 0
			}
			app.evalMu.Lock()
			q := app.evalQueue
			app.evalQueue = nil
			chromium := app.chromium
			app.evalMu.Unlock()
			if chromium != nil && !app.closing.Load() {
				for _, js := range q {
					chromium.Eval(js)
				}
			}
			return 0

		case WM_MOVE, WM_MOVING:
			if chromium := app.getChromium(); chromium != nil && !app.closing.Load() {
				_ = chromium.NotifyParentWindowPositionChanged()
			}

		case WM_SIZE:
			if chromium := app.getChromium(); chromium != nil && !app.closing.Load() {
				chromium.Resize()
			}
			return 0

		case WM_DPICHANGED:
			newDPI := int32(wparam & 0xFFFF)
			if newDPI >= 96 {
				var suggested RECT
				if lparam != 0 {
					// Copy RECT via unsafe.Slice-free field read using GetWindowRect-safe pattern,
					// or read 4 int32s from lparam via reflect/unsafe helper to keep go vet clean.
					suggested = readRectFromLParam(lparam)
				}
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
				if chromium := app.getChromium(); chromium != nil && !app.closing.Load() {
					chromium.Resize()
				}
			}
			return 0

		case WM_DWMCOMPOSITIONCHANGED:
			app.applyGlassBackdrop()
			return 0

		case WM_ACTIVATE:
			if wparam != WA_INACTIVE && !app.closing.Load() {
				if chromium := app.getChromium(); chromium != nil {
					chromium.Focus()
				}
			}

		case WM_CLOSE:
			app.closing.Store(true)
			app.evalMu.Lock()
			chromium := app.chromium
			app.chromium = nil
			app.evalQueue = nil
			app.evalMu.Unlock()
			if app.server != nil {
				app.server.SetNativeEmitFn(nil)
				app.server.StopActiveDownload()
			}
			if chromium != nil {
				if ctrl := chromium.GetController(); ctrl != nil {
					closeWebViewController(ctrl)
				}
			}
			procDestroyWindow.Call(hwnd)
			return 0

		case WM_DESTROY:
			app.closing.Store(true)
			app.evalMu.Lock()
			app.hwnd = 0
			app.evalQueue = nil
			app.evalMu.Unlock()
			app.closeOnce.Do(func() {
				close(app.closedCh)
			})
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

	if path, ok := showFolderDialogVista(owner); ok {
		return path
	}
	return showFolderDialogLegacy(owner)
}

func showFolderDialogVista(owner uintptr) (string, bool) {
	if procCoCreateInstance.Find() != nil {
		return "", false
	}

	var dlg *iFileOpenDialog
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
		0,
		CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
		uintptr(unsafe.Pointer(&dlg)),
	)
	if hr != 0 || dlg == nil {
		return "", false
	}
	defer syscall.SyscallN(dlg.vtbl.Release, uintptr(unsafe.Pointer(dlg)))

	var opts uint32
	hr, _, _ = syscall.SyscallN(dlg.vtbl.GetOptions, uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(&opts)))
	if hr == 0 {
		opts |= FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM | FOS_NOCHANGEDIR
		_, _, _ = syscall.SyscallN(dlg.vtbl.SetOptions, uintptr(unsafe.Pointer(dlg)), uintptr(opts))
	}

	titlePtr := utf16Ptr("請選擇動畫瘋影片儲存目錄")
	_, _, _ = syscall.SyscallN(dlg.vtbl.SetTitle, uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(titlePtr)))

	hr, _, _ = syscall.SyscallN(dlg.vtbl.Show, uintptr(unsafe.Pointer(dlg)), owner)
	if hr != 0 {
		// User cancelled or closed the dialog: do NOT fall back to legacy dialog.
		return "", true
	}

	var item *iShellItem
	hr, _, _ = syscall.SyscallN(dlg.vtbl.GetResult, uintptr(unsafe.Pointer(dlg)), uintptr(unsafe.Pointer(&item)))
	if hr != 0 || item == nil {
		return "", true
	}
	defer syscall.SyscallN(item.vtbl.Release, uintptr(unsafe.Pointer(item)))

	var pathPtr *uint16
	hr, _, _ = syscall.SyscallN(item.vtbl.GetDisplayName, uintptr(unsafe.Pointer(item)), SIGDN_FILESYSPATH, uintptr(unsafe.Pointer(&pathPtr)))
	if hr != 0 || pathPtr == nil {
		return "", true
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(pathPtr)))

	return utf16PtrToString(pathPtr), true
}

func showFolderDialogLegacy(owner uintptr) string {
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

func browseFolderStandalone() string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return showFolderDialog(0)
}

func showSaveLogDialog(owner uintptr, initialDir string) string {
	if procGetSaveFileNameW.Find() != nil {
		return ""
	}

	var fileBuf [1024]uint16
	defaultName := fmt.Sprintf("AniDownloader_Log_%s.txt", time.Now().Format("20060102_150405"))
	nameUTF16 := utf16.Encode([]rune(defaultName))
	copy(fileBuf[:], nameUTF16)

	filterUTF16 := utf16.Encode([]rune("日誌檔案 (*.txt;*.log)\x00*.txt;*.log\x00所有檔案 (*.*)\x00*.*\x00\x00"))
	titlePtr := utf16Ptr("導出執行日誌")
	defExtPtr := utf16Ptr("txt")

	var initDirPtr *uint16
	if initialDir != "" {
		if st, err := os.Stat(initialDir); err == nil && st.IsDir() {
			initDirPtr = utf16Ptr(initialDir)
		}
	}

	var ofn OPENFILENAMEW
	ofn.LStructSize = uint32(unsafe.Sizeof(ofn))
	ofn.HwndOwner = owner
	ofn.LpstrFilter = &filterUTF16[0]
	ofn.NFilterIndex = 1
	ofn.LpstrFile = &fileBuf[0]
	ofn.NMaxFile = uint32(len(fileBuf))
	ofn.LpstrInitialDir = initDirPtr
	ofn.LpstrTitle = titlePtr
	ofn.LpstrDefExt = defExtPtr
	ofn.Flags = OFN_OVERWRITEPROMPT | OFN_PATHMUSTEXIST | OFN_NOCHANGEDIR | OFN_EXPLORER

	ok, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(fileBuf[:])
}

func (a *DesktopApp) BrowseFolder() string {
	if a.closing.Load() {
		return ""
	}
	hwnd := a.getHwnd()
	if hwnd != 0 {
		select {
		case <-a.browseResCh:
		default:
		}
		posted, _, _ := procPostMessageW.Call(hwnd, WM_APP_BROWSE, 0, 0)
		if posted == 0 {
			return ""
		}
		select {
		case res := <-a.browseResCh:
			return res
		case <-a.closedCh:
			return ""
		}
	}
	return browseFolderStandalone()
}

func (a *DesktopApp) handleWebMessage(raw string) {
	if a.closing.Load() {
		return
	}
	var m webMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return
	}
	switch m.Action {
	case "drag":
		if hwnd := a.getHwnd(); hwnd != 0 && !a.closing.Load() {
			procPostMessageW.Call(hwnd, WM_APP_DRAG, 0, 0)
		}
	case "minimize":
		if hwnd := a.getHwnd(); hwnd != 0 && !a.closing.Load() {
			procPostMessageW.Call(hwnd, WM_APP_MINIMIZE, 0, 0)
		}
	case "close":
		if hwnd := a.getHwnd(); hwnd != 0 && !a.closing.Load() {
			procPostMessageW.Call(hwnd, WM_CLOSE, 0, 0)
		}
	case "init":
		a.pageReady.Store(true)
		a.PushInitState()
	case "save_config":
		if m.Config != nil && a.server != nil {
			a.server.UpdateConfig(*m.Config)
		}
	case "browse":
		if hwnd := a.getHwnd(); hwnd != 0 && !a.closing.Load() {
			procPostMessageW.Call(hwnd, WM_APP_BROWSE, 0, 0)
		}
	case "clear_logs":
		if a.server != nil {
			a.server.ClearLogs()
		}
	case "export_logs":
		a.exportMu.Lock()
		a.exportContent = m.Content
		a.exportMu.Unlock()
		if hwnd := a.getHwnd(); hwnd != 0 && !a.closing.Load() {
			procPostMessageW.Call(hwnd, WM_APP_EXPORT, 0, 0)
		}
	case "download":
		if srv := a.server; srv != nil {
			req := downloadRequest{
				URL:            m.URL,
				Title:          m.Title,
				SaveDir:        m.SaveDir,
				Engine:         m.Engine,
				RefererEnabled: m.RefererEnabled,
				UserAgent:      m.UserAgent,
				JA3:            m.JA3,
				Akamai:         m.Akamai,
				Concurrency:    DefaultConcurrency,
			}
			go func() {
				if a.closing.Load() {
					return
				}
				if err := srv.StartDownloadTask(req); err != nil {
					if a.closing.Load() {
						return
					}
					payload := map[string]string{
						"engine":  req.Engine,
						"message": err.Error(),
					}
					if active := srv.ActiveEngine(); active != "" {
						payload["activeEngine"] = active
					}
					b, _ := json.Marshal(payload)
					a.EmitToWeb("error_event", string(b))
				}
			}()
		}
	case "stop":
		if a.server != nil {
			a.server.StopActiveDownload()
		}
	case "update_ytdlp":
		if a.server != nil {
			if err := a.server.StartYtDlpUpdate(); err != nil {
				b, _ := json.Marshal(map[string]interface{}{
					"ok":    false,
					"error": err.Error(),
				})
				a.EmitToWeb("ytdlp_update_result", string(b))
			}
		}
	}
}

func RunDesktopWindow(srv *AppServer) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Initialize STA COM on the locked UI thread (required by WebView2)
	hr, _, _ := procCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED
	if hr == 0 || hr == 1 {
		defer procCoUninitialize.Call()
	}

	enableDPIAwareness()

	app := &DesktopApp{
		server:      srv,
		browseResCh: make(chan string, 1),
		closedCh:    make(chan struct{}),
	}
	gDesktop = app
	srv.SetBrowseFolderFn(app.BrowseFolder)

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	hIcon := loadAppIcon(hInstance)
	hCursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	// BLACK_BRUSH (0x00000000) renders as Alpha=0 transparent when DWM glass frame is extended
	hBrush, _, _ := procGetStockObject.Call(BLACK_BRUSH)

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
	app.evalMu.Lock()
	app.hwnd = hwnd
	app.evalMu.Unlock()

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
	app.applyGlassBackdrop()

	// Show and update the native window BEFORE embedding WebView2
	procShowWindow.Call(hwnd, SW_SHOW)
	procUpdateWindow.Call(hwnd)
	procSetFocus.Call(hwnd)

	// Re-apply DWM transparent backdrop after window is shown so Windows 11 DWM binds it to the active composition surface
	app.applyGlassBackdrop()

	// Ensure WebView2 initializes with Alpha=0 transparent background from the very first frame
	_ = os.Setenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR", "00000000")

	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "AniDownloader", "WebView2")
	if os.Getenv("LOCALAPPDATA") == "" {
		dataDir = filepath.Join(os.TempDir(), "AniDownloader", "WebView2")
	}
	_ = os.MkdirAll(dataDir, 0755)

	chromium := edge.NewChromium()
	chromium.DataPath = dataDir
	chromium.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)
	chromium.MessageCallback = app.handleWebMessage
	chromium.NavigationCompletedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		if app.closing.Load() {
			return
		}
		if ctrl := chromium.GetController(); ctrl != nil {
			if ctrl2 := ctrl.GetICoreWebView2Controller2(); ctrl2 != nil {
				_ = ctrl2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 0, R: 0, G: 0, B: 0})
			}
		}
		app.applyGlassBackdrop()
		chromium.Resize()
		_ = chromium.Show()
	}

	app.evalMu.Lock()
	app.chromium = chromium
	app.evalMu.Unlock()

	if !chromium.Embed(hwnd) {
		app.closing.Store(true)
		app.evalMu.Lock()
		app.chromium = nil
		app.evalQueue = nil
		app.evalMu.Unlock()
		procDestroyWindow.Call(hwnd)
		app.evalMu.Lock()
		app.hwnd = 0
		app.evalMu.Unlock()
		app.closeOnce.Do(func() {
			close(app.closedCh)
		})
		srv.SetNativeEmitFn(nil)
		srv.SetBrowseFolderFn(browseFolderStandalone)

		if err := srv.Start(); err != nil {
			showErrorMessageBox("啟動錯誤", fmt.Sprintf("啟動本機服務失敗: %v", err))
			return nil
		}
		launchBrowserFallback(srv.GetBaseURL())
		return nil
	}

	srv.SetNativeEmitFn(app.EmitToWeb)

	// Fully transparent WebView2 background (Alpha = 0) so the real Windows desktop shows through
	if ctrl := chromium.GetController(); ctrl != nil {
		if ctrl2 := ctrl.GetICoreWebView2Controller2(); ctrl2 != nil {
			_ = ctrl2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 0, R: 0, G: 0, B: 0})
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
	_ = chromium.Show()
	chromium.NavigateToString(string(embeddedIndexHTML))
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

func showFallbackKeepAliveBox(url string) {
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("已於預設瀏覽器開啟 AniDownloader：\n%s\n\n點擊「確定」或關閉此提示視窗即可結束本機背景服務。", url)))),
		uintptr(unsafe.Pointer(utf16Ptr("AniDownloader 瀏覽器模式"))),
		MB_OK|MB_ICONINFORMATION,
	)
}

func launchBrowserFallback(url string) {
	profileBase := os.Getenv("LOCALAPPDATA")
	if profileBase == "" {
		profileBase = os.TempDir()
	}
	profileDir := filepath.Join(profileBase, "AniDownloader", "BrowserProfile")
	_ = os.MkdirAll(profileDir, 0755)

	var candidates []string
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		candidates = append(candidates,
			filepath.Join(pf86, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pf86, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		candidates = append(candidates,
			filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}

	for _, bin := range candidates {
		if fileExistsNonEmpty(bin) {
			cmd := exec.Command(
				bin,
				fmt.Sprintf("--user-data-dir=%s", profileDir),
				fmt.Sprintf("--app=%s", url),
				"--window-size=880,550",
			)
			startAt := time.Now()
			if err := cmd.Start(); err == nil {
				_ = cmd.Wait()
				if time.Since(startAt) < 2*time.Second {
					showFallbackKeepAliveBox(url)
				}
				return
			}
		}
	}
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))), uintptr(unsafe.Pointer(utf16Ptr(url))), 0, 0, SW_SHOWNORMAL)
	showFallbackKeepAliveBox(url)
}
