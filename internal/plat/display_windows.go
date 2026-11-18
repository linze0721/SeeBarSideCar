package plat

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	monitorInfoFPrimary = 1
	mdtEffectiveDPI     = 0
	enumCurrentSettings = ^uintptr(0) // (DWORD)-1 as passed through Call
	dmPositionField     = 0x00000020
	dmPelsWidthField    = 0x00080000
	dmPelsHeightField   = 0x00100000
	cdsUpdateRegistry   = 0x00000001
	cdsNoReset          = 0x10000000
	qdcOnlyActivePaths  = 2
	dcGetSourceName     = 1
	dcGetSourceDPIScale = -3 // undocumented, see setSourceDPI
	dcSetSourceDPIScale = -4
	errInsufficientBuf  = 122
)

type monitorInfo struct {
	size    uint32
	monitor windows.Rect
	work    windows.Rect
	flags   uint32
}

type monitorInfoEx struct {
	monitorInfo
	device [32]uint16
}

// displayDevice is DISPLAY_DEVICEW.
type displayDevice struct {
	cb    uint32
	name  [32]uint16
	str   [128]uint16
	flags uint32
	id    [128]uint16
	key   [128]uint16
}

// devMode is the display variant of DEVMODEW (220 bytes).
type devMode struct {
	deviceName                                    [32]uint16
	specVersion, driverVersion, size, driverExtra uint16
	fields                                        uint32
	posX, posY                                    int32
	orientation, fixedOutput                      uint32
	color, duplex, yResolution, ttOption, collate int16
	formName                                      [32]uint16
	logPixels                                     uint16
	bitsPerPel                                    uint32
	pelsWidth, pelsHeight                         uint32
	displayFlags, frequency                       uint32
	icmMethod, icmIntent, mediaType, ditherType   uint32
	reserved1, reserved2                          uint32
	panningWidth, panningHeight                   uint32
}

type luid struct {
	low  uint32
	high int32
}

// dcPathInfo is DISPLAYCONFIG_PATH_INFO.
type dcPathInfo struct {
	srcAdapter                   luid
	srcID, srcModeIdx, srcFlags  uint32
	tgtAdapter                   luid
	tgtID, tgtModeIdx            uint32
	outputTech, rotation         uint32
	scaling, refreshN, refreshD  uint32
	scanLine, tgtAvail, tgtFlags uint32
	flags                        uint32
}

// dcModeInfo is DISPLAYCONFIG_MODE_INFO, only ever passed through.
type dcModeInfo [8]uint64

// dcHeader is DISPLAYCONFIG_DEVICE_INFO_HEADER.
type dcHeader struct {
	typ     int32
	size    uint32 // sizeof the whole request, header included
	adapter luid
	id      uint32 // source id for source requests
}

type dcSourceName struct {
	hdr     dcHeader
	gdiName [32]uint16
}

// dcDPIGet and dcDPISet are DISPLAYCONFIG_SOURCE_DPI_SCALE_GET/SET of
// github.com/lihas/windows-DPI-scaling-sample (DpiHelper.h): the 20-byte
// header plus three int32 (32 bytes) or one int32 (24 bytes).
type dcDPIGet struct {
	hdr                dcHeader
	minRel, curRel, mx int32
}

type dcDPISet struct {
	hdr dcHeader
	rel int32
}

// Compile-time layout checks against the Windows SDK / DpiHelper.h sizes.
var (
	_ [20]byte = [unsafe.Sizeof(dcHeader{})]byte{}
	_ [84]byte = [unsafe.Sizeof(dcSourceName{})]byte{}
	_ [32]byte = [unsafe.Sizeof(dcDPIGet{})]byte{}
	_ [24]byte = [unsafe.Sizeof(dcDPISet{})]byte{}
	_ [72]byte = [unsafe.Sizeof(dcPathInfo{})]byte{}
	_ [64]byte = [unsafe.Sizeof(dcModeInfo{})]byte{}
)

// dpiSteps are the scale percentages DisplayConfig's relative DPI indices
// walk through (github.com/lihas/windows-DPI-scaling-sample).
var dpiSteps = [...]int{100, 125, 150, 175, 200, 225, 250, 300, 350, 400, 450, 500}

// dpiStep is the index of the dpiSteps entry nearest unitsPerLogical*100%.
func dpiStep(unitsPerLogical float64) int {
	i, pct := 0, unitsPerLogical*100
	for j, s := range dpiSteps {
		if math.Abs(float64(s)-pct) < math.Abs(float64(dpiSteps[i])-pct) {
			i = j
		}
	}
	return i
}

type displayMonitor struct {
	rect    Rect
	dpi     uint32
	primary bool
	device  string // GDI source name, \\.\DISPLAYn
}

var (
	monMu      sync.Mutex
	monHandles []uintptr
	monProc    = windows.NewCallback(func(hmon, hdc, rc, lp uintptr) uintptr {
		monHandles = append(monHandles, hmon)
		return 1
	})
)

