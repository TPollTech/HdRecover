//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"hdrecover/recovery"
)

const (
	appTitle   = "HdRecover 0.5.4-rc.1 — Recuperação, Clonagem e Migração Inteligente"
	appVersion = "0.5.4-rc.1"

	WM_CREATE          = 0x0001
	WM_DESTROY         = 0x0002
	WM_SIZE            = 0x0005
	WM_GETMINMAXINFO   = 0x0024
	WM_DPICHANGED      = 0x02E0
	WM_CLOSE           = 0x0010
	WM_COMMAND         = 0x0111
	WM_ERASEBKGND      = 0x0014
	WM_CTLCOLORMSGBOX  = 0x0132
	WM_CTLCOLOREDIT    = 0x0133
	WM_CTLCOLORLISTBOX = 0x0134
	WM_CTLCOLORBTN     = 0x0135
	WM_CTLCOLORDLG     = 0x0136
	WM_CTLCOLORSTATIC  = 0x0138
	WM_APP_UPDATE      = 0x8001

	WS_OVERLAPPED       = 0x00000000
	WS_CAPTION          = 0x00C00000
	WS_SYSMENU          = 0x00080000
	WS_MINIMIZEBOX      = 0x00020000
	WS_MAXIMIZEBOX      = 0x00010000
	WS_THICKFRAME       = 0x00040000
	WS_CLIPCHILDREN     = 0x02000000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
	WS_BORDER           = 0x00800000
	WS_VSCROLL          = 0x00200000
	WS_EX_CLIENTEDGE    = 0x00000200
	WS_EX_CONTROLPARENT = 0x00010000

	BS_PUSHBUTTON    = 0x00000000
	BS_DEFPUSHBUTTON = 0x00000001
	BS_AUTOCHECKBOX  = 0x00000003
	BST_UNCHECKED    = 0
	BST_CHECKED      = 1
	BM_GETCHECK      = 0x00F0
	BM_SETCHECK      = 0x00F1
	BN_CLICKED       = 0
	CBS_DROPDOWNLIST = 0x0003
	CBS_HASSTRINGS   = 0x0200
	CBN_SELCHANGE    = 1
	CB_ADDSTRING     = 0x0143
	CB_RESETCONTENT  = 0x014B
	CB_SETCURSEL     = 0x014E
	CB_GETCURSEL     = 0x0147
	ES_AUTOHSCROLL   = 0x0080
	ES_AUTOVSCROLL   = 0x0040
	ES_MULTILINE     = 0x0004
	ES_READONLY      = 0x0800
	EM_SETSEL        = 0x00B1
	EM_SCROLLCARET   = 0x00B7
	WM_SETFONT       = 0x0030
	PBM_SETRANGE32   = 0x0406
	PBM_SETPOS       = 0x0402
	PBM_SETBARCOLOR  = 0x0409
	PBM_SETBKCOLOR   = 0x2001

	MB_OK              = 0x00000000
	MB_YESNO           = 0x00000004
	MB_ICONERROR       = 0x00000010
	MB_ICONQUESTION    = 0x00000020
	MB_ICONWARNING     = 0x00000030
	MB_ICONINFORMATION = 0x00000040
	IDYES              = 6

	SW_HIDE        = 0
	SW_SHOWNORMAL  = 1
	SW_SHOW        = 5
	SIZE_MINIMIZED = 1
	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010
	IDC_ARROW      = 32512
	TRANSPARENT    = 1

	BIF_RETURNONLYFSDIRS = 0x0001
	BIF_EDITBOX          = 0x0010
	BIF_NEWDIALOGSTYLE   = 0x0040

	ID_DISK_COMBO          = 101
	ID_REFRESH             = 102
	ID_DEST_EDIT           = 103
	ID_BROWSE              = 104
	ID_IMAGES              = 105
	ID_DOCUMENTS           = 106
	ID_VIDEOS              = 107
	ID_AUDIO               = 108
	ID_ARCHIVES            = 109
	ID_START               = 110
	ID_CANCEL              = 111
	ID_OPEN_FOLDER         = 112
	ID_SELECT_ALL          = 113
	ID_AUTO_OPEN           = 114
	ID_PERFORMANCE         = 115
	ID_MODE                = 116
	ID_PARTITION           = 117
	ID_PROFILE             = 118
	ID_DAMAGE              = 119
	ID_PAUSE               = 120
	ID_RESUME              = 121
	ID_DEDUP               = 122
	ID_MIN_SIZE            = 123
	ID_RANGE_START         = 124
	ID_RANGE_END           = 125
	ID_OPEN_IMAGE          = 126
	ID_ONLY_FREE           = 127
	ID_TAB_RECOVERY        = 128
	ID_TAB_CLONE           = 129
	ID_CLONE_SOURCE        = 130
	ID_CLONE_TARGET        = 131
	ID_CLONE_REFRESH       = 132
	ID_CLONE_MODE          = 133
	ID_CLONE_DAMAGE        = 134
	ID_CLONE_PERFORMANCE   = 135
	ID_CLONE_VERIFY        = 136
	ID_CLONE_RESUME        = 137
	ID_CLONE_REPORT_EDIT   = 138
	ID_CLONE_REPORT_BROWSE = 139
	ID_CLONE_CONFIRM       = 140
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")
	uxtheme  = syscall.NewLazyDLL("uxtheme.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")

	procRegisterClassExW     = user32.NewProc("RegisterClassExW")
	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDefWindowProcW       = user32.NewProc("DefWindowProcW")
	procShowWindow           = user32.NewProc("ShowWindow")
	procUpdateWindow         = user32.NewProc("UpdateWindow")
	procGetMessageW          = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessageW     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procPostMessageW         = user32.NewProc("PostMessageW")
	procSendMessageW         = user32.NewProc("SendMessageW")
	procSetWindowTextW       = user32.NewProc("SetWindowTextW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procEnableWindow         = user32.NewProc("EnableWindow")
	procMessageBoxW          = user32.NewProc("MessageBoxW")
	procMessageBeep          = user32.NewProc("MessageBeep")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procLoadCursorW          = user32.NewProc("LoadCursorW")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	procGetClientRect        = user32.NewProc("GetClientRect")
	procMoveWindow           = user32.NewProc("MoveWindow")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procInvalidateRect       = user32.NewProc("InvalidateRect")
	procGetDpiForWindow      = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem      = user32.NewProc("GetDpiForSystem")
	procSetProcessDpiAware   = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetTextColor         = gdi32.NewProc("SetTextColor")
	procSetBkColor           = gdi32.NewProc("SetBkColor")
	procSetBkMode            = gdi32.NewProc("SetBkMode")

	procGetModuleHandleW     = kernel32.NewProc("GetModuleHandleW")
	procSetThreadExecState   = kernel32.NewProc("SetThreadExecutionState")
	procRtlMoveMemory        = kernel32.NewProc("RtlMoveMemory")
	procCreateFontW          = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush     = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	procShellExecuteW        = shell32.NewProc("ShellExecuteW")
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procIsUserAnAdmin        = shell32.NewProc("IsUserAnAdmin")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")
	procSetWindowTheme       = uxtheme.NewProc("SetWindowTheme")
	procDwmSetWindowAttr     = dwmapi.NewProc("DwmSetWindowAttribute")
	procGetOpenFileNameW     = comdlg32.NewProc("GetOpenFileNameW")
)

const (
	ES_CONTINUOUS      = 0x80000000
	ES_SYSTEM_REQUIRED = 0x00000001
)

var (
	colorBackground = rgb(14, 17, 22)
	colorSurface    = rgb(25, 30, 38)
	colorInput      = rgb(37, 44, 54)
	colorText       = rgb(248, 250, 252)
	colorMuted      = rgb(199, 207, 219)
	colorAccent     = rgb(87, 174, 255)
	colorWarning    = rgb(255, 207, 102)
)

type point struct{ X, Y int32 }
type rect struct {
	Left, Top, Right, Bottom int32
}

type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}
type initCommonControlsEx struct {
	Size uint32
	ICC  uint32
}
type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Callback    uintptr
	LParam      uintptr
	Image       int32
}
type openFileName struct {
	StructSize    uint32
	Owner         uintptr
	Instance      uintptr
	Filter        *uint16
	CustomFilter  *uint16
	MaxCustFilter uint32
	FilterIndex   uint32
	File          *uint16
	MaxFile       uint32
	FileTitle     *uint16
	MaxFileTitle  uint32
	InitialDir    *uint16
	Title         *uint16
	Flags         uint32
	FileOffset    uint16
	FileExtension uint16
	DefExt        *uint16
	CustData      uintptr
	Hook          uintptr
	TemplateName  *uint16
	ReservedPtr   uintptr
	Reserved      uint32
	FlagsEx       uint32
}

type appSettings struct {
	Destination      string `json:"destination"`
	AutoOpen         bool   `json:"auto_open"`
	Performance      int    `json:"performance"`
	Mode             int    `json:"mode"`
	Profile          int    `json:"profile"`
	Damage           int    `json:"damage"`
	Resume           bool   `json:"resume"`
	Dedup            bool   `json:"dedup"`
	MinSizeKB        int    `json:"min_size_kb"`
	OnlyFree         bool   `json:"only_free"`
	CloneReport      string `json:"clone_report"`
	CloneMode        int    `json:"clone_mode"`
	CloneDamage      int    `json:"clone_damage"`
	ClonePerformance int    `json:"clone_performance"`
	CloneVerify      bool   `json:"clone_verify"`
	CloneResume      bool   `json:"clone_resume"`
}

type uiState struct {
	mu                      sync.Mutex
	disks                   []diskInfo
	selectedDisk            int
	preferredDiskSelection  int
	disksLoading            bool
	partitions              []recovery.Partition
	partitionsLoading       bool
	partitionVersion        int
	appliedPartitionVersion int
	running                 bool
	paused                  bool
	cancel                  chan struct{}
	cancelOnce              sync.Once
	logHistory              []string
	logVersion              uint64
	progress                int
	status                  string
	files                   int
	outputDir               string
	completed               bool
	completion              string
	completionOK            bool
	diskVersion             int
	appliedVersion          int
	scanStarted             time.Time
	bytesScanned            int64
	totalBytes              int64
	speedBytes              float64
	eta                     time.Duration
	phase                   string
	candidatesFound         int
	candidatesProcessed     int
	totalCandidates         int
	readErrors              int
	autoOpenAfter           bool
	operationKind           string
}

type appUI struct {
	hwnd            uintptr
	font            uintptr
	smallFont       uintptr
	sectionFont     uintptr
	titleFont       uintptr
	statusFont      uintptr
	actionFont      uintptr
	logFont         uintptr
	dpi             int32
	backgroundBrush uintptr
	surfaceBrush    uintptr
	inputBrush      uintptr

	titleLabel         uintptr
	subtitleLabel      uintptr
	sourceSection      uintptr
	strategySection    uintptr
	modeLabel          uintptr
	partitionLabel     uintptr
	profileLabel       uintptr
	damageLabel        uintptr
	performanceLabel   uintptr
	destinationSection uintptr
	fileTypesSection   uintptr
	safetySection      uintptr
	minSizeLabel       uintptr
	rangeLabel         uintptr
	rangeToLabel       uintptr
	rangeHintLabel     uintptr
	diskCombo          uintptr
	refresh            uintptr
	openImage          uintptr
	diskInfo           uintptr
	destEdit           uintptr
	browse             uintptr
	destInfo           uintptr
	images             uintptr
	documents          uintptr
	videos             uintptr
	audio              uintptr
	archives           uintptr
	selectAll          uintptr
	autoOpen           uintptr
	performance        uintptr
	mode               uintptr
	partition          uintptr
	profile            uintptr
	damage             uintptr
	resume             uintptr
	dedup              uintptr
	onlyFree           uintptr
	minSize            uintptr
	rangeStart         uintptr
	rangeEnd           uintptr
	pauseBtn           uintptr
	warning            uintptr
	start              uintptr
	cancelBtn          uintptr
	openFolder         uintptr
	progress           uintptr
	status             uintptr
	stats              uintptr
	logEdit            uintptr
	allControls        []uintptr
	sectionLabels      map[uintptr]bool

	activeTab             int
	tabRecovery           uintptr
	tabClone              uintptr
	cloneSourceSection    uintptr
	cloneTargetSection    uintptr
	cloneOptionsSection   uintptr
	cloneReportSection    uintptr
	cloneSafetySection    uintptr
	cloneSource           uintptr
	cloneSourceInfo       uintptr
	cloneTarget           uintptr
	cloneTargetInfo       uintptr
	cloneRefresh          uintptr
	cloneModeLabel        uintptr
	cloneMode             uintptr
	cloneDamageLabel      uintptr
	cloneDamage           uintptr
	clonePerformanceLabel uintptr
	clonePerformance      uintptr
	cloneVerify           uintptr
	cloneResume           uintptr
	cloneReportEdit       uintptr
	cloneReportBrowse     uintptr
	cloneReportInfo       uintptr
	cloneConfirmLabel     uintptr
	cloneConfirm          uintptr
	cloneConfirmHint      uintptr
	cloneGuide            uintptr
	cloneSourceMap        []int
	cloneTargetMap        []int

	state uiState

	lastStatus     string
	lastStats      string
	lastProgress   int
	lastLogVersion uint64
	lastRunning    bool
	runningSet     bool
	lastLoading    bool
	loadingSet     bool
}

var (
	app               appUI
	uiUpdatePending   int32
	uiLastNotifyNanos int64
	wndProcCallback   uintptr
	startupLogMu      sync.Mutex
)

func main() {
	defer recoverFatal("inicialização principal")
	logStartup("Iniciando %s em %s/%s", appTitle, runtime.GOOS, runtime.GOARCH)
	enableProcessDPIAwareness()
	// A janela, a fila de mensagens e o COM precisam permanecer na mesma thread do Windows.
	// Sem isso, o runtime do Go pode migrar a goroutine principal e a interface aparentar travar.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !isAdmin() {
		logStartup("Processo sem elevação; solicitando UAC")
		if relaunchAsAdmin() {
			logStartup("Reabertura como administrador solicitada com sucesso")
			return
		}
		logStartup("Elevação recusada ou bloqueada; abrindo interface em modo limitado")
		messageBox(0, "A autorização de administrador foi recusada ou bloqueada. O HdRecover será aberto em modo limitado, mas não poderá ler nem gravar discos físicos até ser reiniciado como administrador.", appTitle, MB_OK|MB_ICONWARNING)
	}

	if procCoInitializeEx.Find() == nil {
		procCoInitializeEx.Call(0, 0x2)
		defer procCoUninitialize.Call()
	}

	icc := initCommonControlsEx{Size: uint32(unsafe.Sizeof(initCommonControlsEx{})), ICC: 0x00000020 | 0x00000004}
	if procInitCommonControlsEx.Find() == nil {
		procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))
	} else {
		logStartup("InitCommonControlsEx indisponível: %v", procInitCommonControlsEx.Find())
	}

	app.backgroundBrush, _, _ = procCreateSolidBrush.Call(colorBackground)
	app.surfaceBrush, _, _ = procCreateSolidBrush.Call(colorSurface)
	app.inputBrush, _, _ = procCreateSolidBrush.Call(colorInput)

	instance, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr("HdRecoverMainWindowV052")
	cursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	wndProcCallback = syscall.NewCallback(wndProc)
	wc := wndClassEx{
		Size:       uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:    wndProcCallback,
		Instance:   instance,
		Cursor:     cursor,
		Background: app.backgroundBrush,
		ClassName:  className,
	}
	if r, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		logStartup("RegisterClassExW falhou: %v", callErr)
		messageBox(0, fmt.Sprintf("Não foi possível registrar a janela do aplicativo.\n\nDetalhes: %v\nLog: %s", callErr, startupLogPath()), appTitle, MB_OK|MB_ICONERROR)
		return
	}
	logStartup("Classe de janela registrada")

	app.dpi = systemDPI()
	screenWRaw, _, _ := procGetSystemMetrics.Call(0)
	screenHRaw, _, _ := procGetSystemMetrics.Call(1)
	screenW, screenH := int32(screenWRaw), int32(screenHRaw)
	width := logicalToPixels(1280, app.dpi)
	height := logicalToPixels(900, app.dpi)
	if maxW := screenW * 94 / 100; width > maxW {
		width = maxW
	}
	if maxH := screenH * 92 / 100; height > maxH {
		height = maxH
	}
	x := (screenW - width) / 2
	y := (screenH - height) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	hwnd, _, createErr := procCreateWindowExW.Call(
		WS_EX_CONTROLPARENT,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr(appTitle))),
		WS_OVERLAPPED|WS_CAPTION|WS_SYSMENU|WS_MINIMIZEBOX|WS_MAXIMIZEBOX|WS_THICKFRAME|WS_CLIPCHILDREN,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		0, 0, instance, 0,
	)
	if hwnd == 0 {
		logStartup("CreateWindowExW falhou: %v", createErr)
		messageBox(0, fmt.Sprintf("Não foi possível abrir a interface do HdRecover.\n\nDetalhes: %v\nLog: %s", createErr, startupLogPath()), appTitle, MB_OK|MB_ICONERROR)
		return
	}
	logStartup("Janela principal criada")
	app.hwnd = hwnd
	enableDarkTitleBar(hwnd)
	procShowWindow.Call(hwnd, SW_SHOWNORMAL)
	procUpdateWindow.Call(hwnd)
	logStartup("Interface exibida; entrando no loop de mensagens")

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) (result uintptr) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logStartup("Pânico no procedimento de janela (mensagem 0x%X): %v\n%s", message, recovered, debug.Stack())
			result, _, _ = procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
		}
	}()
	return wndProcImpl(hwnd, message, wParam, lParam)
}

