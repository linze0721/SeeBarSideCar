package plat

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Parsec Virtual Display Driver control, after github.com/nomi-san/parsec-vdd
// (core/parsec-vdd.h, app/Vdd/Core.cs, app/Vdd/Utils.cs). Displays exist only
// while some handle keeps sending VDD_IOCTL_UPDATE; the driver unplugs them
// about a second after the pings stop, which is what removes ours when the
// process dies.
const (
	vddIoctlAdd    = 0x0022e004
	vddIoctlRemove = 0x0022a008
	vddIoctlUpdate = 0x0022a00c

	vddPing      = 100 * time.Millisecond // the reference apps' interval
	vddAppear    = 5 * time.Second
	vddDetach    = 2 * time.Second         // bound on waiting for a removed display to leave the desktop
	vddReplug    = 1 * time.Second         // before the one retry of a failed add
	vddDPISettle = 300 * time.Millisecond  // before re-checking the effective DPI
	vddModeKey   = `SOFTWARE\Parsec\vdd\0` // custom mode slot 0 (slots 0..4)
	vddModeHz    = 60
	waitObject0  = 0
	vddIoTimeout = 5000 // ms, as parsec-vdd.h
)

// {00b41627-04c4-429e-a26e-0265cf50c8fa}
var vddAdapterGUID = windows.GUID{Data1: 0x00b41627, Data2: 0x04c4, Data3: 0x429e,
	Data4: [8]byte{0xa2, 0x6e, 0x02, 0x65, 0xcf, 0x50, 0xc8, 0xfa}}

var errNoVDD = errors.New("plat: Parsec VDD not installed")

// vdd is the state behind SetVirtualDisplay; guarded by winPlat.vddMu.
type vdd struct {
	h      windows.Handle
	index  int    // driver display index, -1 when none
	device string // GDI source name of our display
	want   VirtualDisplay
	rect   Rect
	stop   chan struct{}
	done   chan struct{}
}

func openVDD() (windows.Handle, error) {
	paths, err := windows.CM_Get_Device_Interface_List("", &vddAdapterGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil || len(paths) == 0 {
		return windows.InvalidHandle, errNoVDD
	}
	var last error
	for _, path := range paths {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_WRITE_THROUGH, 0)
		if err == nil {
			return h, nil
		}
		last = err
	}
	return windows.InvalidHandle, fmt.Errorf("plat: open Parsec VDD: %w", last)
}

// vddIoctl issues code with a 32-byte input buffer and returns the DWORD
// the driver writes back.
func vddIoctl(h windows.Handle, code uint32, in []byte) (uint32, error) {
	// The driver completes asynchronously into these, so they must not live
	// on a goroutine stack that can move; Pin also forces heap allocation.
	req := &struct {
		ov  windows.Overlapped
		in  [32]byte
		out uint32
	}{}
	var pin runtime.Pinner
	pin.Pin(req)
	defer pin.Unpin()
	copy(req.in[:], in)
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)
	req.ov.HEvent = ev
	err = windows.DeviceIoControl(h, code, &req.in[0], uint32(len(req.in)),
		(*byte)(unsafe.Pointer(&req.out)), uint32(unsafe.Sizeof(req.out)), nil, &req.ov)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, fmt.Errorf("plat: VDD ioctl %#x: %w", code, err)
	}
	var n uint32
	if w, _ := windows.WaitForSingleObject(ev, vddIoTimeout); w != waitObject0 {
		windows.CancelIoEx(h, &req.ov)
		windows.GetOverlappedResult(h, &req.ov, &n, true) // the buffers must outlive the I/O
		return 0, fmt.Errorf("plat: VDD ioctl %#x timed out", code)
	}
	if err := windows.GetOverlappedResult(h, &req.ov, &n, false); err != nil {
		return 0, fmt.Errorf("plat: VDD ioctl %#x: %w", code, err)
	}
	return req.out, nil
}

// setVDDMode makes w x h the driver's custom mode in slot 0, read when a
// display is plugged.
func setVDDMode(w, h int32) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, vddModeKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("plat: open HKLM\\%s: %w", vddModeKey, err)
	}
	defer k.Close()
	for _, v := range []struct {
		name string
		val  uint32
	}{{"width", uint32(w)}, {"height", uint32(h)}, {"hz", vddModeHz}} {
		if err := k.SetDWordValue(v.name, v.val); err != nil {
			return fmt.Errorf("plat: set HKLM\\%s\\%s: %w", vddModeKey, v.name, err)
		}
	}
	return nil
}

func (p *winPlat) SetVirtualDisplay(v *VirtualDisplay) (Rect, error) {
	p.vddMu.Lock()
	defer p.vddMu.Unlock()
	d := &p.vdd
	if v == nil {
		d.remove()
		return Rect{}, nil
	}
	if d.index >= 0 {
		_, attached := monitorOf(d.device)
		if attached && v.Rect.W == d.want.Rect.W && v.Rect.H == d.want.Rect.H && v.UnitsPerLogical == d.want.UnitsPerLogical {
			if v.Rect.X != d.want.Rect.X || v.Rect.Y != d.want.Rect.Y {
				if err := placeDisplay(d.device, v.Rect, false); err != nil {
					return d.rect, err
				}
			}
			d.want = *v
			if m, ok := monitorOf(d.device); ok {
				d.rect = m.rect
			}
			return d.rect, nil
		}
		// Resized, rescaled, or detached behind our back (Settings, driver reset).
		d.remove()
	}
	return d.add(*v)
}