func displayMonitors() []displayMonitor {
	monMu.Lock()
	monHandles = monHandles[:0]
	procEnumDisplayMonitors.Call(0, 0, monProc, 0)
	hs := append([]uintptr(nil), monHandles...)
	monMu.Unlock()
	out := make([]displayMonitor, 0, len(hs))
	for _, h := range hs {
		mi := monitorInfoEx{monitorInfo: monitorInfo{size: uint32(unsafe.Sizeof(monitorInfoEx{}))}}
		if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r == 0 {
			continue
		}
		var dx, dy uint32 = 96, 96
		if procGetDpiForMonitor.Find() == nil {
			if hr, _, _ := procGetDpiForMonitor.Call(h, mdtEffectiveDPI,
				uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); hr != 0 || dx == 0 {
				dx = 96
			}
		}
		m := mi.monitor
		out = append(out, displayMonitor{
			rect:    Rect{X: m.Left, Y: m.Top, W: m.Right - m.Left, H: m.Bottom - m.Top},
			dpi:     dx,
			primary: mi.flags&monitorInfoFPrimary != 0,
			device:  windows.UTF16ToString(mi.device[:]),
		})
	}
	return out
}

func isParsec(s []uint16) bool {
	l := strings.ToLower(windows.UTF16ToString(s))
	return strings.Contains(l, "parsec") || strings.Contains(l, "psccdd")
}

func enumDisplayDevice(parent *uint16, i uint32, dd *displayDevice) bool {
	*dd = displayDevice{cb: uint32(unsafe.Sizeof(displayDevice{}))}
	r, _, _ := procEnumDisplayDevicesW.Call(uintptr(unsafe.Pointer(parent)), uintptr(i), uintptr(unsafe.Pointer(dd)), 0)
	return r != 0
}

// parsecSources maps the GDI source names belonging to the Parsec VDD to
// whether a Parsec monitor is currently plugged into that source. The
// driver's monitors report a device id containing "PSCCDD0"; its adapter
// is "Parsec Virtual Display Adapter".
func parsecSources() map[string]bool {
	set := make(map[string]bool)
	var ad, mon displayDevice
	for i := uint32(0); enumDisplayDevice(nil, i, &ad); i++ {
		name := windows.UTF16ToString(ad.name[:])
		adParsec := isParsec(ad.str[:]) || isParsec(ad.id[:])
		plugged := false
		for j := uint32(0); enumDisplayDevice(&ad.name[0], j, &mon); j++ {
			if isParsec(mon.id[:]) || isParsec(mon.str[:]) {
				plugged = true
			}
		}
		if adParsec || plugged {
			set[name] = plugged
		}
	}
	return set
}

func (p *winPlat) Monitors() []Monitor {
	parsec := parsecSources()
	var out []Monitor
	for _, m := range displayMonitors() {
		if _, virt := parsec[m.device]; virt {
			continue
		}
		out = append(out, Monitor{Rect: m.rect, UnitsPerLogical: float64(m.dpi) / 96, Scale: 1, Primary: m.primary})
	}
	return out
}

func (p *winPlat) Bounds() Rect {
	ms := p.Monitors()
	if len(ms) == 0 {
		return virtualScreen()
	}
	x0, y0 := ms[0].Rect.X, ms[0].Rect.Y
	x1, y1 := x0+ms[0].Rect.W, y0+ms[0].Rect.H
	for _, m := range ms[1:] {
		x0, y0 = min(x0, m.Rect.X), min(y0, m.Rect.Y)
		x1, y1 = max(x1, m.Rect.X+m.Rect.W), max(y1, m.Rect.Y+m.Rect.H)
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// monitorOf is the attached monitor of GDI source dev (rect and effective
// DPI), if it is on the desktop.
func monitorOf(dev string) (displayMonitor, bool) {
	for _, m := range displayMonitors() {
		if strings.EqualFold(m.device, dev) {
			return m, true
		}
	}
	return displayMonitor{}, false
}

// closestMode picks the source's listed mode nearest to w x h (exact when
// the driver offers it).
func closestMode(name *uint16, w, h int32) (uint32, uint32) {
	bw, bh, best := uint32(w), uint32(h), int64(math.MaxInt64)
	for i := uintptr(0); ; i++ {
		dm := devMode{}
		dm.size = uint16(unsafe.Sizeof(dm))
		if r, _, _ := procEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(name)), i, uintptr(unsafe.Pointer(&dm))); r == 0 {
			break
		}
		d := abs64(int64(dm.pelsWidth)-int64(w)) + abs64(int64(dm.pelsHeight)-int64(h))
		if d < best {
			bw, bh, best = dm.pelsWidth, dm.pelsHeight, d
		}
		if d == 0 {
			break
		}
	}
	return bw, bh
}