func wndProcImpl(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case WM_CREATE:
		// CreateWindowEx envia WM_CREATE antes de retornar para main().
		// Guardar o HWND aqui permite que atualizações assíncronas funcionem desde o início.
		app.hwnd = hwnd
		createControls(hwnd)
		goSafe("atualização inicial dos discos", refreshDisks)
		return 0

	case WM_SIZE:
		if wParam != SIZE_MINIMIZED {
			layoutControls(hwnd)
		}
		return 0

	case WM_GETMINMAXINFO:
		if lParam != 0 {
			var info minMaxInfo
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&info)), lParam, unsafe.Sizeof(info))
			screenW, _, _ := procGetSystemMetrics.Call(0)
			screenH, _, _ := procGetSystemMetrics.Call(1)
			minW := logicalToPixels(1040, currentDPI())
			minH := logicalToPixels(720, currentDPI())
			if limit := int32(screenW) - 32; minW > limit {
				minW = limit
			}
			if limit := int32(screenH) - 64; minH > limit {
				minH = limit
			}
			info.MinTrackSize = point{X: minW, Y: minH}
			procRtlMoveMemory.Call(lParam, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		}
		return 0

	case WM_DPICHANGED:
		newDPI := int32(wParam & 0xFFFF)
		if newDPI > 0 {
			app.dpi = newDPI
		}
		recreateFonts()
		if lParam != 0 {
			var suggested rect
			procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&suggested)), lParam, unsafe.Sizeof(suggested))
			procSetWindowPos.Call(hwnd, 0, uintptr(suggested.Left), uintptr(suggested.Top), uintptr(suggested.Right-suggested.Left), uintptr(suggested.Bottom-suggested.Top), SWP_NOZORDER|SWP_NOACTIVATE)
		}
		layoutControls(hwnd)
		return 0

	case WM_COMMAND:
		id := int(wParam & 0xFFFF)
		notification := int((wParam >> 16) & 0xFFFF)
		switch id {
		case ID_TAB_RECOVERY:
			if notification == BN_CLICKED {
				switchTab(0)
			}
		case ID_TAB_CLONE:
			if notification == BN_CLICKED {
				switchTab(1)
			}
		case ID_DISK_COMBO:
			if notification == CBN_SELCHANGE {
				updateSelectedDiskInfo()
				goSafe("atualização das partições", refreshPartitions)
			}
		case ID_CLONE_SOURCE, ID_CLONE_TARGET:
			if notification == CBN_SELCHANGE {
				updateCloneDiskInfo()
			}
		case ID_REFRESH, ID_CLONE_REFRESH:
			if notification == BN_CLICKED {
				goSafe("atualização dos discos", refreshDisks)
			}
		case ID_OPEN_IMAGE:
			if notification == BN_CLICKED {
				openSourceImage()
			}
		case ID_BROWSE:
			if notification == BN_CLICKED {
				if path := browseFolder(hwnd); path != "" {
					setText(app.destEdit, path)
					updateDestinationInfo(path)
					saveSettings()
				}
			}
		case ID_CLONE_REPORT_BROWSE:
			if notification == BN_CLICKED {
				if path := browseFolder(hwnd); path != "" {
					setText(app.cloneReportEdit, path)
					saveSettings()
				}
			}
		case ID_SELECT_ALL:
			if notification == BN_CLICKED {
				checked := isChecked(app.selectAll)
				for _, h := range []uintptr{app.images, app.documents, app.videos, app.audio, app.archives} {
					setChecked(h, checked)
				}
			}
		case ID_IMAGES, ID_DOCUMENTS, ID_VIDEOS, ID_AUDIO, ID_ARCHIVES:
			if notification == BN_CLICKED {
				updateSelectAllState()
			}
		case ID_AUTO_OPEN:
			if notification == BN_CLICKED {
				saveSettings()
			}
		case ID_PERFORMANCE, ID_MODE, ID_PROFILE, ID_DAMAGE:
			if notification == CBN_SELCHANGE {
				saveSettings()
				updateModeUI()
			}
		case ID_RESUME, ID_DEDUP, ID_ONLY_FREE, ID_CLONE_VERIFY, ID_CLONE_RESUME:
			if notification == BN_CLICKED {
				saveSettings()
			}
		case ID_CLONE_DAMAGE, ID_CLONE_PERFORMANCE, ID_CLONE_MODE:
			if notification == CBN_SELCHANGE {
				saveSettings()
				updateCloneModeUI()
				updateCloneDiskInfo()
			}
		case ID_PAUSE:
			if notification == BN_CLICKED {
				togglePause()
			}
		case ID_START:
			if notification == BN_CLICKED {
				if app.activeTab == 1 {
					startClone()
				} else {
					startRecovery()
				}
			}
		case ID_CANCEL:
			if notification == BN_CLICKED {
				cancelRecovery()
			}
		case ID_OPEN_FOLDER:
			if notification == BN_CLICKED {
				app.state.mu.Lock()
				dir := app.state.outputDir
				app.state.mu.Unlock()
				if dir != "" {
					cloneReport := filepath.Join(dir, "RELATORIO_CLONAGEM.html")
					recoveryReport := filepath.Join(dir, "RESULTADOS.html")
					if _, err := os.Stat(cloneReport); err == nil {
						shellOpen(cloneReport)
					} else if _, err := os.Stat(recoveryReport); err == nil {
						shellOpen(recoveryReport)
					} else {
						shellOpen(dir)
					}
				}
			}
		}
		return 0

	case WM_APP_UPDATE:
		atomic.StoreInt32(&uiUpdatePending, 0)
		updateUI()
		return 0

	case WM_CTLCOLOREDIT, WM_CTLCOLORLISTBOX:
		hdc := wParam
		procSetTextColor.Call(hdc, colorText)
		procSetBkColor.Call(hdc, colorInput)
		return app.inputBrush

	case WM_CTLCOLORSTATIC, WM_CTLCOLORBTN, WM_CTLCOLORDLG, WM_CTLCOLORMSGBOX:
		hdc := wParam
		control := lParam
		textColor := colorText
		if control == app.subtitleLabel || control == app.diskInfo || control == app.destInfo || control == app.stats || control == app.cloneSourceInfo || control == app.cloneTargetInfo || control == app.cloneReportInfo || control == app.cloneConfirmHint || control == app.cloneGuide {
			textColor = colorMuted
		}
		if control == app.warning {
			textColor = colorWarning
		}
		if app.sectionLabels != nil && app.sectionLabels[control] {
			textColor = colorAccent
		}
		procSetTextColor.Call(hdc, textColor)
		procSetBkColor.Call(hdc, colorBackground)
		procSetBkMode.Call(hdc, TRANSPARENT)
		return app.backgroundBrush

	case WM_CLOSE:
		app.state.mu.Lock()
		running := app.state.running
		app.state.mu.Unlock()
		if running {
			if messageBox(hwnd, "Há uma operação em andamento. Deseja cancelar e sair?", appTitle, MB_YESNO|MB_ICONWARNING) != IDYES {
				return 0
			}
			cancelRecoveryWithoutPrompt()
		}
		saveSettings()
		procDestroyWindow.Call(hwnd)
		return 0

	case WM_DESTROY:
		for _, obj := range []uintptr{app.font, app.smallFont, app.sectionFont, app.titleFont, app.statusFont, app.actionFont, app.logFont, app.backgroundBrush, app.surfaceBrush, app.inputBrush} {
			if obj != 0 {
				procDeleteObject.Call(obj)
			}
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func createControls(hwnd uintptr) {
	app.sectionLabels = make(map[uintptr]bool)
	if procGetDpiForWindow.Find() == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
			app.dpi = int32(dpi)
		}
	}
	recreateFonts()

	app.titleLabel = createLabel(hwnd, "HdRecover Professional", 0, 0, 1, 1, 0)
	app.subtitleLabel = createLabel(hwnd, "Recupere arquivos ou faça uma clonagem física completa em áreas separadas e seguras.", 0, 0, 1, 1, 0)
	app.tabRecovery = createControl("BUTTON", "RECUPERAÇÃO", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_TAB_RECOVERY)
	app.tabClone = createControl("BUTTON", "CLONAGEM DE DISCO", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_TAB_CLONE)
	app.activeTab = 0
	app.sourceSection = createSectionLabel(hwnd, "1. ORIGEM")
	app.strategySection = createSectionLabel(hwnd, "2. ESTRATÉGIA DE RECUPERAÇÃO")
	app.destinationSection = createSectionLabel(hwnd, "3. DESTINO")
	app.fileTypesSection = createSectionLabel(hwnd, "TIPOS DE ARQUIVO")
	app.safetySection = createSectionLabel(hwnd, "SEGURANÇA E FILTROS")

	app.diskCombo = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS|WS_VSCROLL, 0, 0, 0, 1, 1, hwnd, ID_DISK_COMBO)
	app.openImage = createControl("BUTTON", "Abrir IMG / DD", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_OPEN_IMAGE)
	app.refresh = createControl("BUTTON", "Atualizar discos", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_REFRESH)
	app.diskInfo = createLabel(hwnd, "Consultando discos físicos e estado SMART...", 0, 0, 1, 1, 0)

	app.modeLabel = createLabel(hwnd, "Modo", 0, 0, 1, 1, 0)
	app.mode = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_MODE)
	for _, label := range []string{"Verificação rápida NTFS", "Verificação profunda por assinatura", "Criar imagem do disco"} {
		procSendMessageW.Call(app.mode, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.mode, CB_SETCURSEL, 0, 0)

	app.partitionLabel = createLabel(hwnd, "Partição", 0, 0, 1, 1, 0)
	app.partition = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS|WS_VSCROLL, 0, 0, 0, 1, 1, hwnd, ID_PARTITION)
	procSendMessageW.Call(app.partition, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr("Disco inteiro — aguardando análise"))))
	procSendMessageW.Call(app.partition, CB_SETCURSEL, 0, 0)

	app.profileLabel = createLabel(hwnd, "Perfil", 0, 0, 1, 1, 0)
	app.profile = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_PROFILE)
	for _, label := range []string{"Recuperação completa", "Fotos de câmera", "Documentos de trabalho", "Vídeos", "Unidade formatada", "Disco com falhas"} {
		procSendMessageW.Call(app.profile, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.profile, CB_SETCURSEL, 0, 0)

	app.damageLabel = createLabel(hwnd, "Leitura", 0, 0, 1, 1, 0)
	app.damage = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_DAMAGE)
	for _, label := range []string{"Rápida", "Equilibrada", "Disco danificado"} {
		procSendMessageW.Call(app.damage, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.damage, CB_SETCURSEL, 1, 0)

	app.performanceLabel = createLabel(hwnd, "Desempenho", 0, 0, 1, 1, 0)
	app.performance = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_PERFORMANCE)
	for _, label := range []string{"Automático / Turbo", "Compatibilidade"} {
		procSendMessageW.Call(app.performance, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.performance, CB_SETCURSEL, 0, 0)

	app.destEdit = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_DEST_EDIT)
	app.browse = createControl("BUTTON", "Escolher pasta...", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_BROWSE)
	app.destInfo = createLabel(hwnd, "O destino será validado antes de começar.", 0, 0, 1, 1, 0)

	app.selectAll = createControl("BUTTON", "Selecionar todos", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_SELECT_ALL)
	app.images = createControl("BUTTON", "Fotos e imagens", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_IMAGES)
	app.documents = createControl("BUTTON", "Documentos e bancos", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_DOCUMENTS)
	app.videos = createControl("BUTTON", "Vídeos", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_VIDEOS)
	app.audio = createControl("BUTTON", "Áudios", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_AUDIO)
	app.archives = createControl("BUTTON", "Compactados", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_ARCHIVES)
	for _, h := range []uintptr{app.images, app.documents, app.videos, app.audio, app.archives, app.selectAll} {
		setChecked(h, true)
	}

	app.resume = createControl("BUTTON", "Retomar sessão interrompida automaticamente", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_RESUME)
	app.dedup = createControl("BUTTON", "Ignorar duplicados exatos (SHA-256)", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_DEDUP)
	app.autoOpen = createControl("BUTTON", "Abrir resultados ao concluir", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_AUTO_OPEN)
	app.onlyFree = createControl("BUTTON", "Somente espaço livre NTFS", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_ONLY_FREE)
	setChecked(app.resume, true)
	setChecked(app.dedup, true)

	app.minSizeLabel = createLabel(hwnd, "Tamanho mínimo (KB)", 0, 0, 1, 1, 0)
	app.minSize = createControl("EDIT", "4", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_MIN_SIZE)
	app.rangeLabel = createLabel(hwnd, "Intervalo em GB: de", 0, 0, 1, 1, 0)
	app.rangeStart = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_RANGE_START)
	app.rangeToLabel = createLabel(hwnd, "até", 0, 0, 1, 1, 0)
	app.rangeEnd = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_RANGE_END)
	app.rangeHintLabel = createLabel(hwnd, "vazio = tudo", 0, 0, 1, 1, 0)

	// Aba dedicada de clonagem física. Os controles são ocultados inicialmente.
	app.cloneSourceSection = createSectionLabel(hwnd, "1. DISCO DE ORIGEM")
	app.cloneTargetSection = createSectionLabel(hwnd, "2. DISCO DE DESTINO — SERÁ TOTALMENTE APAGADO")
	app.cloneOptionsSection = createSectionLabel(hwnd, "3. OPÇÕES DE CLONAGEM")
	app.cloneReportSection = createSectionLabel(hwnd, "RELATÓRIO E RETOMADA")
	app.cloneSafetySection = createSectionLabel(hwnd, "CONFIRMAÇÃO DE SEGURANÇA")
	app.cloneSource = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS|WS_VSCROLL, 0, 0, 0, 1, 1, hwnd, ID_CLONE_SOURCE)
	app.cloneSourceInfo = createLabel(hwnd, "Selecione o disco que contém os dados originais.", 0, 0, 1, 1, 0)
	app.cloneTarget = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS|WS_VSCROLL, 0, 0, 0, 1, 1, hwnd, ID_CLONE_TARGET)
	app.cloneTargetInfo = createLabel(hwnd, "O destino precisa ter capacidade igual ou maior que a origem.", 0, 0, 1, 1, 0)
	app.cloneRefresh = createControl("BUTTON", "Atualizar discos", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_CLONE_REFRESH)
	app.cloneModeLabel = createLabel(hwnd, "Modo", 0, 0, 1, 1, 0)
	app.cloneMode = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_CLONE_MODE)
	for _, label := range []string{"Clone setor a setor exato (disco secundário)", "Migrar Windows em uso com VSS — destino igual/maior", "Migrar Windows para SSD menor — inteligente"} {
		procSendMessageW.Call(app.cloneMode, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.cloneMode, CB_SETCURSEL, 0, 0)
	app.cloneDamageLabel = createLabel(hwnd, "Leitura da origem", 0, 0, 1, 1, 0)
	app.cloneDamage = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_CLONE_DAMAGE)
	for _, label := range []string{"Rápida", "Equilibrada", "Disco danificado"} {
		procSendMessageW.Call(app.cloneDamage, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.cloneDamage, CB_SETCURSEL, 1, 0)
	app.clonePerformanceLabel = createLabel(hwnd, "Desempenho", 0, 0, 1, 1, 0)
	app.clonePerformance = createControl("COMBOBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|CBS_DROPDOWNLIST|CBS_HASSTRINGS, 0, 0, 0, 1, 1, hwnd, ID_CLONE_PERFORMANCE)
	for _, label := range []string{"Automático / Turbo", "Compatibilidade"} {
		procSendMessageW.Call(app.clonePerformance, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	procSendMessageW.Call(app.clonePerformance, CB_SETCURSEL, 0, 0)
	app.cloneVerify = createControl("BUTTON", "Verificar todos os blocos após copiar (recomendado)", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_CLONE_VERIFY)
	app.cloneResume = createControl("BUTTON", "Retomar clonagem interrompida usando o relatório", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_AUTOCHECKBOX, 0, 0, 0, 1, 1, hwnd, ID_CLONE_RESUME)
	setChecked(app.cloneVerify, true)
	setChecked(app.cloneResume, true)
	app.cloneReportEdit = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_CLONE_REPORT_EDIT)
	app.cloneReportBrowse = createControl("BUTTON", "Escolher pasta...", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_CLONE_REPORT_BROWSE)
	app.cloneReportInfo = createLabel(hwnd, "A sessão, o mapa de setores ruins e a verificação serão salvos aqui.", 0, 0, 1, 1, 0)
	app.cloneConfirmLabel = createLabel(hwnd, "Digite CLONAR para liberar a gravação no destino", 0, 0, 1, 1, 0)
	app.cloneConfirm = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, ID_CLONE_CONFIRM)
	app.cloneConfirmHint = createLabel(hwnd, "O disco escolhido como destino terá MBR/GPT, partições, arquivos e espaço livre substituídos pela origem.", 0, 0, 1, 1, 0)
	app.cloneGuide = createLabel(hwnd, "No modo exato, use discos secundários. Para trocar o HD do Windows por um SSD sem desligar o sistema, selecione “Migrar Windows em uso com VSS”.", 0, 0, 1, 1, 0)

	app.warning = createLabel(hwnd, "A origem é aberta somente para leitura. Em SSD com TRIM, arquivos apagados podem já ter sido eliminados fisicamente.", 0, 0, 1, 1, 0)

	app.start = createControl("BUTTON", "Iniciar verificação rápida", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_DEFPUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_START)
	app.pauseBtn = createControl("BUTTON", "Pausar", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_PAUSE)
	app.cancelBtn = createControl("BUTTON", "Cancelar", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_CANCEL)
	app.openFolder = createControl("BUTTON", "Abrir resultados", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, 0, 0, 0, 1, 1, hwnd, ID_OPEN_FOLDER)
	enable(app.pauseBtn, false)
	enable(app.cancelBtn, false)
	enable(app.openFolder, false)
	enable(app.diskCombo, false)
	enable(app.refresh, false)
	enable(app.start, false)

	app.progress = createControl("msctls_progress32", "", WS_CHILD|WS_VISIBLE, 0, 0, 0, 1, 1, hwnd, 0)
	procSendMessageW.Call(app.progress, PBM_SETRANGE32, 0, 100)
	procSendMessageW.Call(app.progress, PBM_SETBARCOLOR, 0, colorAccent)
	procSendMessageW.Call(app.progress, PBM_SETBKCOLOR, 0, colorSurface)
	app.status = createLabel(hwnd, "Inicializando...", 0, 0, 1, 1, 0)
	app.stats = createLabel(hwnd, "", 0, 0, 1, 1, 0)
	app.logEdit = createControl("EDIT", "", WS_CHILD|WS_VISIBLE|WS_VSCROLL|ES_MULTILINE|ES_AUTOVSCROLL|ES_READONLY, WS_EX_CLIENTEDGE, 0, 0, 1, 1, hwnd, 0)

	applyFonts()
	applyDarkThemeToControls()
	layoutControls(hwnd)
	loadSettingsIntoUI()
	switchTab(0)
	updateModeUI()
	updateCloneModeUI()

	app.state.mu.Lock()
	app.state.status = "Procurando discos físicos em segundo plano..."
	app.state.disksLoading = false
	app.state.mu.Unlock()
	requestUIUpdate()
}

func createLabel(parent uintptr, text string, x, y, w, h int32, id int) uintptr {
	return createControl("STATIC", text, WS_CHILD|WS_VISIBLE, 0, x, y, w, h, parent, id)
}

func createSectionLabel(parent uintptr, text string) uintptr {
	h := createLabel(parent, text, 0, 0, 1, 1, 0)
	if h != 0 {
		app.sectionLabels[h] = true
	}
	return h
}

func createControl(className, text string, style, exStyle uintptr, x, y, w, h int32, parent uintptr, id int) uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		exStyle,
		uintptr(unsafe.Pointer(utf16Ptr(className))),
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), instance, 0,
	)
	if hwnd != 0 {
		app.allControls = append(app.allControls, hwnd)
		if app.font != 0 {
			procSendMessageW.Call(hwnd, WM_SETFONT, app.font, 1)
		}
	}
	return hwnd
}