// add plugs a display for v and makes its logical size v's: at v's DPI
// scaling when Windows accepts it, otherwise at the DPI Windows applied
// with the pixel size scaled to match.
func (d *vdd) add(v VirtualDisplay) (Rect, error) {
	if err := d.plug(v.Rect); err != nil {
		return Rect{}, err
	}
	want := uint32(math.Round(v.UnitsPerLogical * 96))
	// A failed DPI set only shows up as the wrong effective DPI below, which
	// the resize compensates, so its error is not reported. The scaling
	// change re-enumerates the monitor, so a missing one is re-read too.
	setSourceDPI(d.device, v.UnitsPerLogical)
	m, ok := monitorOf(d.device)
	if !ok || m.dpi != want {
		time.Sleep(vddDPISettle)
		setSourceDPI(d.device, v.UnitsPerLogical)
		time.Sleep(vddDPISettle)
		m, ok = monitorOf(d.device)
	}
	if ok && m.dpi != want && m.dpi > 0 && v.UnitsPerLogical > 0 {
		f := float64(m.dpi) / 96 / v.UnitsPerLogical
		r := Rect{X: v.Rect.X, Y: v.Rect.Y,
			W: int32(math.Round(float64(v.Rect.W) * f)), H: int32(math.Round(float64(v.Rect.H) * f))}
		if err := d.resize(r); err != nil {
			d.remove()
			return Rect{}, err
		}
		m, ok = monitorOf(d.device)
	}
	if !ok {
		dev := d.device
		d.remove()
		return Rect{}, fmt.Errorf("plat: virtual display %s did not attach to the desktop", dev)
	}
	d.want, d.rect = v, m.rect
	return m.rect, nil
}

// plug adds a driver display with r's size as its custom mode and places
// it at r, retrying once when the display does not come up.
func (d *vdd) plug(r Rect) error {
	err := d.plugOnce(r)
	if err == nil || errors.Is(err, errNoVDD) {
		return err
	}
	time.Sleep(vddReplug)
	return d.plugOnce(r)
}

func (d *vdd) plugOnce(r Rect) error {
	if d.h == 0 || d.h == windows.InvalidHandle {
		h, err := openVDD()
		if err != nil {
			return err
		}
		d.h = h
	}
	if err := setVDDMode(r.W, r.H); err != nil {
		return err
	}
	before := attachedParsec()
	idx, err := vddIoctl(d.h, vddIoctlAdd, nil)
	if err != nil {
		return err
	}
	d.index = int(idx)
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	go keepAlive(d.h, d.stop, d.done)

	dev, err := waitNewSource(before)
	if err != nil {
		d.remove()
		return err
	}
	d.device = dev
	if err := placeDisplay(dev, r, true); err != nil {
		d.remove()
		return err
	}
	return nil
}

// resize switches our display to r's size at r's origin. The driver reads
// its custom mode only when a display is plugged, so a size it does not
// list needs a fresh display.
func (d *vdd) resize(r Rect) error {
	if hasMode(d.device, r.W, r.H) {
		return placeDisplay(d.device, r, true)
	}
	d.remove()
	return d.plug(r)
}

// attachedParsec is the set of Parsec GDI sources currently on the desktop.
// Each of the driver's sources reports the plugged monitor, so only being
// attached identifies the one Windows actually extended the desktop to.
func attachedParsec() map[string]bool {
	parsec := parsecSources()
	set := make(map[string]bool)
	for _, m := range displayMonitors() {
		if _, ok := parsec[m.device]; ok {
			set[m.device] = true
		}
	}
	return set
}

// waitNewSource polls until the Parsec source of a just-added display is
// attached to the desktop and returns its GDI name. That is a source not in
// before, or one in before that detached and came back: a display removed
// just before (ours from an earlier add, or one left by a previous process)
// can still be attached when the new one is added, and the new one may
// reuse its source. Past half the timeout, a single attached Parsec source
// is taken as ours.
func waitNewSource(before map[string]bool) (string, error) {
	start := time.Now()
	gone := make(map[string]bool)
	for {
		now := attachedParsec()
		for name := range now {
			if !before[name] || gone[name] {
				return name, nil
			}
		}
		for name := range before {
			if !now[name] {
				gone[name] = true
			}
		}
		if len(now) == 1 && time.Since(start) > vddAppear/2 {
			for name := range now {
				return name, nil
			}
		}
		if time.Since(start) > vddAppear {
			return "", errors.New("plat: Parsec virtual display did not attach to the desktop")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// keepAlive pings the driver until stop; the driver drops the display
// shortly after the pings end (also when this process dies).
func keepAlive(h windows.Handle, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(vddPing)
	defer t.Stop()
	for {
		vddIoctl(h, vddIoctlUpdate, nil)
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// remove unplugs our display and waits (bounded) for Windows to detach its
// source, so an add right after does not find it still on the desktop.
func (d *vdd) remove() {
	if d.index < 0 {
		return
	}
	close(d.stop)
	<-d.done
	// 16-bit big-endian index, as parsec-vdd.h's VddRemoveDisplay.
	in := []byte{byte(d.index >> 8), byte(d.index)}
	vddIoctl(d.h, vddIoctlRemove, in)
	vddIoctl(d.h, vddIoctlUpdate, nil)
	if d.device != "" {
		for deadline := time.Now().Add(vddDetach); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if !attachedParsec()[d.device] {
				break
			}
		}
	}
	d.index, d.device, d.rect, d.want = -1, "", Rect{}, VirtualDisplay{}
}