// hasMode reports whether source dev lists a w x h mode.
func hasMode(dev string, w, h int32) bool {
	name, err := windows.UTF16PtrFromString(dev)
	if err != nil {
		return false
	}
	bw, bh := closestMode(name, w, h)
	return int32(bw) == w && int32(bh) == h
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// placeDisplay positions source dev at r.X,r.Y and, with size, switches
// it to the mode closest to r.W x r.H, then applies the staged change.
func placeDisplay(dev string, r Rect, size bool) error {
	name, err := windows.UTF16PtrFromString(dev)
	if err != nil {
		return err
	}
	dm := devMode{}
	dm.size = uint16(unsafe.Sizeof(dm))
	// May fail for a source not yet on the desktop; the fields set below suffice.
	procEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(name)), enumCurrentSettings, uintptr(unsafe.Pointer(&dm)))
	dm.size = uint16(unsafe.Sizeof(dm))
	dm.driverExtra = 0
	dm.fields = dmPositionField
	dm.posX, dm.posY = r.X, r.Y
	if size {
		dm.pelsWidth, dm.pelsHeight = closestMode(name, r.W, r.H)
		dm.fields |= dmPelsWidthField | dmPelsHeightField
	}
	if rc, _, _ := procChangeDisplaySettingsExW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&dm)),
		0, cdsUpdateRegistry|cdsNoReset, 0); int32(rc) != 0 {
		return fmt.Errorf("plat: ChangeDisplaySettingsEx(%s): DISP_CHANGE %d", dev, int32(rc))
	}
	if rc, _, _ := procChangeDisplaySettingsExW.Call(0, 0, 0, 0, 0); int32(rc) != 0 {
		return fmt.Errorf("plat: ChangeDisplaySettingsEx(apply): DISP_CHANGE %d", int32(rc))
	}
	return nil
}

// displaySource finds the DisplayConfig adapter/source id of GDI source dev.
func displaySource(dev string) (luid, uint32, error) {
	var paths []dcPathInfo
	var modes []dcModeInfo
	for {
		var np, nm uint32
		if r, _, _ := procGetDisplayConfigBufferSizes.Call(qdcOnlyActivePaths,
			uintptr(unsafe.Pointer(&np)), uintptr(unsafe.Pointer(&nm))); r != 0 {
			return luid{}, 0, fmt.Errorf("plat: GetDisplayConfigBufferSizes: %d", r)
		}
		if np == 0 {
			return luid{}, 0, fmt.Errorf("plat: no active display paths")
		}
		paths, modes = make([]dcPathInfo, np), make([]dcModeInfo, max(nm, 1))
		r, _, _ := procQueryDisplayConfig.Call(qdcOnlyActivePaths,
			uintptr(unsafe.Pointer(&np)), uintptr(unsafe.Pointer(&paths[0])),
			uintptr(unsafe.Pointer(&nm)), uintptr(unsafe.Pointer(&modes[0])), 0)
		if r == errInsufficientBuf {
			continue // topology changed in between
		}
		if r != 0 {
			return luid{}, 0, fmt.Errorf("plat: QueryDisplayConfig: %d", r)
		}
		paths = paths[:np]
		break
	}
	for _, pi := range paths {
		sn := dcSourceName{hdr: dcHeader{typ: dcGetSourceName, size: uint32(unsafe.Sizeof(dcSourceName{})), adapter: pi.srcAdapter, id: pi.srcID}}
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&sn))); r != 0 {
			continue
		}
		if strings.EqualFold(windows.UTF16ToString(sn.gdiName[:]), dev) {
			return pi.srcAdapter, pi.srcID, nil
		}
	}
	return luid{}, 0, fmt.Errorf("plat: display path for %s not found", dev)
}

// setSourceDPI sets dev's scaling to the step nearest unitsPerLogical*100%
// through the undocumented DISPLAYCONFIG_DEVICE_INFO_{GET,SET}_DPI_SCALE
// (-3/-4) requests the Settings app uses. They address the DisplayConfig
// source (adapter + source id) and are relative to the display's
// recommended step, which sits at index -minRel of dpiSteps.
func setSourceDPI(dev string, unitsPerLogical float64) error {
	adapter, id, err := displaySource(dev)
	if err != nil {
		return err
	}
	get := dcDPIGet{hdr: dcHeader{typ: dcGetSourceDPIScale, size: uint32(unsafe.Sizeof(dcDPIGet{})), adapter: adapter, id: id}}
	if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&get))); r != 0 {
		return fmt.Errorf("plat: get DPI scale of %s: %d", dev, r)
	}
	if rec := -get.minRel; rec < 0 || int(rec) >= len(dpiSteps) {
		return fmt.Errorf("plat: DPI scale range of %s out of table: min %d max %d", dev, get.minRel, get.mx)
	}
	rel := min(max(int32(dpiStep(unitsPerLogical))+get.minRel, get.minRel), get.mx) // want - recommended
	if rel == get.curRel {
		return nil
	}
	set := dcDPISet{hdr: dcHeader{typ: dcSetSourceDPIScale, size: uint32(unsafe.Sizeof(dcDPISet{})), adapter: adapter, id: id}, rel: rel}
	if r, _, _ := procDisplayConfigSetDeviceInfo.Call(uintptr(unsafe.Pointer(&set))); r != 0 {
		return fmt.Errorf("plat: set DPI scale of %s: %d", dev, r)
	}
	return nil
}