func showControl(hwnd uintptr, visible bool) {
	if hwnd == 0 {
		return
	}
	cmd := uintptr(SW_HIDE)
	if visible {
		cmd = SW_SHOW
	}
	procShowWindow.Call(hwnd, cmd)
}

func recoveryTabControls() []uintptr {
	return []uintptr{
		app.sourceSection, app.strategySection, app.destinationSection, app.fileTypesSection, app.safetySection,
		app.diskCombo, app.refresh, app.openImage, app.diskInfo,
		app.modeLabel, app.mode, app.partitionLabel, app.partition, app.profileLabel, app.profile,
		app.damageLabel, app.damage, app.performanceLabel, app.performance,
		app.destEdit, app.browse, app.destInfo, app.images, app.documents, app.videos, app.audio, app.archives,
		app.selectAll, app.autoOpen, app.resume, app.dedup, app.onlyFree, app.minSizeLabel, app.minSize,
		app.rangeLabel, app.rangeStart, app.rangeToLabel, app.rangeEnd, app.rangeHintLabel,
	}
}

func cloneTabControls() []uintptr {
	return []uintptr{
		app.cloneSourceSection, app.cloneTargetSection, app.cloneOptionsSection, app.cloneReportSection, app.cloneSafetySection,
		app.cloneSource, app.cloneSourceInfo, app.cloneTarget, app.cloneTargetInfo, app.cloneRefresh,
		app.cloneModeLabel, app.cloneMode, app.cloneDamageLabel, app.cloneDamage,
		app.clonePerformanceLabel, app.clonePerformance, app.cloneVerify, app.cloneResume,
		app.cloneReportEdit, app.cloneReportBrowse, app.cloneReportInfo,
		app.cloneConfirmLabel, app.cloneConfirm, app.cloneConfirmHint, app.cloneGuide,
	}
}

func switchTab(tab int) {
	app.state.mu.Lock()
	running := app.state.running
	app.state.mu.Unlock()
	if running || (tab != 0 && tab != 1) {
		return
	}
	app.activeTab = tab
	for _, h := range recoveryTabControls() {
		showControl(h, tab == 0)
	}
	for _, h := range cloneTabControls() {
		showControl(h, tab == 1)
	}
	if tab == 1 {
		setText(app.tabRecovery, "RECUPERAÇÃO")
		setText(app.tabClone, "●  CLONAGEM DE DISCO")
		setText(app.subtitleLabel, "Clone discos secundários ou migre o Windows em uso para um SSD com snapshots VSS.")
		setText(app.openFolder, "Abrir relatório")
		updateCloneModeUI()
		updateCloneDiskInfo()
	} else {
		setText(app.tabRecovery, "●  RECUPERAÇÃO")
		setText(app.tabClone, "CLONAGEM DE DISCO")
		setText(app.subtitleLabel, "Recuperação NTFS, varredura profunda, imagens de disco e leitura segura.")
		setText(app.openFolder, "Abrir resultados")
		updateModeUI()
		updateSelectedDiskInfo()
	}
	layoutControls(app.hwnd)
	app.state.mu.Lock()
	running = app.state.running
	loading := app.state.disksLoading
	hasDisks := len(app.state.disks) > 0
	app.state.mu.Unlock()
	setBusyControls(running, loading, hasDisks)
}

func enableProcessDPIAwareness() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4.
	if procSetProcessDpiContext.Find() == nil {
		if r, _, _ := procSetProcessDpiContext.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	if procSetProcessDpiAware.Find() == nil {
		procSetProcessDpiAware.Call()
	}
}

func systemDPI() int32 {
	if procGetDpiForSystem.Find() == nil {
		if dpi, _, _ := procGetDpiForSystem.Call(); dpi >= 96 {
			return int32(dpi)
		}
	}
	return 96
}

func currentDPI() int32 {
	if app.dpi >= 96 {
		return app.dpi
	}
	return 96
}

func logicalToPixels(value, dpi int32) int32 {
	if dpi <= 0 {
		dpi = 96
	}
	return (value*dpi + 48) / 96
}

func pixelsToLogical(value, dpi int32) int32 {
	if dpi <= 0 {
		dpi = 96
	}
	return (value*96 + dpi/2) / dpi
}

func fontHeight(points, dpi int32) uintptr {
	h := -((points*dpi + 36) / 72)
	return uintptr(int64(h))
}

func makeFont(points int32, weight uintptr, face string) uintptr {
	f, _, _ := procCreateFontW.Call(
		fontHeight(points, currentDPI()), 0, 0, 0, weight, 0, 0, 0,
		1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(utf16Ptr(face))),
	)
	return f
}

func recreateFonts() {
	for _, f := range []uintptr{app.font, app.smallFont, app.sectionFont, app.titleFont, app.statusFont, app.actionFont, app.logFont} {
		if f != 0 {
			procDeleteObject.Call(f)
		}
	}
	app.font = makeFont(11, 400, "Segoe UI")
	app.smallFont = makeFont(10, 400, "Segoe UI")
	app.sectionFont = makeFont(12, 700, "Segoe UI")
	app.titleFont = makeFont(22, 700, "Segoe UI")
	app.statusFont = makeFont(12, 600, "Segoe UI")
	app.actionFont = makeFont(11, 600, "Segoe UI")
	app.logFont = makeFont(10, 400, "Consolas")
	applyFonts()
}

func applyFonts() {
	for _, h := range app.allControls {
		if h != 0 && app.font != 0 {
			procSendMessageW.Call(h, WM_SETFONT, app.font, 1)
		}
	}
	for _, h := range []uintptr{app.subtitleLabel, app.diskInfo, app.destInfo, app.stats, app.rangeHintLabel, app.cloneSourceInfo, app.cloneTargetInfo, app.cloneReportInfo, app.cloneConfirmHint, app.cloneGuide} {
		if h != 0 && app.smallFont != 0 {
			procSendMessageW.Call(h, WM_SETFONT, app.smallFont, 1)
		}
	}
	for h := range app.sectionLabels {
		if h != 0 && app.sectionFont != 0 {
			procSendMessageW.Call(h, WM_SETFONT, app.sectionFont, 1)
		}
	}
	if app.titleLabel != 0 && app.titleFont != 0 {
		procSendMessageW.Call(app.titleLabel, WM_SETFONT, app.titleFont, 1)
	}
	if app.status != 0 && app.statusFont != 0 {
		procSendMessageW.Call(app.status, WM_SETFONT, app.statusFont, 1)
	}
	for _, h := range []uintptr{app.start, app.pauseBtn, app.cancelBtn, app.openFolder, app.refresh, app.openImage, app.browse, app.tabRecovery, app.tabClone, app.cloneRefresh, app.cloneReportBrowse} {
		if h != 0 && app.actionFont != 0 {
			procSendMessageW.Call(h, WM_SETFONT, app.actionFont, 1)
		}
	}
	if app.logEdit != 0 && app.logFont != 0 {
		procSendMessageW.Call(app.logEdit, WM_SETFONT, app.logFont, 1)
	}
}

func moveControl(hwnd uintptr, x, y, w, h, dpi int32) {
	if hwnd == 0 {
		return
	}
	procMoveWindow.Call(hwnd,
		uintptr(logicalToPixels(x, dpi)), uintptr(logicalToPixels(y, dpi)),
		uintptr(logicalToPixels(w, dpi)), uintptr(logicalToPixels(h, dpi)), 1)
}

func moveCombo(hwnd uintptr, x, y, w, dropdownHeight, dpi int32) {
	if dropdownHeight < 160 {
		dropdownHeight = 160
	}
	moveControl(hwnd, x, y, w, dropdownHeight, dpi)
}

func layoutControls(hwnd uintptr) {
	if hwnd == 0 || app.titleLabel == 0 {
		return
	}
	var client rect
	if r, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&client))); r == 0 {
		return
	}
	dpi := currentDPI()
	cw := pixelsToLogical(client.Right-client.Left, dpi)
	ch := pixelsToLogical(client.Bottom-client.Top, dpi)
	if cw < 900 {
		cw = 900
	}
	if ch < 620 {
		ch = 620
	}

	margin := int32(24)
	gap := int32(24)
	rightW := int32(410)
	if cw < 1160 {
		rightW = 350
	}
	leftW := cw - margin*2 - gap - rightW
	if leftW < 560 {
		leftW = 560
		rightW = max32(280, cw-margin*2-gap-leftW)
	}
	leftX := margin
	rightX := leftX + leftW + gap
	fullW := cw - margin*2

	moveControl(app.titleLabel, margin, 10, fullW, 34, dpi)
	moveControl(app.subtitleLabel, margin, 46, fullW, 24, dpi)
	moveControl(app.tabRecovery, margin, 78, 190, 40, dpi)
	moveControl(app.tabClone, margin+202, 78, 230, 40, dpi)

	if app.activeTab == 0 {
		// RECUPERAÇÃO
		moveControl(app.sourceSection, leftX, 128, leftW, 22, dpi)
		buttonW := int32(150)
		buttonGap := int32(10)
		diskW := leftW - buttonW*2 - buttonGap*2
		if diskW < 250 {
			diskW = 250
		}
		moveCombo(app.diskCombo, leftX, 154, diskW, 300, dpi)
		moveControl(app.openImage, leftX+diskW+buttonGap, 154, buttonW, 36, dpi)
		moveControl(app.refresh, leftX+diskW+buttonGap+buttonW+buttonGap, 154, buttonW, 36, dpi)
		moveControl(app.diskInfo, leftX, 193, leftW, 22, dpi)

		moveControl(app.strategySection, leftX, 220, leftW, 22, dpi)
		labelW := int32(72)
		comboGap := int32(16)
		rowComboW := (leftW - labelW*2 - comboGap) / 2
		moveControl(app.modeLabel, leftX, 252, labelW, 22, dpi)
		moveCombo(app.mode, leftX+labelW, 246, rowComboW, 230, dpi)
		partX := leftX + labelW + rowComboW + comboGap
		moveControl(app.partitionLabel, partX, 252, labelW, 22, dpi)
		moveCombo(app.partition, partX+labelW, 246, leftX+leftW-(partX+labelW), 300, dpi)

		fieldGap := int32(14)
		availableFields := leftW - fieldGap*2
		profileW := availableFields * 38 / 100
		damageW := availableFields * 27 / 100
		perfW := availableFields - profileW - damageW
		x := leftX
		moveControl(app.profileLabel, x, 287, profileW, 20, dpi)
		moveCombo(app.profile, x, 307, profileW, 230, dpi)
		x += profileW + fieldGap
		moveControl(app.damageLabel, x, 287, damageW, 20, dpi)
		moveCombo(app.damage, x, 307, damageW, 210, dpi)
		x += damageW + fieldGap
		moveControl(app.performanceLabel, x, 287, perfW, 20, dpi)
		moveCombo(app.performance, x, 307, perfW, 190, dpi)

		moveControl(app.destinationSection, leftX, 350, leftW, 22, dpi)
		browseW := int32(170)
		moveControl(app.destEdit, leftX, 376, leftW-browseW-10, 36, dpi)
		moveControl(app.browse, leftX+leftW-browseW, 376, browseW, 36, dpi)
		moveControl(app.destInfo, leftX, 416, leftW, 22, dpi)

		moveControl(app.fileTypesSection, rightX, 128, rightW-155, 22, dpi)
		moveControl(app.selectAll, rightX+rightW-155, 126, 155, 26, dpi)
		colW := (rightW - 12) / 2
		moveControl(app.images, rightX, 158, colW, 26, dpi)
		moveControl(app.documents, rightX+colW+12, 158, colW, 26, dpi)
		moveControl(app.videos, rightX, 190, colW, 26, dpi)
		moveControl(app.audio, rightX+colW+12, 190, colW, 26, dpi)
		moveControl(app.archives, rightX, 222, colW, 26, dpi)

		moveControl(app.safetySection, rightX, 258, rightW, 22, dpi)
		moveControl(app.resume, rightX, 286, rightW, 25, dpi)
		moveControl(app.dedup, rightX, 316, rightW, 25, dpi)
		moveControl(app.autoOpen, rightX, 346, rightW, 25, dpi)
		moveControl(app.onlyFree, rightX, 376, rightW, 25, dpi)
		moveControl(app.minSizeLabel, rightX, 410, 178, 22, dpi)
		moveControl(app.minSize, rightX+188, 405, 86, 32, dpi)
		moveControl(app.rangeLabel, rightX, 440, 158, 22, dpi)
		moveControl(app.rangeStart, rightX+160, 435, 62, 30, dpi)
		moveControl(app.rangeToLabel, rightX+227, 440, 28, 22, dpi)
		moveControl(app.rangeEnd, rightX+258, 435, 62, 30, dpi)
		moveControl(app.rangeHintLabel, rightX+326, 440, max32(0, rightW-326), 22, dpi)
	} else {
		// CLONAGEM DE DISCO
		moveControl(app.cloneSourceSection, leftX, 128, leftW, 22, dpi)
		moveCombo(app.cloneSource, leftX, 156, leftW-170, 320, dpi)
		moveControl(app.cloneRefresh, leftX+leftW-160, 156, 160, 36, dpi)
		moveControl(app.cloneSourceInfo, leftX, 196, leftW, 22, dpi)

		moveControl(app.cloneTargetSection, leftX, 228, leftW, 22, dpi)
		moveCombo(app.cloneTarget, leftX, 256, leftW, 320, dpi)
		moveControl(app.cloneTargetInfo, leftX, 296, leftW, 22, dpi)

		moveControl(app.cloneOptionsSection, leftX, 328, leftW, 22, dpi)
		fieldGap := int32(14)
		modeW := (leftW - fieldGap*2) * 40 / 100
		damageW := (leftW - fieldGap*2) * 30 / 100
		perfW := leftW - fieldGap*2 - modeW - damageW
		x := leftX
		moveControl(app.cloneModeLabel, x, 356, modeW, 20, dpi)
		moveCombo(app.cloneMode, x, 376, modeW, 180, dpi)
		x += modeW + fieldGap
		moveControl(app.cloneDamageLabel, x, 356, damageW, 20, dpi)
		moveCombo(app.cloneDamage, x, 376, damageW, 210, dpi)
		x += damageW + fieldGap
		moveControl(app.clonePerformanceLabel, x, 356, perfW, 20, dpi)
		moveCombo(app.clonePerformance, x, 376, perfW, 190, dpi)
		moveControl(app.cloneVerify, leftX, 416, leftW, 25, dpi)
		moveControl(app.cloneResume, leftX, 445, leftW, 25, dpi)

		moveControl(app.cloneReportSection, rightX, 128, rightW, 22, dpi)
		moveControl(app.cloneReportEdit, rightX, 156, rightW-150, 36, dpi)
		moveControl(app.cloneReportBrowse, rightX+rightW-140, 156, 140, 36, dpi)
		moveControl(app.cloneReportInfo, rightX, 196, rightW, 42, dpi)

		moveControl(app.cloneSafetySection, rightX, 250, rightW, 22, dpi)
		moveControl(app.cloneConfirmLabel, rightX, 280, rightW, 22, dpi)
		moveControl(app.cloneConfirm, rightX, 306, rightW, 38, dpi)
		moveControl(app.cloneConfirmHint, rightX, 352, rightW, 48, dpi)
		moveControl(app.cloneGuide, rightX, 407, rightW, 63, dpi)
	}

	warningY := ch - 190
	if warningY > 500 {
		warningY = 500
	}
	if warningY < 470 {
		warningY = 470
	}
	moveControl(app.warning, margin, warningY, fullW, 38, dpi)

	actionY := warningY + 44
	moveControl(app.start, margin, actionY, 260, 42, dpi)
	moveControl(app.pauseBtn, margin+272, actionY, 130, 42, dpi)
	moveControl(app.cancelBtn, margin+414, actionY, 130, 42, dpi)
	moveControl(app.openFolder, margin+556, actionY, 220, 42, dpi)

	progressY := actionY + 50
	moveControl(app.progress, margin, progressY, fullW, 18, dpi)
	moveControl(app.status, margin, progressY+23, fullW, 23, dpi)
	moveControl(app.stats, margin, progressY+47, fullW, 22, dpi)
	logY := progressY + 72
	logH := ch - logY - margin
	if logH < 40 {
		logH = 40
	}
	moveControl(app.logEdit, margin, logY, fullW, logH, dpi)
	procInvalidateRect.Call(hwnd, 0, 1)
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func applyDarkThemeToControls() {
	dark := utf16Ptr("DarkMode_Explorer")
	themeAvailable := procSetWindowTheme.Find() == nil
	for _, h := range []uintptr{
		app.hwnd, app.tabRecovery, app.tabClone, app.diskCombo, app.refresh, app.openImage, app.destEdit, app.browse,
		app.images, app.documents, app.videos, app.audio, app.archives,
		app.selectAll, app.autoOpen, app.performance, app.mode, app.partition, app.profile, app.damage,
		app.resume, app.dedup, app.onlyFree, app.minSize, app.rangeStart, app.rangeEnd,
		app.cloneSource, app.cloneTarget, app.cloneRefresh, app.cloneMode, app.cloneDamage, app.clonePerformance,
		app.cloneVerify, app.cloneResume, app.cloneReportEdit, app.cloneReportBrowse, app.cloneConfirm,
		app.start, app.pauseBtn, app.cancelBtn, app.openFolder, app.progress, app.logEdit,
	} {
		if h != 0 && themeAvailable {
			procSetWindowTheme.Call(h, uintptr(unsafe.Pointer(dark)), 0)
		}
	}
	enableDarkTitleBar(app.hwnd)
}

func enableDarkTitleBar(hwnd uintptr) {
	if hwnd == 0 || procDwmSetWindowAttr.Find() != nil {
		return
	}
	enabled := int32(1)
	// Windows 11/versões recentes usam 20; versões anteriores podem usar 19.
	if r, _, _ := procDwmSetWindowAttr.Call(hwnd, 20, uintptr(unsafe.Pointer(&enabled)), 4); int32(r) != 0 {
		procDwmSetWindowAttr.Call(hwnd, 19, uintptr(unsafe.Pointer(&enabled)), 4)
	}
}

func refreshDisks() {
	app.state.mu.Lock()
	if app.state.running || app.state.disksLoading {
		app.state.mu.Unlock()
		return
	}
	app.state.disksLoading = true
	app.state.status = "Procurando discos físicos em segundo plano..."
	addLogLocked(&app.state, "Atualizando lista de discos pelo Windows, sem PowerShell.")
	app.state.mu.Unlock()
	requestUIUpdate()

	app.state.mu.Lock()
	var custom []diskInfo
	for _, d := range app.state.disks {
		if d.CustomPath != "" {
			custom = append(custom, d)
		}
	}
	app.state.mu.Unlock()
	disks, err := listDisksNative()
	if err == nil {
		disks = append(disks, custom...)
	} else if len(custom) > 0 {
		disks = custom
		err = nil
	}

	app.state.mu.Lock()
	app.state.disksLoading = false
	if err != nil {
		app.state.status = "Não foi possível listar os discos."
		addLogLocked(&app.state, "Erro ao listar discos: "+err.Error())
	} else {
		app.state.disks = disks
		app.state.diskVersion++
		app.state.status = fmt.Sprintf("%d disco(s) físico(s) encontrado(s).", len(disks))
		addLogLocked(&app.state, fmt.Sprintf("%d disco(s) físico(s) encontrado(s).", len(disks)))
	}
	app.state.mu.Unlock()
	requestUIUpdate()
}

func refreshPartitions() {
	app.state.mu.Lock()
	if app.state.running || app.state.partitionsLoading {
		app.state.mu.Unlock()
		return
	}
	selected := app.state.selectedDisk
	disks := append([]diskInfo(nil), app.state.disks...)
	if selected < 0 || selected >= len(disks) {
		app.state.mu.Unlock()
		return
	}
	disk := disks[selected]
	app.state.partitionsLoading = true
	addLogLocked(&app.state, fmt.Sprintf("Lendo tabela de partições do Disco %d...", disk.Index))
	app.state.mu.Unlock()
	requestUIUpdate()

	parts, err := recovery.DetectPartitionsFromPath(disk.Path(), disk.Size, disk.BytesPerSector)
	whole := recovery.Partition{Index: 0, Scheme: "RAW", Type: "Disco inteiro", Name: "Disco inteiro", Start: 0, Size: disk.Size}
	if err == nil {
		parts = append([]recovery.Partition{whole}, parts...)
	} else {
		parts = []recovery.Partition{whole}
	}
	app.state.mu.Lock()
	app.state.partitionsLoading = false
	app.state.partitions = parts
	app.state.partitionVersion++
	if err != nil {
		addLogLocked(&app.state, "Não foi possível interpretar as partições: "+err.Error()+". O disco inteiro continua disponível.")
	} else {
		addLogLocked(&app.state, fmt.Sprintf("%d opção(ões) de partição disponível(is).", len(parts)))
	}
	app.state.mu.Unlock()
	requestUIUpdate()
}

func selectedMode() recovery.OperationMode {
	selection, _, _ := procSendMessageW.Call(app.mode, CB_GETCURSEL, 0, 0)
	switch int(selection) {
	case 0:
		return recovery.ModeQuickNTFS
	case 2:
		return recovery.ModeCreateImage
	default:
		return recovery.ModeDeepCarving
	}
}

func selectedProfile() recovery.RecoveryProfile {
	selection, _, _ := procSendMessageW.Call(app.profile, CB_GETCURSEL, 0, 0)
	if int(selection) < 0 || int(selection) > int(recovery.ProfileDamagedDrive) {
		return recovery.ProfileComplete
	}
	return recovery.RecoveryProfile(selection)
}

func selectedDamageMode() recovery.DamageMode {
	selection, _, _ := procSendMessageW.Call(app.damage, CB_GETCURSEL, 0, 0)
	if int(selection) < 0 || int(selection) > int(recovery.DamageCareful) {
		return recovery.DamageBalanced
	}
	return recovery.DamageMode(selection)
}

func selectedPartition() recovery.Partition {
	app.state.mu.Lock()
	parts := append([]recovery.Partition(nil), app.state.partitions...)
	app.state.mu.Unlock()
	selection, _, _ := procSendMessageW.Call(app.partition, CB_GETCURSEL, 0, 0)
	if int(selection) >= 0 && int(selection) < len(parts) {
		return parts[int(selection)]
	}
	return recovery.Partition{}
}

func applyCustomRange(part recovery.Partition) (recovery.Partition, error) {
	startText := strings.TrimSpace(strings.ReplaceAll(getText(app.rangeStart), ",", "."))
	endText := strings.TrimSpace(strings.ReplaceAll(getText(app.rangeEnd), ",", "."))
	if startText == "" && endText == "" {
		return part, nil
	}
	startGB := float64(0)
	endGB := float64(part.Size) / float64(int64(1)<<30)
	var err error
	if startText != "" {
		startGB, err = strconv.ParseFloat(startText, 64)
		if err != nil || startGB < 0 {
			return part, errors.New("início do intervalo inválido")
		}
	}
	if endText != "" {
		endGB, err = strconv.ParseFloat(endText, 64)
		if err != nil || endGB <= startGB {
			return part, errors.New("fim do intervalo inválido")
		}
	}
	startBytes := int64(startGB * float64(int64(1)<<30))
	endBytes := int64(endGB * float64(int64(1)<<30))
	if startBytes >= part.Size || endBytes > part.Size || endBytes <= startBytes {
		return part, errors.New("o intervalo está fora da partição selecionada")
	}
	part.Start += startBytes
	part.Size = endBytes - startBytes
	part.Name = "Intervalo personalizado"
	part.Type = "Intervalo personalizado"
	return part, nil
}

func updateModeUI() {
	app.state.mu.Lock()
	running := app.state.running
	app.state.mu.Unlock()
	if running {
		return
	}
	mode := selectedMode()
	imageMode := mode == recovery.ModeCreateImage
	quickMode := mode == recovery.ModeQuickNTFS
	for _, h := range []uintptr{app.images, app.documents, app.videos, app.audio, app.archives, app.selectAll, app.dedup, app.minSize} {
		enable(h, !imageMode)
	}
	enable(app.onlyFree, mode == recovery.ModeDeepCarving)
	if imageMode {
		setText(app.start, "Criar imagem de segurança")
		setText(app.warning, "A imagem será criada em outro disco e poderá ser analisada depois sem continuar forçando a origem.")
	} else if quickMode {
		setText(app.start, "Iniciar verificação rápida")
	} else {
		setText(app.start, "Iniciar verificação profunda")
	}
}

func togglePause() {
	app.state.mu.Lock()
	if !app.state.running {
		app.state.mu.Unlock()
		return
	}
	app.state.paused = !app.state.paused
	paused := app.state.paused
	if paused {
		app.state.status = "Pausado — o disco não está sendo lido"
		addLogLocked(&app.state, "Operação pausada pelo usuário.")
	} else {
		app.state.status = "Continuando a operação..."
		addLogLocked(&app.state, "Operação retomada.")
	}
	app.state.mu.Unlock()
	requestUIUpdate()
}

func isPaused() bool {
	app.state.mu.Lock()
	defer app.state.mu.Unlock()
	return app.state.paused
}

func selectedMinSizeBytes() int64 {
	value := strings.TrimSpace(getText(app.minSize))
	kb, err := strconv.ParseInt(value, 10, 64)
	if err != nil || kb < 0 {
		return 0
	}
	if kb > 10*1024*1024 {
		kb = 10 * 1024 * 1024
	}
	return kb * 1024
}

func selectedCloneDisk(combo uintptr, mapping []int) (diskInfo, bool) {
	selection, _, _ := procSendMessageW.Call(combo, CB_GETCURSEL, 0, 0)
	idx := int(selection)
	if idx < 0 || idx >= len(mapping) {
		return diskInfo{}, false
	}
	app.state.mu.Lock()
	defer app.state.mu.Unlock()
	diskIndex := mapping[idx]
	if diskIndex < 0 || diskIndex >= len(app.state.disks) {
		return diskInfo{}, false
	}
	return app.state.disks[diskIndex], true
}

func selectedCloneMode() int {
	selection, _, _ := procSendMessageW.Call(app.cloneMode, CB_GETCURSEL, 0, 0)
	value := int(selection)
	if value < 0 || value > 2 {
		return 0
	}
	return value
}

func updateCloneModeUI() {
	if app.cloneMode == 0 {
		return
	}
	mode := selectedCloneMode()
	switch mode {
	case 1:
		setText(app.cloneGuide, "Usa VSS para congelar C: e os demais volumes simples da origem, enquanto o Windows continua funcionando. Este modo exige destino com capacidade física igual ou maior.")
		setText(app.cloneConfirmHint, "O SSD será sobrescrito por uma cópia física completa do HD. Os volumes do Windows serão lidos de snapshots VSS consistentes; partições ocultas também serão copiadas.")
		setText(app.cloneConfirmLabel, "Digite MIGRAR para liberar a migração do Windows")
		setText(app.start, "Migrar Windows para SSD")
		setText(app.warning, "MIGRAÇÃO VSS: feche jogos, atualizações, máquinas virtuais e programas que gravem muitos dados. O destino será totalmente apagado.")
		setChecked(app.cloneResume, false)
		enable(app.cloneResume, false)
	case 2:
		setText(app.cloneGuide, "Analisa as partições e o espaço realmente usado. Quando necessário, reduz temporariamente a última partição NTFS, clona somente até o limite do SSD e restaura o HD ao tamanho original.")
		setText(app.cloneConfirmHint, "Indicado para migrar um HD de 500 GB para um SSD de 480 GB. A análise bloqueia o serviço quando os dados ou arquivos imóveis não cabem com segurança.")
		setText(app.cloneConfirmLabel, "Digite MIGRAR para liberar a migração inteligente")
		setText(app.start, "Analisar e migrar para SSD menor")
		setText(app.warning, "MIGRAÇÃO INTELIGENTE: o destino será apagado. A origem pode ter uma partição NTFS reduzida temporariamente e restaurada ao final. Não desligue o computador.")
		setChecked(app.cloneResume, false)
		enable(app.cloneResume, false)
	default:
		setText(app.cloneGuide, "Para maior consistência, conecte a origem e o destino como discos secundários. O modo exato não copia com segurança o Windows que está atualmente em uso.")
		setText(app.cloneConfirmHint, "O disco escolhido como destino terá MBR/GPT, partições, arquivos e espaço livre substituídos pela origem.")
		setText(app.cloneConfirmLabel, "Digite CLONAR para liberar a gravação no destino")
		setText(app.start, "Iniciar clonagem")
		setText(app.warning, "PERIGO: o destino inteiro será sobrescrito. Confira modelo, capacidade e letras antes de iniciar.")
		enable(app.cloneResume, true)
	}
}

func updateCloneDiskInfo() {
	source, sourceOK := selectedCloneDisk(app.cloneSource, app.cloneSourceMap)
	target, targetOK := selectedCloneDisk(app.cloneTarget, app.cloneTargetMap)
	if !sourceOK {
		setText(app.cloneSourceInfo, "Nenhum disco físico selecionado como origem.")
	} else {
		usage := "disco secundário"
		if diskContainsProtectedSystemRole(source) {
			if selectedCloneMode() == 1 || selectedCloneMode() == 2 {
				usage = "Windows em uso — protegido por snapshots VSS durante a migração"
			} else {
				usage = "Windows em uso — selecione um modo de migração com VSS"
			}
		}
		setText(app.cloneSourceInfo, fmt.Sprintf("PhysicalDrive%d | %s | %s | setor %d bytes | %s", source.Index, humanBytes(source.Size), source.Model, source.BytesPerSector, usage))
	}
	if !targetOK {
		setText(app.cloneTargetInfo, "Nenhum disco físico selecionado como destino.")
		return
	}
	letters := "sem volumes montados"
	if len(target.DriveLetters) > 0 {
		letters = "volumes: " + strings.Join(target.DriveLetters, ", ")
	}
	state := "pronto para validação"
	if diskContainsProtectedSystemRole(target) {
		state = "PROTEGIDO: contém o Windows em uso"
	} else if sourceOK && target.Index == source.Index {
		state = "INVÁLIDO: é o mesmo disco da origem"
	} else if sourceOK && target.Size < source.Size {
		if selectedCloneMode() == 2 {
			state = "SSD menor — será analisado pelo modo inteligente"
		} else {
			state = "INVÁLIDO neste modo: capacidade menor que a origem"
		}
	}
	setText(app.cloneTargetInfo, fmt.Sprintf("PhysicalDrive%d | %s | %s | %s | %s", target.Index, humanBytes(target.Size), target.Model, letters, state))
	if app.activeTab == 1 {
		updateCloneModeUI()
	}
}

func defaultCloneReportRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultDestination()
	}
	documents := filepath.Join(home, "Documents")
	if _, err := os.Stat(documents); err != nil {
		documents = home
	}
	return filepath.Join(documents, "HdRecover", "Clonagens")
}

func cloneSessionDir(base string, source, target diskInfo) string {
	return filepath.Join(base, fmt.Sprintf("Disco%d_para_Disco%d", source.Index, target.Index))
}

func isLocalDrivePath(path string) bool {
	volume := filepath.VolumeName(strings.TrimSpace(path))
	return len(volume) >= 2 && volume[1] == ':'
}

func startClone() {
	if !isAdmin() {
		messageBox(app.hwnd, "A clonagem exige privilégios de administrador. Feche o aplicativo e use INICIAR-HdRecover-COMO-ADMIN.cmd.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	app.state.mu.Lock()
	if app.state.running || app.state.disksLoading {
		app.state.mu.Unlock()
		return
	}
	app.state.mu.Unlock()

	source, ok := selectedCloneDisk(app.cloneSource, app.cloneSourceMap)
	if !ok {
		messageBox(app.hwnd, "Selecione o disco físico de origem.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	target, ok := selectedCloneDisk(app.cloneTarget, app.cloneTargetMap)
	if !ok {
		messageBox(app.hwnd, "Selecione o disco físico de destino.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	mode := selectedCloneMode()
	liveVSS := mode == 1 || mode == 2
	smartMode := mode == 2
	if source.Index == target.Index {
		messageBox(app.hwnd, "Origem e destino não podem ser o mesmo disco.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if source.CustomPath != "" || target.CustomPath != "" {
		messageBox(app.hwnd, "A clonagem direta desta versão aceita somente discos físicos.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if !source.MetadataVerified || !target.MetadataVerified {
		messageBox(app.hwnd, "O Windows não confirmou os metadados de segurança da origem e do destino. Atualize a lista de discos; se a identificação continuar incompleta, a clonagem permanecerá bloqueada.", appTitle, MB_OK|MB_ICONERROR)
		return
	}
	if !source.HasHardwareIdentity() || !target.HasHardwareIdentity() {
		messageBox(app.hwnd, "A clonagem exige serial ou identificador físico confirmado nos dois discos. Conecte-os por interfaces que exponham a identidade do hardware e atualize a lista.", appTitle, MB_OK|MB_ICONERROR)
		return
	}
	if target.Size < source.Size && !smartMode {
		messageBox(app.hwnd, "O SSD de destino é menor que o disco de origem. Selecione “Migrar Windows para SSD menor — inteligente” para analisar os dados e reduzir somente a partição NTFS necessária.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if source.BytesPerSector != target.BytesPerSector {
		messageBox(app.hwnd, fmt.Sprintf("Os discos apresentam setores lógicos diferentes (%d e %d bytes). A migração direta preservando o Windows não é segura entre esses dois dispositivos.", source.BytesPerSector, target.BytesPerSector), appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if diskContainsProtectedSystemRole(target) {
		messageBox(app.hwnd, "O destino possui função de boot ou sistema do Windows em uso. Por segurança, esse disco nunca pode ser sobrescrito.", appTitle, MB_OK|MB_ICONERROR)
		return
	}
	if liveVSS {
		if !diskContainsProtectedSystemRole(source) {
			messageBox(app.hwnd, "O modo de migração deve usar como origem o disco que contém o Windows atualmente em execução. Para discos secundários, selecione o clone setor a setor exato.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
		if strings.ToUpper(strings.TrimSpace(getText(app.cloneConfirm))) != "MIGRAR" {
			messageBox(app.hwnd, "Digite exatamente MIGRAR no campo de confirmação antes de iniciar.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
	} else {
		if diskContainsProtectedSystemRole(source) {
			messageBox(app.hwnd, "A origem contém o Windows atualmente em uso. Selecione um dos modos de migração com VSS.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
		if strings.ToUpper(strings.TrimSpace(getText(app.cloneConfirm))) != "CLONAR" {
			messageBox(app.hwnd, "Digite exatamente CLONAR no campo de confirmação antes de iniciar.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
	}

	reportBase := strings.TrimSpace(getText(app.cloneReportEdit))
	if reportBase == "" {
		messageBox(app.hwnd, "Escolha uma pasta segura para salvar a sessão e os relatórios da clonagem.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if !filepath.IsAbs(reportBase) {
		messageBox(app.hwnd, "A pasta de relatório deve ser um caminho absoluto em outra unidade local ou em uma pasta de rede.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	reportDisk, reportMapErr := physicalDiskForPathNative(reportBase)
	if isLocalDrivePath(reportBase) && reportMapErr != nil {
		messageBox(app.hwnd, "Não foi possível confirmar em qual disco físico a pasta de relatório está localizada. Escolha outra unidade local ou uma pasta de rede.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if reportMapErr == nil {
		if reportDisk == target.Index || (!liveVSS && reportDisk == source.Index) {
			messageBox(app.hwnd, "A pasta de relatório não pode ficar no disco de destino. No clone exato offline, ela também não pode ficar na origem. Escolha outro armazenamento ou uma pasta de rede.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
	}
	reportDir := cloneSessionDir(reportBase, source, target)

	var smartPlan *smartMigrationPlan
	if smartMode {
		setText(app.status, "Analisando partições, espaço usado e limite seguro de redução...")
		plan, err := analyzeSmartMigration(source, target)
		if err != nil {
			setText(app.status, "A migração inteligente não pôde ser preparada")
			messageBox(app.hwnd, "O SSD menor não pode receber esta instalação com segurança:\n\n"+err.Error()+"\n\nNenhuma alteração foi feita no HD ou no SSD.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
		smartPlan = &plan
	}

	damageSelection, _, _ := procSendMessageW.Call(app.cloneDamage, CB_GETCURSEL, 0, 0)
	damage := recovery.DamageBalanced
	switch int(damageSelection) {
	case 0:
		damage = recovery.DamageFast
	case 2:
		damage = recovery.DamageCareful
	}
	performanceSelection, _, _ := procSendMessageW.Call(app.clonePerformance, CB_GETCURSEL, 0, 0)
	chunkSize := 32 * 1024 * 1024
	performanceName := "Automático / Turbo (32 MB)"
	if int(performanceSelection) == 1 {
		chunkSize = 4 * 1024 * 1024
		performanceName = "Compatibilidade (4 MB)"
	}
	verify := isChecked(app.cloneVerify)
	resume := isChecked(app.cloneResume)
	modeName := "clone setor a setor exato com a origem offline"
	planText := ""
	copyTotal := source.Size
	if mode == 1 {
		resume = false
		modeName = "migração completa do Windows em uso com snapshots VSS"
	} else if smartMode {
		resume = false
		modeName = "migração inteligente do Windows para SSD menor"
		copyTotal = smartPlan.CopySize
		planText = "\n\nPLANO AUTOMÁTICO:\n" + smartPlan.Summary()
	}
	if resume && (!source.HasHardwareIdentity() || !target.HasHardwareIdentity()) {
		messageBox(app.hwnd, "A retomada exige serial ou identificador físico confirmado nos dois discos. Desmarque a retomada para iniciar uma sessão nova ou conecte os discos por uma interface que exponha essa identificação.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}

	confirmation := fmt.Sprintf(
		"CONFIRMAÇÃO FINAL\n\nORIGEM — será lida e preservada:\n%s\n\nDESTINO — SERÁ TOTALMENTE APAGADO:\n%s\n\nModo: %s\nLeitura: %s\nDesempenho: %s\nVerificação final: %t\nRetomada: %t\nRelatório: %s%s\n\nTodos os dados atuais do destino serão perdidos. Não desligue o computador durante a operação. Deseja continuar?",
		source.Label(), target.Label(), modeName, damage.String(), performanceName, verify, resume, reportDir, planText,
	)
	if messageBox(app.hwnd, confirmation, appTitle, MB_YESNO|MB_ICONWARNING) != IDYES {
		return
	}

	cancel := make(chan struct{})
	started := time.Now()
	app.state.mu.Lock()
	app.state.running = true
	app.state.operationKind = "clone"
	app.state.paused = false
	app.state.cancel = cancel
	app.state.cancelOnce = sync.Once{}
	app.state.logHistory = nil
	app.state.logVersion++
	app.state.progress = 0
	app.state.files = 0
	if smartMode && smartPlan.RequiresShrink {
		app.state.status = "Preparando redução temporária da partição NTFS..."
	} else if liveVSS {
		app.state.status = "Criando snapshots VSS do Windows..."
	} else {
		app.state.status = "Bloqueando e desmontando o disco de destino..."
	}
	app.state.completed = false
	app.state.completion = ""
	app.state.outputDir = reportDir
	app.state.scanStarted = started
	app.state.bytesScanned = 0
	app.state.totalBytes = copyTotal
	app.state.speedBytes = 0
	app.state.eta = 0
	app.state.phase = "cloning"
	app.state.candidatesFound = 0
	app.state.candidatesProcessed = 0
	app.state.totalCandidates = 0
	app.state.readErrors = 0
	if smartMode {
		addLogLocked(&app.state, "Preparando migração inteligente. O programa validou que as partições essenciais cabem no SSD e salvará um plano de restauração da origem.")
	} else if liveVSS {
		addLogLocked(&app.state, "Preparando migração do Windows. O destino será bloqueado; a origem continuará em uso e será lida por cópias de sombra VSS.")
	} else {
		addLogLocked(&app.state, "Preparando clonagem física. O destino será bloqueado e desmontado antes da primeira gravação.")
	}
	app.state.mu.Unlock()
	requestUIUpdate()
	saveSettings()

	goSafe("clonagem", func() {
		runClone(source, target, reportDir, chunkSize, damage, verify, resume, liveVSS, smartPlan, cancel, started)
	})
}

func runClone(source, target diskInfo, reportDir string, chunkSize int, damage recovery.DamageMode, verify, resume, liveVSS bool, smartPlan *smartMigrationPlan, cancel <-chan struct{}, started time.Time) {
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		finishWithError("Não foi possível criar a pasta de relatório da clonagem:\n" + err.Error())
		return
	}
	logLine := func(line string) {
		app.state.mu.Lock()
		addLogLocked(&app.state, line)
		app.state.mu.Unlock()
		requestUIUpdateThrottled()
	}

	var access *cloneDiskAccess
	var snapshots *vssSnapshotSet
	sourceShrunk := false
	cleanupDone := false
	defer func() {
		if cleanupDone {
			return
		}
		if snapshots != nil {
			snapshots.Close()
		}
		if access != nil {
			access.Close()
		}
		if sourceShrunk && smartPlan != nil {
			_ = restoreSmartShrink(*smartPlan, reportDir, logLine)
		}
		goSafe("atualização pós-clonagem dos discos", refreshDisks)
	}()

	currentSource, currentTarget, err := revalidateCloneDisks(source, target)
	if err != nil {
		finishWithError("A identidade dos discos não pôde ser confirmada antes da operação:\n\n" + err.Error())
		return
	}
	source = currentSource
	target = currentTarget
	if smartPlan != nil {
		currentPlan, analyzeErr := analyzeSmartMigration(source, target)
		if analyzeErr != nil {
			finishWithError("O plano de migração não pôde ser revalidado antes de alterar a origem:\n\n" + analyzeErr.Error())
			return
		}
		if !smartPlansEquivalent(*smartPlan, currentPlan) {
			finishWithError("O mapa de discos ou partições mudou depois da confirmação. Nenhuma alteração foi iniciada; atualize os discos e confirme um novo plano.")
			return
		}
		smartPlan = &currentPlan
		planData, marshalErr := json.MarshalIndent(smartPlan, "", "  ")
		if marshalErr != nil {
			finishWithError("Não foi possível registrar o plano de migração:\n\n" + marshalErr.Error())
			return
		}
		if writeErr := os.WriteFile(filepath.Join(reportDir, "PLANO_MIGRACAO_INTELIGENTE.json"), planData, 0o644); writeErr != nil {
			finishWithError("Não foi possível salvar o plano de migração antes de alterar a origem:\n\n" + writeErr.Error())
			return
		}
		logLine(smartPlan.Summary())
		if smartPlan.RequiresShrink {
			sourceShrunk = true
			if err := applySmartShrink(*smartPlan, reportDir, logLine); err != nil {
				restoreErr := restoreSmartShrink(*smartPlan, reportDir, logLine)
				sourceShrunk = false
				message := "A migração inteligente foi interrompida antes de apagar o SSD:\n\n" + err.Error()
				if restoreErr != nil {
					message += "\n\nATENÇÃO NA ORIGEM: " + restoreErr.Error()
				}
				finishWithError(message)
				return
			}
		}
	}

	allowSmaller := smartPlan != nil && target.Size < source.Size
	access, err = openCloneDiskAccess(source, target, !liveVSS, allowSmaller, logLine)
	if err != nil {
		finishWithError("Não foi possível preparar os discos para clonagem:\n\n" + err.Error())
		return
	}

	var sourceReader io.ReaderAt = access.Source
	if liveVSS {
		snapshots, err = createVSSSnapshotSet(source, reportDir, logLine)
		if err != nil {
			finishWithError("Não foi possível criar uma leitura consistente do Windows com VSS:\n\n" + err.Error())
			return
		}
		sourceReader = snapshots.Reader(access.Source)
		resume = false
		logLine("Snapshots VSS ativos. Os volumes do Windows serão lidos em um estado consistente.")
	}

	copySize := source.Size
	smartMode := smartPlan != nil
	if smartMode {
		copySize = smartPlan.CopySize
	}
	if copySize <= 0 || copySize > target.Size || copySize > source.Size {
		finishWithError("O plano calculou um tamanho de cópia inválido e foi cancelado antes da gravação.")
		return
	}

	procSetThreadExecState.Call(ES_CONTINUOUS | ES_SYSTEM_REQUIRED)
	defer procSetThreadExecState.Call(ES_CONTINUOUS)

	lastBytes := int64(0)
	lastAt := started
	emaSpeed := float64(0)
	sourceSuffix := ""
	if liveVSS {
		sourceSuffix = " — VSS"
	}
	if smartMode {
		sourceSuffix += " — migração inteligente"
	}
	result, cloneErr := recovery.CloneDisk(recovery.CloneOptions{
		Source: sourceReader, Destination: access.Destination,
		SourceID:            source.Path() + " — " + source.Model + sourceSuffix,
		DestinationID:       target.Path() + " — " + target.Model,
		SourceIdentity:      source.StableIdentity(),
		DestinationIdentity: target.StableIdentity(),
		SourceSize:          copySize, DestinationSize: target.Size, SectorSize: source.BytesPerSector,
		ChunkSize: chunkSize, DamageMode: damage, Resume: resume, Verify: verify, AdjustGPT: true,
		ReportDir: reportDir, Cancel: cancel, Paused: isPaused,
		Progress: func(s recovery.CloneStatus) {
			now := time.Now()
			delta := now.Sub(lastAt).Seconds()
			if delta >= 0.15 && s.BytesProcessed >= lastBytes {
				instant := float64(s.BytesProcessed-lastBytes) / delta
				if emaSpeed == 0 {
					emaSpeed = instant
				} else {
					emaSpeed = emaSpeed*0.78 + instant*0.22
				}
				lastBytes = s.BytesProcessed
				lastAt = now
			}
			fraction := safeFraction(s.BytesProcessed, s.TotalBytes)
			percent := int(fraction * 85)
			statusPrefix := "Clonando"
			if smartMode {
				statusPrefix = "Migrando para SSD menor"
			} else if liveVSS {
				statusPrefix = "Migrando Windows"
			}
			status := fmt.Sprintf("%s — %d%% — %s de %s", statusPrefix, int(fraction*100), humanBytes(s.BytesProcessed), humanBytes(s.TotalBytes))
			eta := time.Duration(0)
			if emaSpeed > 0 && s.TotalBytes > s.BytesProcessed {
				eta = time.Duration(float64(s.TotalBytes-s.BytesProcessed)/emaSpeed) * time.Second
			}
			if s.Phase == "verifying" {
				percent = 85 + int(fraction*15)
				status = fmt.Sprintf("Verificando destino — %d%% — %s de %s", int(fraction*100), humanBytes(s.BytesProcessed), humanBytes(s.TotalBytes))
			}
			app.state.mu.Lock()
			app.state.progress = percent
			app.state.status = status
			app.state.phase = s.Phase
			app.state.bytesScanned = s.BytesProcessed
			app.state.totalBytes = s.TotalBytes
			app.state.speedBytes = emaSpeed
			app.state.eta = eta
			app.state.readErrors = s.ReadErrors + s.VerifyErrors
			app.state.mu.Unlock()
			requestUIUpdateThrottled()
		},
		Log: logLine,
	})

	if snapshots != nil {
		snapshots.Close()
		snapshots = nil
	}
	if access != nil {
		access.Close()
		access = nil
	}

	var restoreErr error
	if sourceShrunk && smartPlan != nil {
		restoreErr = restoreSmartShrink(*smartPlan, reportDir, logLine)
		sourceShrunk = false
	}
	if cloneErr == nil && liveVSS {
		setDiskOfflineBestEffort(target.Index, logLine)
	}
	cleanupDone = true
	goSafe("atualização pós-clonagem dos discos", refreshDisks)

	app.state.mu.Lock()
	app.state.running = false
	app.state.paused = false
	app.state.outputDir = result.ReportDir
	if app.state.outputDir == "" {
		app.state.outputDir = reportDir
	}
	app.state.readErrors = result.ReadErrors + result.VerifyErrors
	app.state.completed = true
	app.state.autoOpenAfter = false
	if cloneErr == nil {
		app.state.progress = 100
		app.state.bytesScanned = result.BytesCopied
		if smartMode {
			app.state.status = "Migração inteligente concluída"
		} else if liveVSS {
			app.state.status = "Migração do Windows concluída"
		} else {
			app.state.status = "Clonagem concluída com sucesso"
		}
		verification := "não solicitada"
		if result.Verified {
			verification = "aprovada bloco a bloco"
		}
		extra := ""
		if target.Size > source.Size {
			extra = "\nO espaço excedente do destino ficou não alocado e pode ser expandido depois pelo Gerenciamento de Disco."
		}
		if result.PartitionsDropped > 0 {
			extra += fmt.Sprintf("\n%d partição(ões) de recuperação que ficavam fora do SSD foram omitidas. O Windows deve iniciar normalmente; o Ambiente de Recuperação pode precisar ser reativado depois.", result.PartitionsDropped)
		}
		if restoreErr != nil {
			extra += "\n\nATENÇÃO NA ORIGEM: " + restoreErr.Error()
		} else if smartMode && smartPlan != nil && smartPlan.RequiresShrink {
			extra += "\nA partição temporariamente reduzida no HD original foi restaurada ao tamanho anterior."
		}
		if liveVSS {
			extra += "\n\nPRÓXIMO PASSO OBRIGATÓRIO: desligue totalmente o computador, desconecte o HD antigo e faça o primeiro boot somente com o SSD. Não formate o HD antigo antes de confirmar que o Windows iniciou e seus arquivos estão corretos."
		}
		gptStatus := "não necessária"
		if result.GPTAdjusted {
			gptStatus = "adaptada ao tamanho do disco de destino"
		}
		title := "Clonagem concluída."
		if smartMode {
			title = "Migração inteligente do Windows concluída."
		} else if liveVSS {
			title = "Migração completa do Windows concluída."
		}
		app.state.completion = fmt.Sprintf("%s\n\nOrigem: %s\nDestino: %s\nCopiado: %s\nSetores ilegíveis preenchidos com zero: %d\nVerificação: %s\nTabela de partições: %s\nTempo: %s\n\nRelatórios: %s%s", title, source.Label(), target.Label(), humanBytes(result.BytesCopied), result.ReadErrors, verification, gptStatus, formatDuration(result.Duration), result.ReportDir, extra)
		app.state.completionOK = result.VerifyErrors == 0 && restoreErr == nil
		if restoreErr != nil {
			app.state.status = "Migração concluída — restauração da origem requer atenção"
		}
	} else if errors.Is(cloneErr, recovery.ErrCancelled()) || strings.Contains(strings.ToLower(cloneErr.Error()), "cancel") {
		app.state.status = "Clonagem cancelada"
		if liveVSS {
			restoreNote := ""
			if restoreErr != nil {
				restoreNote = "\n\nATENÇÃO NA ORIGEM: " + restoreErr.Error()
			}
			app.state.completion = fmt.Sprintf("A migração foi cancelada com segurança em %s.\n\nO SSD ficou incompleto e NÃO deve ser usado para inicializar. Reinicie a migração desde o começo.\n\nRelatórios: %s%s", humanBytes(result.BytesCopied), reportDir, restoreNote)
		} else {
			app.state.completion = fmt.Sprintf("A clonagem foi cancelada com segurança em %s.\n\nA sessão foi mantida em:\n%s\n\nMantendo a opção de retomada marcada e escolhendo os mesmos discos, a próxima execução continuará do último bloco confirmado.", humanBytes(result.BytesCopied), reportDir)
		}
		app.state.completionOK = false
	} else {
		app.state.status = "Falha na clonagem"
		restoreNote := ""
		if restoreErr != nil {
			restoreNote = "\n\nATENÇÃO NA ORIGEM: " + restoreErr.Error()
		}
		app.state.completion = "Não foi possível concluir a clonagem:\n\n" + cloneErr.Error() + "\n\nConsulte os relatórios em:\n" + reportDir + restoreNote
		app.state.completionOK = false
	}
	app.state.mu.Unlock()
	requestUIUpdate()
}

func startRecovery() {
	if !isAdmin() {
		messageBox(app.hwnd, "A recuperação de disco físico exige privilégios de administrador. Feche o aplicativo e use INICIAR-HdRecover-COMO-ADMIN.cmd.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	app.state.mu.Lock()
	if app.state.running || app.state.disksLoading || app.state.partitionsLoading {
		app.state.mu.Unlock()
		return
	}
	disks := append([]diskInfo(nil), app.state.disks...)
	parts := append([]recovery.Partition(nil), app.state.partitions...)
	app.state.mu.Unlock()

	selection, _, _ := procSendMessageW.Call(app.diskCombo, CB_GETCURSEL, 0, 0)
	if int(selection) < 0 || int(selection) >= len(disks) {
		messageBox(app.hwnd, "Selecione o disco que será analisado.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	disk := disks[int(selection)]
	destination := strings.TrimSpace(getText(app.destEdit))
	if destination == "" {
		messageBox(app.hwnd, "Escolha a pasta onde os resultados serão salvos.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}

	mode := selectedMode()
	profile := selectedProfile()
	damage := selectedDamageMode()
	performance := recovery.PerformanceAutomatic
	performanceName := "Automático / Turbo"
	performanceSelection, _, _ := procSendMessageW.Call(app.performance, CB_GETCURSEL, 0, 0)
	if int(performanceSelection) == 1 {
		performance = recovery.PerformanceCompatibility
		performanceName = "Compatibilidade"
	}
	if profile == recovery.ProfileFormattedDrive {
		mode = recovery.ModeDeepCarving
	}
	if profile == recovery.ProfileDamagedDrive {
		damage = recovery.DamageCareful
		performance = recovery.PerformanceCompatibility
		performanceName = "Compatibilidade"
	}

	partition := selectedPartition()
	if partition.Size <= 0 {
		partition = recovery.Partition{Index: 0, Scheme: "RAW", Name: "Disco inteiro", Start: 0, Size: disk.Size}
	}
	var rangeErr error
	partition, rangeErr = applyCustomRange(partition)
	if rangeErr != nil {
		messageBox(app.hwnd, "Intervalo personalizado inválido:\n"+rangeErr.Error(), appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	if mode == recovery.ModeQuickNTFS && !strings.EqualFold(partition.FileSystem, "NTFS") {
		var best recovery.Partition
		for _, p := range parts {
			if strings.EqualFold(p.FileSystem, "NTFS") && p.Size > best.Size {
				best = p
			}
		}
		if best.Size > 0 {
			partition = best
		} else {
			messageBox(app.hwnd, "A verificação rápida precisa de uma partição NTFS válida. Selecione uma partição NTFS ou use a verificação profunda.", appTitle, MB_OK|MB_ICONWARNING)
			return
		}
	}

	categories := recovery.Categories{
		Images: isChecked(app.images), Documents: isChecked(app.documents), Videos: isChecked(app.videos),
		Audio: isChecked(app.audio), Archives: isChecked(app.archives),
	}
	if mode != recovery.ModeCreateImage && !categories.Images && !categories.Documents && !categories.Videos && !categories.Audio && !categories.Archives {
		messageBox(app.hwnd, "Marque pelo menos um tipo de arquivo.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}

	resume := isChecked(app.resume)
	dedup := isChecked(app.dedup)
	minSize := selectedMinSizeBytes()

	action := mode.String()
	freeSpaceMode := "Não"
	if mode == recovery.ModeDeepCarving && isChecked(app.onlyFree) {
		freeSpaceMode = "Sim — apenas clusters não alocados do NTFS"
	}
	confirm := fmt.Sprintf(
		"Origem:\n%s\n\nIntervalo:\n%s\n\nModo: %s\nPerfil: %s\nLeitura: %s\nDesempenho: %s\nSomente espaço livre: %s\nDestino: %s\n\nA origem será aberta somente para leitura. Deseja iniciar?",
		disk.Label(), partition.Label(), action, profile.String(), damage.String(), performanceName, freeSpaceMode, destination,
	)
	if messageBox(app.hwnd, confirm, appTitle, MB_YESNO|MB_ICONQUESTION) != IDYES {
		return
	}

	cancel := make(chan struct{})
	started := time.Now()
	app.state.mu.Lock()
	app.state.running = true
	app.state.operationKind = "recovery"
	app.state.paused = false
	app.state.cancel = cancel
	app.state.cancelOnce = sync.Once{}
	app.state.logHistory = nil
	app.state.logVersion++
	app.state.progress = 0
	app.state.files = 0
	app.state.status = "Validando origem e destino..."
	app.state.completed = false
	app.state.completion = ""
	app.state.outputDir = ""
	app.state.scanStarted = started
	app.state.bytesScanned = 0
	app.state.totalBytes = partition.Size
	app.state.speedBytes = 0
	app.state.eta = 0
	app.state.phase = ""
	app.state.candidatesFound = 0
	app.state.candidatesProcessed = 0
	app.state.totalCandidates = 0
	app.state.readErrors = 0
	addLogLocked(&app.state, "Preparando "+action+" em segundo plano.")
	app.state.mu.Unlock()
	requestUIUpdate()
	saveSettings()

	cfg := recovery.Options{
		SourcePath: disk.Path(), SourceSize: disk.Size, SectorSize: disk.BytesPerSector,
		Destination: destination, Categories: categories, Performance: performance,
		Mode: mode, Profile: profile, DamageMode: damage, Partition: partition,
		MinFileSize: minSize, SkipDuplicates: dedup, OnlyFreeSpace: isChecked(app.onlyFree), Resume: resume,
		ImageName: fmt.Sprintf("HdRecover_Disco%d_%s.img", disk.Index, time.Now().Format("20060102_150405")),
		Cancel:    cancel, Paused: isPaused,
	}
	goSafe("recuperação", func() { runRecovery(disk, cfg, started) })
}

func runRecovery(disk diskInfo, opts recovery.Options, started time.Time) {
	// Toda validação potencialmente lenta fica fora da thread da interface.
	destination := opts.Destination
	if err := os.MkdirAll(destination, 0o755); err != nil {
		finishWithError("Não foi possível usar a pasta de destino:\n" + err.Error())
		return
	}
	if disk.Index >= 0 {
		if destDisk, err := physicalDiskForPathNative(destination); err == nil && destDisk == disk.Index {
			finishWithError("A pasta de destino está no MESMO disco que será recuperado.\n\nEscolha outro HD, SSD, pendrive ou armazenamento de rede para não sobrescrever os arquivos apagados.")
			return
		}
	}
	if free, err := freeSpaceForPath(destination); err == nil {
		app.state.mu.Lock()
		addLogLocked(&app.state, "Espaço livre no destino: "+humanBytes(int64(free))+".")
		if free < 2*1024*1024*1024 {
			addLogLocked(&app.state, "Atenção: o destino tem menos de 2 GB livres.")
		}
		app.state.mu.Unlock()
		requestUIUpdate()
	}

	procSetThreadExecState.Call(ES_CONTINUOUS | ES_SYSTEM_REQUIRED)
	defer procSetThreadExecState.Call(ES_CONTINUOUS)

	var lastScanBytes int64
	lastSpeedAt := started
	emaSpeed := float64(0)

	result, err := recovery.Recover(recovery.Options{
		SourcePath:     opts.SourcePath,
		SourceSize:     opts.SourceSize,
		SectorSize:     opts.SectorSize,
		Destination:    destination,
		Categories:     opts.Categories,
		Performance:    opts.Performance,
		Mode:           opts.Mode,
		Profile:        opts.Profile,
		DamageMode:     opts.DamageMode,
		Partition:      opts.Partition,
		MinFileSize:    opts.MinFileSize,
		SkipDuplicates: opts.SkipDuplicates,
		OnlyFreeSpace:  opts.OnlyFreeSpace,
		Resume:         opts.Resume,
		ImageName:      opts.ImageName,
		Cancel:         opts.Cancel,
		Paused:         opts.Paused,
		Progress: func(s recovery.Status) {
			percent := 0
			speed := emaSpeed
			eta := time.Duration(0)
			status := ""

			// Calcula velocidade para qualquer fase baseada em bytes.
			now := time.Now()
			deltaTime := now.Sub(lastSpeedAt).Seconds()
			if deltaTime >= 0.15 && s.BytesScanned >= lastScanBytes {
				instant := float64(s.BytesScanned-lastScanBytes) / deltaTime
				if emaSpeed == 0 {
					emaSpeed = instant
				} else if instant >= 0 {
					emaSpeed = emaSpeed*0.78 + instant*0.22
				}
				lastScanBytes = s.BytesScanned
				lastSpeedAt = now
			}
			speed = emaSpeed

			switch s.Phase {
			case recovery.PhaseScanning:
				fraction := safeFraction(s.BytesScanned, s.TotalBytes)
				percent = int(fraction * 70)
				if speed > 0 && s.TotalBytes > s.BytesScanned {
					eta = time.Duration(float64(s.TotalBytes-s.BytesScanned)/speed) * time.Second
				}
				status = fmt.Sprintf("Varredura profunda — %d%% — %s de %s — %d candidato(s)", int(fraction*100), humanBytes(s.BytesScanned), humanBytes(s.TotalBytes), s.CandidatesFound)
			case "sorting":
				fraction := safeFraction(s.BytesScanned, s.TotalBytes)
				percent = 70 + int(fraction*5)
				status = fmt.Sprintf("Organizando índice de candidatos — %d%%", int(fraction*100))
			case recovery.PhaseExtracting:
				fraction := safeFraction(int64(s.CandidatesProcessed), int64(s.TotalCandidates))
				percent = 75 + int(fraction*25)
				if s.CurrentFileSize > 0 {
					filePercent := int(safeFraction(s.CurrentFileBytes, s.CurrentFileSize) * 100)
					status = fmt.Sprintf("Extraindo %s — %d%% (%s de %s)", s.CurrentFile, filePercent, humanBytes(s.CurrentFileBytes), humanBytes(s.CurrentFileSize))
				} else {
					status = fmt.Sprintf("Validando %d de %d — %d recuperado(s)", s.CandidatesProcessed, s.TotalCandidates, s.FilesFound)
				}
			case "quick_ntfs_scan":
				fraction := safeFraction(s.BytesScanned, s.TotalBytes)
				percent = int(fraction * 45)
				status = fmt.Sprintf("Verificação rápida — lendo MFT — %d%%", int(fraction*100))
			case "quick_ntfs":
				fraction := safeFraction(int64(s.CandidatesProcessed), int64(s.TotalCandidates))
				percent = 45 + int(fraction*55)
				if s.CurrentFileSize > 0 {
					status = fmt.Sprintf("MFT — recuperando %s — %d%%", s.CurrentFile, int(safeFraction(s.CurrentFileBytes, s.CurrentFileSize)*100))
				} else {
					status = fmt.Sprintf("MFT — %d de %d — %d recuperado(s)", s.CandidatesProcessed, s.TotalCandidates, s.FilesFound)
				}
			case "imaging":
				fraction := safeFraction(s.BytesScanned, s.TotalBytes)
				percent = int(fraction * 100)
				if speed > 0 && s.TotalBytes > s.BytesScanned {
					eta = time.Duration(float64(s.TotalBytes-s.BytesScanned)/speed) * time.Second
				}
				status = fmt.Sprintf("Criando imagem — %d%% — %s de %s", int(fraction*100), humanBytes(s.BytesScanned), humanBytes(s.TotalBytes))
			default:
				fraction := safeFraction(s.BytesScanned, s.TotalBytes)
				percent = int(fraction * 100)
				status = s.Current
			}

			if percent > 100 {
				percent = 100
			}

			app.state.mu.Lock()
			app.state.progress = percent
			app.state.files = s.FilesFound
			app.state.bytesScanned = s.BytesScanned
			app.state.totalBytes = s.TotalBytes
			app.state.speedBytes = speed
			app.state.eta = eta
			app.state.phase = s.Phase
			app.state.candidatesFound = s.CandidatesFound
			app.state.candidatesProcessed = s.CandidatesProcessed
			app.state.totalCandidates = s.TotalCandidates
			app.state.readErrors = s.ReadErrors
			app.state.status = status
			app.state.mu.Unlock()
			requestUIUpdateThrottled()
		},
		Log: func(line string) {
			app.state.mu.Lock()
			addLogLocked(&app.state, line)
			app.state.mu.Unlock()
			requestUIUpdateThrottled()
		},
	})

	app.state.mu.Lock()
	app.state.running = false
	app.state.paused = false
	app.state.outputDir = result.OutputDir
	app.state.files = result.FilesFound
	app.state.totalCandidates = result.Candidates
	app.state.readErrors = result.ReadErrors
	app.state.completed = true
	if err == nil {
		app.state.progress = 100
		app.state.bytesScanned = result.BytesScanned
		if opts.Mode == recovery.ModeCreateImage {
			app.state.status = "Imagem criada com sucesso"
			app.state.completion = fmt.Sprintf("Imagem de segurança concluída.\n\nArquivo: %s\nTrechos problemáticos: %d\nDados copiados: %s\nTempo total: %s\n\nO mapa de leitura e os relatórios estão em: %s", result.ImagePath, result.ReadErrors, humanBytes(result.BytesScanned), formatDuration(result.Duration), result.OutputDir)
			app.state.completionOK = true
		} else if result.FilesFound == 0 {
			app.state.status = "Concluído — nenhum arquivo recuperável encontrado"
			app.state.completion = fmt.Sprintf("A análise terminou, mas nenhum arquivo recuperável foi confirmado.\n\nCandidatos: %d\nFalhas de leitura: %d\nTempo: %s\n\nRelatórios: %s\n\nIsso pode ocorrer quando os dados foram sobrescritos, o SSD executou TRIM, a MFT foi reutilizada ou os dados estão criptografados.", result.Candidates, result.ReadErrors, formatDuration(result.Duration), result.OutputDir)
			app.state.completionOK = false
		} else {
			app.state.status = fmt.Sprintf("Concluído — %d arquivo(s) recuperado(s)", result.FilesFound)
			app.state.completion = fmt.Sprintf("Recuperação concluída.\n\nArquivos recuperados: %d\nCandidatos analisados: %d\nFalhas de leitura: %d\nVarredura: %s\nExtração: %s\nTempo total: %s\n\nPasta: %s\n\nAbra RESULTADOS.html para visualizar e revisar a integridade.", result.FilesFound, result.Candidates, result.ReadErrors, formatDuration(result.ScanDuration), formatDuration(result.ExtractionDuration), formatDuration(result.Duration), result.OutputDir)
			app.state.completionOK = true
		}
	} else if errors.Is(err, recovery.ErrCancelled()) || strings.Contains(strings.ToLower(err.Error()), "cancel") {
		app.state.status = fmt.Sprintf("Cancelado — %d arquivo(s) recuperado(s)", result.FilesFound)
		app.state.completion = fmt.Sprintf("A recuperação foi cancelada.\n\nOs %d arquivo(s) já recuperados permanecem salvos em:\n%s", result.FilesFound, result.OutputDir)
		app.state.completionOK = false
	} else {
		app.state.status = "Falha na recuperação"
		app.state.completion = "Não foi possível concluir a recuperação:\n\n" + err.Error()
		app.state.completionOK = false
	}
	app.state.autoOpenAfter = isAutoOpenConfigured() && app.state.completionOK && app.state.outputDir != ""
	app.state.mu.Unlock()
	requestUIUpdate()
}

func finishWithError(message string) {
	app.state.mu.Lock()
	app.state.running = false
	app.state.completed = true
	app.state.completionOK = false
	if app.state.operationKind == "clone" {
		app.state.status = "Não foi possível iniciar a clonagem"
	} else {
		app.state.status = "Não foi possível iniciar a recuperação"
	}
	app.state.completion = message
	addLogLocked(&app.state, strings.ReplaceAll(message, "\n", " "))
	app.state.mu.Unlock()
	requestUIUpdate()
}

func cancelRecovery() {
	app.state.mu.Lock()
	running := app.state.running
	app.state.mu.Unlock()
	if !running {
		return
	}
	app.state.mu.Lock()
	kind := app.state.operationKind
	app.state.mu.Unlock()
	text := "Deseja cancelar a recuperação? Os arquivos já encontrados serão mantidos."
	if kind == "clone" {
		if selectedCloneMode() == 1 || selectedCloneMode() == 2 {
			text = "Deseja cancelar a migração do Windows? O SSD ficará incompleto e a operação precisará ser reiniciada desde o começo. A partição temporariamente reduzida será restaurada antes de encerrar."
		} else {
			text = "Deseja cancelar a clonagem? A sessão será salva no último bloco confirmado para permitir retomada."
		}
	}
	if messageBox(app.hwnd, text, appTitle, MB_YESNO|MB_ICONWARNING) != IDYES {
		return
	}
	cancelRecoveryWithoutPrompt()
}

func cancelRecoveryWithoutPrompt() {
	app.state.mu.Lock()
	if app.state.running && app.state.cancel != nil {
		app.state.cancelOnce.Do(func() { close(app.state.cancel) })
		app.state.status = "Cancelando com segurança..."
		addLogLocked(&app.state, "Cancelamento solicitado. Finalizando a operação atual com segurança.")
	}
	app.state.mu.Unlock()
	requestUIUpdate()
}

func updateUI() {
	app.state.mu.Lock()
	disks := append([]diskInfo(nil), app.state.disks...)
	diskVersion := app.state.diskVersion
	appliedVersion := app.state.appliedVersion
	partitions := append([]recovery.Partition(nil), app.state.partitions...)
	partitionVersion := app.state.partitionVersion
	appliedPartitionVersion := app.state.appliedPartitionVersion
	partitionLoading := app.state.partitionsLoading
	paused := app.state.paused
	progress := app.state.progress
	status := app.state.status
	completed := app.state.completed
	completion := app.state.completion
	completionOK := app.state.completionOK
	running := app.state.running
	loading := app.state.disksLoading
	outputDir := app.state.outputDir
	logVersion := app.state.logVersion
	logs := append([]string(nil), app.state.logHistory...)
	speed := app.state.speedBytes
	eta := app.state.eta
	bytesScanned := app.state.bytesScanned
	totalBytes := app.state.totalBytes
	files := app.state.files
	phase := app.state.phase
	candidatesFound := app.state.candidatesFound
	candidatesProcessed := app.state.candidatesProcessed
	totalCandidates := app.state.totalCandidates
	readErrors := app.state.readErrors
	operationKind := app.state.operationKind
	autoOpenAfter := app.state.autoOpenAfter
	app.state.completed = false
	app.state.autoOpenAfter = false
	app.state.mu.Unlock()

	if diskVersion != appliedVersion && !running {
		oldSelection, _, _ := procSendMessageW.Call(app.diskCombo, CB_GETCURSEL, 0, 0)
		oldSourceDiskIndex := -1
		oldTargetDiskIndex := -1
		if sel, _, _ := procSendMessageW.Call(app.cloneSource, CB_GETCURSEL, 0, 0); int(sel) >= 0 && int(sel) < len(app.cloneSourceMap) {
			i := app.cloneSourceMap[int(sel)]
			if i >= 0 && i < len(disks) {
				oldSourceDiskIndex = disks[i].Index
			}
		}
		if sel, _, _ := procSendMessageW.Call(app.cloneTarget, CB_GETCURSEL, 0, 0); int(sel) >= 0 && int(sel) < len(app.cloneTargetMap) {
			i := app.cloneTargetMap[int(sel)]
			if i >= 0 && i < len(disks) {
				oldTargetDiskIndex = disks[i].Index
			}
		}

		procSendMessageW.Call(app.diskCombo, CB_RESETCONTENT, 0, 0)
		procSendMessageW.Call(app.cloneSource, CB_RESETCONTENT, 0, 0)
		procSendMessageW.Call(app.cloneTarget, CB_RESETCONTENT, 0, 0)
		app.cloneSourceMap = app.cloneSourceMap[:0]
		app.cloneTargetMap = app.cloneTargetMap[:0]
		for i, d := range disks {
			p := utf16Ptr(d.Label())
			procSendMessageW.Call(app.diskCombo, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(p)))
			if d.CustomPath == "" {
				procSendMessageW.Call(app.cloneSource, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(d.Label()))))
				procSendMessageW.Call(app.cloneTarget, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(d.Label()))))
				app.cloneSourceMap = append(app.cloneSourceMap, i)
				app.cloneTargetMap = append(app.cloneTargetMap, i)
			}
		}
		selection := int(oldSelection)
		app.state.mu.Lock()
		preferred := app.state.preferredDiskSelection
		app.state.preferredDiskSelection = -1
		app.state.mu.Unlock()
		if preferred >= 0 && preferred < len(disks) {
			selection = preferred
		}
		if selection < 0 || selection >= len(disks) {
			selection = 0
		}
		if len(disks) > 0 {
			procSendMessageW.Call(app.diskCombo, CB_SETCURSEL, uintptr(selection), 0)
		}

		sourceSel := -1
		targetSel := -1
		for comboIndex, diskIndex := range app.cloneSourceMap {
			d := disks[diskIndex]
			if d.Index == oldSourceDiskIndex {
				sourceSel = comboIndex
			}
			if d.Index == oldTargetDiskIndex {
				targetSel = comboIndex
			}
		}
		if sourceSel < 0 {
			for comboIndex, diskIndex := range app.cloneSourceMap {
				if !diskContainsProtectedSystemRole(disks[diskIndex]) {
					sourceSel = comboIndex
					break
				}
			}
		}
		if sourceSel < 0 && len(app.cloneSourceMap) > 0 {
			sourceSel = 0
		}
		if targetSel < 0 {
			for comboIndex, diskIndex := range app.cloneTargetMap {
				d := disks[diskIndex]
				if comboIndex != sourceSel && !diskContainsProtectedSystemRole(d) {
					targetSel = comboIndex
					break
				}
			}
		}
		if targetSel < 0 && len(app.cloneTargetMap) > 1 {
			if sourceSel == 0 {
				targetSel = 1
			} else {
				targetSel = 0
			}
		}
		if sourceSel >= 0 {
			procSendMessageW.Call(app.cloneSource, CB_SETCURSEL, uintptr(sourceSel), 0)
		}
		if targetSel >= 0 {
			procSendMessageW.Call(app.cloneTarget, CB_SETCURSEL, uintptr(targetSel), 0)
		}

		app.state.mu.Lock()
		app.state.appliedVersion = diskVersion
		app.state.mu.Unlock()
		updateSelectedDiskInfo()
		updateCloneDiskInfo()
		goSafe("atualização pós-varredura das partições", refreshPartitions)
	}

	if partitionVersion != appliedPartitionVersion && !running {
		oldSelection, _, _ := procSendMessageW.Call(app.partition, CB_GETCURSEL, 0, 0)
		procSendMessageW.Call(app.partition, CB_RESETCONTENT, 0, 0)
		for _, part := range partitions {
			label := part.Label()
			if part.Index == 0 && part.Name == "Disco inteiro" {
				label = "Disco inteiro — " + humanBytes(part.Size)
			}
			procSendMessageW.Call(app.partition, CB_ADDSTRING, 0, uintptr(unsafe.Pointer(utf16Ptr(label))))
		}
		selection := int(oldSelection)
		if selection < 0 || selection >= len(partitions) {
			selection = 0
		}
		if len(partitions) > 0 {
			procSendMessageW.Call(app.partition, CB_SETCURSEL, uintptr(selection), 0)
		}
		app.state.mu.Lock()
		app.state.appliedPartitionVersion = partitionVersion
		app.state.mu.Unlock()
	}

	if paused && running {
		setText(app.pauseBtn, "Continuar")
	} else {
		setText(app.pauseBtn, "Pausar")
	}
	if partitionLoading && !running && app.activeTab == 0 {
		setText(app.status, "Lendo tabela de partições em segundo plano...")
	}

	if logVersion != app.lastLogVersion {
		text := strings.Join(logs, "\r\n")
		if text != "" {
			text += "\r\n"
		}
		setText(app.logEdit, text)
		length, _, _ := procGetWindowTextLengthW.Call(app.logEdit)
		procSendMessageW.Call(app.logEdit, EM_SETSEL, length, length)
		procSendMessageW.Call(app.logEdit, EM_SCROLLCARET, 0, 0)
		app.lastLogVersion = logVersion
	}

	if progress != app.lastProgress {
		procSendMessageW.Call(app.progress, PBM_SETPOS, uintptr(progress), 0)
		app.lastProgress = progress
	}
	if status != "" && status != app.lastStatus {
		setText(app.status, status)
		app.lastStatus = status
	}

	stats := ""
	if running && totalBytes > 0 {
		switch phase {
		case "cloning", "verifying":
			stats = fmt.Sprintf("Velocidade: %s/s  |  Restante estimado: %s  |  Processado: %s de %s  |  Falhas: %d", humanBytes(int64(speed)), formatDuration(eta), humanBytes(bytesScanned), humanBytes(totalBytes), readErrors)
		case recovery.PhaseScanning, "quick_ntfs_scan", "imaging":
			stats = fmt.Sprintf("Velocidade: %s/s  |  Restante estimado: %s  |  Processado: %s de %s  |  Falhas: %d", humanBytes(int64(speed)), formatDuration(eta), humanBytes(bytesScanned), humanBytes(totalBytes), readErrors)
		case "sorting":
			stats = fmt.Sprintf("Organizando índice em disco  |  Candidatos: %d  |  RAM mantida sob controle", candidatesFound)
		default:
			stats = fmt.Sprintf("Candidatos: %d de %d  |  Recuperados: %d  |  Falhas: %d", candidatesProcessed, totalCandidates, files, readErrors)
		}
	} else if outputDir != "" {
		if operationKind == "clone" {
			stats = fmt.Sprintf("Relatório da clonagem: %s  |  Falhas registradas: %d", outputDir, readErrors)
		} else {
			stats = fmt.Sprintf("Recuperados: %d  |  Candidatos: %d  |  Falhas: %d  |  Pasta: %s", files, totalCandidates, readErrors, outputDir)
		}
	}
	if stats != app.lastStats {
		setText(app.stats, stats)
		app.lastStats = stats
	}

	if !app.runningSet || !app.loadingSet || running != app.lastRunning || loading != app.lastLoading {
		setBusyControls(running, loading, len(disks) > 0)
		app.lastRunning = running
		app.lastLoading = loading
		app.runningSet = true
		app.loadingSet = true
	}
	enable(app.openFolder, !running && outputDir != "")

	if completed {
		procMessageBeep.Call(MB_ICONINFORMATION)
		flags := uintptr(MB_OK | MB_ICONINFORMATION)
		if !completionOK {
			flags = MB_OK | MB_ICONWARNING
		}
		messageBox(app.hwnd, completion, appTitle, flags)
		if autoOpenAfter && outputDir != "" {
			shellOpen(outputDir)
		}
	}
}

func setBusyControls(running, loading, hasDisks bool) {
	physicalCount := len(app.cloneSourceMap)
	enable(app.tabRecovery, !running)
	enable(app.tabClone, !running)

	// Recuperação
	enable(app.diskCombo, !running && !loading && hasDisks)
	enable(app.refresh, !running && !loading)
	enable(app.openImage, !running)
	for _, h := range []uintptr{
		app.destEdit, app.browse, app.images, app.documents, app.videos, app.audio,
		app.archives, app.selectAll, app.autoOpen, app.performance, app.mode, app.partition,
		app.profile, app.damage, app.resume, app.dedup, app.onlyFree, app.minSize, app.rangeStart, app.rangeEnd,
	} {
		enable(h, !running)
	}

	// Clonagem
	enable(app.cloneSource, !running && !loading && physicalCount > 0)
	enable(app.cloneTarget, !running && !loading && physicalCount > 1)
	enable(app.cloneRefresh, !running && !loading)
	for _, h := range []uintptr{
		app.cloneMode, app.cloneDamage, app.clonePerformance, app.cloneVerify, app.cloneResume,
		app.cloneReportEdit, app.cloneReportBrowse, app.cloneConfirm,
	} {
		enable(h, !running)
	}

	canStart := !running && !loading
	if app.activeTab == 1 {
		canStart = canStart && physicalCount > 1
	} else {
		canStart = canStart && hasDisks
	}
	enable(app.start, canStart)
	enable(app.pauseBtn, running)
	enable(app.cancelBtn, running)
	if !running && app.activeTab == 0 {
		updateModeUI()
	} else if !running && app.activeTab == 1 {
		updateCloneModeUI()
	}
}

func updateSelectedDiskInfo() {
	app.state.mu.Lock()
	disks := append([]diskInfo(nil), app.state.disks...)
	app.state.mu.Unlock()
	selection, _, _ := procSendMessageW.Call(app.diskCombo, CB_GETCURSEL, 0, 0)
	if int(selection) < 0 || int(selection) >= len(disks) {
		setText(app.diskInfo, "Nenhum disco selecionado.")
		return
	}
	d := disks[int(selection)]
	app.state.mu.Lock()
	app.state.selectedDisk = int(selection)
	app.state.mu.Unlock()
	if d.CustomPath != "" {
		setText(app.diskInfo, fmt.Sprintf("Imagem local | %s | %s | Setor lógico assumido: %d bytes", d.CustomPath, humanBytes(d.Size), d.BytesPerSector))
		setText(app.warning, "A imagem será analisada sem alterar o arquivo original. Os resultados ainda devem ser salvos em outra pasta.")
		return
	}
	letters := "sem letra de unidade"
	if len(d.DriveLetters) > 0 {
		letters = strings.Join(d.DriveLetters, ", ")
	}
	media := "HDD"
	if d.TrimEnabled || !d.SeekPenalty || d.InterfaceType == "NVMe" {
		media = "SSD"
	}
	trim := "TRIM não detectado"
	if d.TrimEnabled {
		trim = "TRIM ativo"
	}
	setText(app.diskInfo, fmt.Sprintf("Disco físico %d | %s | %s/%s | Setor: %d | %s | SMART: %s | Volumes: %s", d.Index, humanBytes(d.Size), d.InterfaceType, media, d.BytesPerSector, trim, d.Health, letters))
	if d.TrimEnabled {
		setText(app.warning, "Atenção: este SSD informa TRIM ativo. Arquivos apagados podem já ter sido eliminados fisicamente; tente primeiro a MFT NTFS.")
	} else if strings.Contains(strings.ToLower(d.Health), "falha") && !strings.Contains(strings.ToLower(d.Health), "sem falha") {
		setText(app.warning, "Atenção: o disco informa possível falha. Use 'Criar imagem/clone' e leitura 'Disco danificado' antes de recuperar arquivos.")
	} else {
		setText(app.warning, "A origem é aberta somente para leitura. Salve sempre em outro disco físico.")
	}
}

func updateDestinationInfo(path string) {
	if free, err := freeSpaceForPath(path); err == nil {
		setText(app.destInfo, "Espaço livre no destino: "+humanBytes(int64(free))+". O programa verificará se é outro disco físico.")
	} else {
		setText(app.destInfo, "Destino selecionado. A unidade será validada antes de começar.")
	}
}

func updateSelectAllState() {
	all := true
	for _, h := range []uintptr{app.images, app.documents, app.videos, app.audio, app.archives} {
		if !isChecked(h) {
			all = false
			break
		}
	}
	setChecked(app.selectAll, all)
}

func addLogLocked(state *uiState, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	stamped := time.Now().Format("15:04:05") + "  " + line
	state.logHistory = append(state.logHistory, stamped)
	const maxLines = 300
	if len(state.logHistory) > maxLines {
		state.logHistory = append([]string(nil), state.logHistory[len(state.logHistory)-maxLines:]...)
	}
	state.logVersion++
}

func requestUIUpdateThrottled() {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&uiLastNotifyNanos)
	if now-last < int64(100*time.Millisecond) {
		return
	}
	if atomic.CompareAndSwapInt64(&uiLastNotifyNanos, last, now) {
		requestUIUpdate()
	}
}

func requestUIUpdate() {
	hwnd := app.hwnd
	if hwnd == 0 {
		return
	}
	if atomic.CompareAndSwapInt32(&uiUpdatePending, 0, 1) {
		procPostMessageW.Call(hwnd, WM_APP_UPDATE, 0, 0)
	}
}

func defaultDestination() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	desktop := filepath.Join(home, "Desktop")
	if _, err := os.Stat(desktop); err == nil {
		return desktop
	}
	return home
}

func settingsPath() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	return filepath.Join(base, "HdRecover", "settings.json")
}

func loadSettingsIntoUI() {
	settings := appSettings{Destination: defaultDestination(), AutoOpen: true, Performance: 0, Mode: 0, Profile: 0, Damage: 1, Resume: true, Dedup: true, MinSizeKB: 4, OnlyFree: false, CloneReport: defaultCloneReportRoot(), CloneMode: 0, CloneDamage: 1, ClonePerformance: 0, CloneVerify: true, CloneResume: true}
	if data, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if strings.TrimSpace(settings.Destination) == "" {
		settings.Destination = defaultDestination()
	}
	if settings.Performance < 0 || settings.Performance > 1 {
		settings.Performance = 0
	}
	if settings.Mode < 0 || settings.Mode > 2 {
		settings.Mode = 0
	}
	if settings.Profile < 0 || settings.Profile > 5 {
		settings.Profile = 0
	}
	if settings.Damage < 0 || settings.Damage > 2 {
		settings.Damage = 1
	}
	if settings.MinSizeKB < 0 {
		settings.MinSizeKB = 0
	}
	if strings.TrimSpace(settings.CloneReport) == "" {
		settings.CloneReport = defaultCloneReportRoot()
	}
	if settings.CloneMode < 0 || settings.CloneMode > 2 {
		settings.CloneMode = 0
	}
	if settings.CloneDamage < 0 || settings.CloneDamage > 2 {
		settings.CloneDamage = 1
	}
	if settings.ClonePerformance < 0 || settings.ClonePerformance > 1 {
		settings.ClonePerformance = 0
	}

	setText(app.destEdit, settings.Destination)
	setChecked(app.autoOpen, settings.AutoOpen)
	setChecked(app.resume, settings.Resume)
	setChecked(app.dedup, settings.Dedup)
	setChecked(app.onlyFree, settings.OnlyFree)
	setText(app.minSize, strconv.Itoa(settings.MinSizeKB))
	procSendMessageW.Call(app.performance, CB_SETCURSEL, uintptr(settings.Performance), 0)
	procSendMessageW.Call(app.mode, CB_SETCURSEL, uintptr(settings.Mode), 0)
	procSendMessageW.Call(app.profile, CB_SETCURSEL, uintptr(settings.Profile), 0)
	procSendMessageW.Call(app.damage, CB_SETCURSEL, uintptr(settings.Damage), 0)
	setText(app.cloneReportEdit, settings.CloneReport)
	procSendMessageW.Call(app.cloneMode, CB_SETCURSEL, uintptr(settings.CloneMode), 0)
	setChecked(app.cloneVerify, settings.CloneVerify)
	setChecked(app.cloneResume, settings.CloneResume)
	procSendMessageW.Call(app.cloneDamage, CB_SETCURSEL, uintptr(settings.CloneDamage), 0)
	procSendMessageW.Call(app.clonePerformance, CB_SETCURSEL, uintptr(settings.ClonePerformance), 0)
	updateDestinationInfo(settings.Destination)
}

func saveSettings() {
	if app.destEdit == 0 || app.autoOpen == 0 || app.performance == 0 || app.mode == 0 {
		return
	}
	performance, _, _ := procSendMessageW.Call(app.performance, CB_GETCURSEL, 0, 0)
	mode, _, _ := procSendMessageW.Call(app.mode, CB_GETCURSEL, 0, 0)
	profile, _, _ := procSendMessageW.Call(app.profile, CB_GETCURSEL, 0, 0)
	damage, _, _ := procSendMessageW.Call(app.damage, CB_GETCURSEL, 0, 0)
	cloneMode, _, _ := procSendMessageW.Call(app.cloneMode, CB_GETCURSEL, 0, 0)
	cloneDamage, _, _ := procSendMessageW.Call(app.cloneDamage, CB_GETCURSEL, 0, 0)
	clonePerformance, _, _ := procSendMessageW.Call(app.clonePerformance, CB_GETCURSEL, 0, 0)
	minKB, _ := strconv.Atoi(strings.TrimSpace(getText(app.minSize)))
	if minKB < 0 {
		minKB = 0
	}
	settings := appSettings{
		Destination: strings.TrimSpace(getText(app.destEdit)), AutoOpen: isChecked(app.autoOpen),
		Performance: int(performance), Mode: int(mode), Profile: int(profile), Damage: int(damage),
		Resume: isChecked(app.resume), Dedup: isChecked(app.dedup), MinSizeKB: minKB, OnlyFree: isChecked(app.onlyFree),
		CloneReport: strings.TrimSpace(getText(app.cloneReportEdit)), CloneMode: int(cloneMode), CloneDamage: int(cloneDamage), ClonePerformance: int(clonePerformance),
		CloneVerify: isChecked(app.cloneVerify), CloneResume: isChecked(app.cloneResume),
	}
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

func isAutoOpenConfigured() bool {
	// A preferência é salva antes de iniciar; ler o JSON evita acessar controles fora da thread da janela.
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return true
	}
	var settings appSettings
	if json.Unmarshal(data, &settings) != nil {
		return true
	}
	return settings.AutoOpen
}

func openSourceImage() {
	path := browseImageFile(app.hwnd)
	if path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() <= 0 {
		messageBox(app.hwnd, "O arquivo de imagem selecionado é inválido.", appTitle, MB_OK|MB_ICONWARNING)
		return
	}
	d := diskInfo{Index: -1, Model: filepath.Base(path), Size: st.Size(), InterfaceType: "Arquivo", BytesPerSector: 512, Health: "Imagem local", CustomPath: path}
	app.state.mu.Lock()
	for i, existing := range app.state.disks {
		if existing.CustomPath != "" && strings.EqualFold(existing.CustomPath, path) {
			app.state.preferredDiskSelection = i
			app.state.diskVersion++
			app.state.mu.Unlock()
			requestUIUpdate()
			return
		}
	}
	app.state.disks = append(app.state.disks, d)
	app.state.preferredDiskSelection = len(app.state.disks) - 1
	app.state.diskVersion++
	app.state.status = "Imagem de disco adicionada como origem."
	addLogLocked(&app.state, "Imagem aberta em modo somente leitura: "+path)
	app.state.mu.Unlock()
	requestUIUpdate()
}

func browseImageFile(owner uintptr) string {
	file := make([]uint16, 32768)
	filter := utf16Multi("Imagens de disco (*.img;*.dd;*.raw;*.bin)\x00*.img;*.dd;*.raw;*.bin\x00Todos os arquivos (*.*)\x00*.*\x00\x00")
	of := openFileName{
		StructSize: uint32(unsafe.Sizeof(openFileName{})), Owner: owner,
		Filter: &filter[0], FilterIndex: 1, File: &file[0], MaxFile: uint32(len(file)),
		Title: utf16Ptr("Abrir imagem de disco em modo somente leitura"),
		Flags: 0x00001000 | 0x00000800 | 0x00080000,
	}
	ok, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&of)))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(file)
}

func utf16Multi(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

func browseFolder(owner uintptr) string {
	display := make([]uint16, 260)
	bi := browseInfo{
		Owner:       owner,
		DisplayName: &display[0],
		Title:       utf16Ptr("Escolha uma pasta em outro disco"),
		Flags:       BIF_RETURNONLYFSDIRS | BIF_EDITBOX | BIF_NEWDIALOGSTYLE,
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)
	path := make([]uint16, 32768)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(path)
}

func startupLogPath() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("TEMP"))
	}
	if base == "" {
		base = "."
	}
	dir := filepath.Join(base, "HdRecover")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "HdRecover_startup.log")
}

func logStartup(format string, args ...any) {
	startupLogMu.Lock()
	defer startupLogMu.Unlock()
	line := time.Now().Format("2006-01-02 15:04:05.000") + " | " + fmt.Sprintf(format, args...) + "\r\n"
	path := startupLogPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	_, _ = os.Stderr.WriteString(line)
}

func recoverFatal(stage string) {
	if recovered := recover(); recovered != nil {
		logStartup("Falha fatal em %s: %v\n%s", stage, recovered, debug.Stack())
		messageBox(0, fmt.Sprintf("O HdRecover encontrou uma falha ao iniciar.\n\nEtapa: %s\nErro: %v\n\nFoi criado um diagnóstico em:\n%s", stage, recovered, startupLogPath()), appTitle, MB_OK|MB_ICONERROR)
	}
}

func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logStartup("Falha na tarefa %s: %v\n%s", name, recovered, debug.Stack())
				app.state.mu.Lock()
				app.state.status = "Falha interna: " + name
				addLogLocked(&app.state, fmt.Sprintf("Erro interno em %s: %v. Consulte %s", name, recovered, startupLogPath()))
				app.state.running = false
				app.state.mu.Unlock()
				requestUIUpdate()
			}
		}()
		fn()
	}()
}

func isAdmin() bool {
	r, _, _ := procIsUserAnAdmin.Call()
	return r != 0
}

func relaunchAsAdmin() bool {
	exe, err := os.Executable()
	if err != nil {
		logStartup("os.Executable falhou: %v", err)
		return false
	}
	verb := utf16Ptr("runas")
	file := utf16Ptr(exe)
	r, _, callErr := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, SW_SHOWNORMAL)
	if r <= 32 {
		logStartup("ShellExecuteW(runas) falhou/cancelado: código=%d erro=%v", r, callErr)
	}
	return r > 32
}

func shellOpen(path string) {
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))), uintptr(unsafe.Pointer(utf16Ptr(path))), 0, 0, SW_SHOWNORMAL)
}

func isChecked(hwnd uintptr) bool {
	r, _, _ := procSendMessageW.Call(hwnd, BM_GETCHECK, 0, 0)
	return r == BST_CHECKED
}

func setChecked(hwnd uintptr, checked bool) {
	value := uintptr(BST_UNCHECKED)
	if checked {
		value = BST_CHECKED
	}
	procSendMessageW.Call(hwnd, BM_SETCHECK, value, 0)
}

func enable(hwnd uintptr, enabled bool) {
	if hwnd == 0 {
		return
	}
	v := uintptr(0)
	if enabled {
		v = 1
	}
	procEnableWindow.Call(hwnd, v)
}

func setText(hwnd uintptr, text string) {
	if hwnd == 0 {
		return
	}
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr(text))))
}

func getText(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

func messageBox(owner uintptr, text, title string, flags uintptr) int {
	r, _, _ := procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(utf16Ptr(text))), uintptr(unsafe.Pointer(utf16Ptr(title))), flags)
	return int(r)
}

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func humanBytes(v int64) string {
	if v < 0 {
		v = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	n := float64(v)
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

func safeFraction(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	v := float64(done) / float64(total)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "calculando..."
	}
	d = d.Round(time.Second)
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	d -= time.Duration(minutes) * time.Minute
	seconds := int(d / time.Second)
	if hours > 0 {
		return fmt.Sprintf("%dh %02dmin", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dmin %02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func rgb(r, g, b byte) uintptr {
	return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16
}
