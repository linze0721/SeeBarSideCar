package plat

import (
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"time"

	"github.com/ebitengine/purego/objc"
)

// Private CoreGraphics virtual displays (CGVirtualDisplay*, the API behind
// Sidecar/AirPlay displays; also used by Chromium's display tests). The
// display lives exactly as long as the CGVirtualDisplay object, and the
// WindowServer drops it when our connection goes away, so a crash never
// leaves it behind.
var (
	selSetQueue             = objc.RegisterName("setQueue:")
	selSetName              = objc.RegisterName("setName:")
	selSetMaxPixelsWide     = objc.RegisterName("setMaxPixelsWide:")
	selSetMaxPixelsHigh     = objc.RegisterName("setMaxPixelsHigh:")
	selSetSizeInMillimeters = objc.RegisterName("setSizeInMillimeters:")
	selSetProductID         = objc.RegisterName("setProductID:")
	selSetVendorID          = objc.RegisterName("setVendorID:")
	selSetSerialNum         = objc.RegisterName("setSerialNum:")
	selSetRedPrimary        = objc.RegisterName("setRedPrimary:")
	selSetGreenPrimary      = objc.RegisterName("setGreenPrimary:")
	selSetBluePrimary       = objc.RegisterName("setBluePrimary:")
	selSetWhitePoint        = objc.RegisterName("setWhitePoint:")
	selSetTermination       = objc.RegisterName("setTerminationHandler:")
	selInitWithDescriptor   = objc.RegisterName("initWithDescriptor:")
	selInit                 = objc.RegisterName("init")
	selSetHiDPI             = objc.RegisterName("setHiDPI:")
	selInitWithWidthHeight  = objc.RegisterName("initWithWidth:height:refreshRate:")
	selSetModes             = objc.RegisterName("setModes:")
	selApplySettings        = objc.RegisterName("applySettings:")
	selDisplayID            = objc.RegisterName("displayID")
)

const (
	vdisplayWait    = 2 * time.Second // for the WindowServer to bring it online
	vdisplayPoll    = 20 * time.Millisecond
	vdisplayMMPerPt = 25.4 / 100 // report ~100 dpi logical; only cosmetic
)

func (p *macPlat) Monitors() []Monitor {
	var ids [32]uint32
	var n uint32
	// Online, not active: a sleeping display drops out of the active list,
	// and the peer's virtual display must not vanish while ours is asleep.
	if cgGetOnlineDisplayList(uint32(len(ids)), &ids[0], &n) != 0 {
		return nil
	}
	virt := p.vdID.Load()
	var out []Monitor
	for _, id := range ids[:n] {
		// Hardware-mirrored members report the same bounds as their master.
		if id == virt || cgDisplayMirrorsDisplay(id) != 0 {
			continue
		}
		r := cgDisplayBounds(id)
		m := Monitor{
			Rect: Rect{
				X: int32(math.Round(r.X)), Y: int32(math.Round(r.Y)),
				W: int32(math.Round(r.W)), H: int32(math.Round(r.H)),
			},
			UnitsPerLogical: 1,
			Scale:           1,
			Primary:         cgDisplayIsMain(id) != 0,
		}
		if mode := cgDisplayCopyDisplayMode(id); mode != 0 {
			if px := cgDisplayModeGetPixelWidth(mode); px > 0 && r.W > 0 {
				m.Scale = float64(px) / r.W
			}
			cgDisplayModeRelease(mode)
		}
		out = append(out, m)
	}
	return out
}

func (p *macPlat) Bounds() Rect {
	mons := p.Monitors()
	if len(mons) == 0 {
		return Rect{}
	}
	minX, minY := int32(math.MaxInt32), int32(math.MaxInt32)
	maxX, maxY := int32(math.MinInt32), int32(math.MinInt32)
	for _, m := range mons {
		minX, minY = min(minX, m.Rect.X), min(minY, m.Rect.Y)
		maxX, maxY = max(maxX, m.Rect.X+m.Rect.W), max(maxY, m.Rect.Y+m.Rect.H)
	}
	return Rect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
}

// SetVirtualDisplay keeps one HiDPI virtual display of v.Rect.W x H points.
// A size change recreates it; a move only reconfigures its origin.
func (p *macPlat) SetVirtualDisplay(v *VirtualDisplay) (Rect, error) {
	p.vdMu.Lock()
	defer p.vdMu.Unlock()
	if v == nil || v.Rect.W <= 0 || v.Rect.H <= 0 {
		p.dropVirtual()
		if v != nil {
			return Rect{}, fmt.Errorf("plat: virtual display size %dx%d", v.Rect.W, v.Rect.H)
		}
		return Rect{}, nil
	}
	if p.vd == 0 || p.vdW != v.Rect.W || p.vdH != v.Rect.H {
		p.dropVirtual()
		if err := p.createVirtual(v.Rect.W, v.Rect.H); err != nil {
			return Rect{}, err
		}
	}
	id := p.vdID.Load()
	if err := placeDisplay(id, v.Rect.X, v.Rect.Y); err != nil {
		return Rect{}, err
	}
	// The origin change lands asynchronously; the WindowServer may also
	// snap it to keep the displays touching, so report what it settled on.
	deadline := time.Now().Add(vdisplayWait / 4)
	r := cgDisplayBounds(id)
	for (int32(r.X) != v.Rect.X || int32(r.Y) != v.Rect.Y) && time.Now().Before(deadline) {
		time.Sleep(vdisplayPoll)
		r = cgDisplayBounds(id)
	}
	return Rect{
		X: int32(math.Round(r.X)), Y: int32(math.Round(r.Y)),
		W: int32(math.Round(r.W)), H: int32(math.Round(r.H)),
	}, nil
}

// createVirtual makes the display and waits until it is online. Caller
// holds vdMu.
func (p *macPlat) createVirtual(w, h int32) error {
	if objc.GetClass("CGVirtualDisplay") == 0 || objc.GetClass("CGVirtualDisplayDescriptor") == 0 ||
		objc.GetClass("CGVirtualDisplaySettings") == 0 || objc.GetClass("CGVirtualDisplayMode") == 0 {
		return errors.New("plat: CGVirtualDisplay API unavailable")
	}
	var err error
	onMainSync(func() {
		desc := class("CGVirtualDisplayDescriptor").Send(selAlloc).Send(selInit)
		if desc == 0 {
			err = errors.New("plat: creating CGVirtualDisplayDescriptor failed")
			return
		}
		defer desc.Send(selRelease)
		desc.Send(selSetQueue, objc.ID(dispatchMainQ))
		desc.Send(selSetName, nsString("magic"))
		desc.Send(selSetMaxPixelsWide, uint32(2*w))
		desc.Send(selSetMaxPixelsHigh, uint32(2*h))
		desc.Send(selSetSizeInMillimeters, cgSize{W: float64(w) * vdisplayMMPerPt, H: float64(h) * vdisplayMMPerPt})
		desc.Send(selSetProductID, uint32(0x1234))
		desc.Send(selSetVendorID, uint32(0x3456))
		desc.Send(selSetSerialNum, uint32(1))
		// sRGB/D65; left at (0,0) macOS infers a colour space from
		// invalid chromaticities.
		desc.Send(selSetRedPrimary, cgPoint{X: 0.64, Y: 0.33})
		desc.Send(selSetGreenPrimary, cgPoint{X: 0.30, Y: 0.60})
		desc.Send(selSetBluePrimary, cgPoint{X: 0.15, Y: 0.06})
		desc.Send(selSetWhitePoint, cgPoint{X: 0.3127, Y: 0.3290})
		term := objc.NewBlock(func(_ objc.Block, _, _ objc.ID) {
			log.Printf("plat: virtual display terminated by the system")
		})
		desc.Send(selSetTermination, term) // copy property
		term.Release()

		disp := class("CGVirtualDisplay").Send(selAlloc).Send(selInitWithDescriptor, desc)
		if disp == 0 {
			err = errors.New("plat: creating CGVirtualDisplay failed")
			return
		}
		settings := class("CGVirtualDisplaySettings").Send(selAlloc).Send(selInit)
		defer settings.Send(selRelease)
		// With hiDPI the mode is in points and the framebuffer is 2x.
		settings.Send(selSetHiDPI, uint32(1))
		mode := class("CGVirtualDisplayMode").Send(selAlloc).Send(selInitWithWidthHeight, uint32(w), uint32(h), float64(60))
		settings.Send(selSetModes, class("NSArray").Send(selArrayWithObject, mode))
		mode.Send(selRelease)
		if !objc.Send[bool](disp, selApplySettings, settings) {
			disp.Send(selRelease)
			err = errors.New("plat: CGVirtualDisplay applySettings failed")
			return
		}
		p.vd, p.vdW, p.vdH = disp, w, h
		p.vdID.Store(objc.Send[uint32](disp, selDisplayID))
	})
	if err != nil {
		return err
	}
	id := p.vdID.Load()
	deadline := time.Now().Add(vdisplayWait)
	for !displayActive(id) {
		if time.Now().After(deadline) {
			p.dropVirtual()
			return fmt.Errorf("plat: virtual display %d did not come online", id)
		}
		time.Sleep(vdisplayPoll)
	}
	return nil
}

// dropVirtual releases the display (it disappears and macOS moves its
// windows to a real one). Caller holds vdMu.
func (p *macPlat) dropVirtual() {
	if p.vd == 0 {
		return
	}
	disp := p.vd
	p.vd, p.vdW, p.vdH = 0, 0, 0
	p.vdID.Store(0)
	onMainSync(func() { disp.Send(selRelease) })
}

func displayActive(id uint32) bool {
	var ids [32]uint32
	var n uint32
	if cgGetActiveDisplayList(uint32(len(ids)), &ids[0], &n) != 0 {
		return false
	}
	return slices.Contains(ids[:n], id)
}

// placeDisplay moves the display's top-left to (x, y) global points for
// this login session, keeping it an extended (not mirrored) display.
func placeDisplay(id uint32, x, y int32) error {
	var cfg uintptr
	if e := cgBeginDisplayConfiguration(&cfg); e != 0 {
		return fmt.Errorf("plat: CGBeginDisplayConfiguration: %d", e)
	}
	if e := cgConfigureDisplayMirrorOfDisplay(cfg, id, 0); e != 0 {
		cgCancelDisplayConfiguration(cfg)
		return fmt.Errorf("plat: CGConfigureDisplayMirrorOfDisplay: %d", e)
	}
	if e := cgConfigureDisplayOrigin(cfg, id, x, y); e != 0 {
		cgCancelDisplayConfiguration(cfg)
		return fmt.Errorf("plat: CGConfigureDisplayOrigin: %d", e)
	}
	if e := cgCompleteDisplayConfiguration(cfg, 1); e != 0 { // kCGConfigureForSession
		return fmt.Errorf("plat: CGCompleteDisplayConfiguration: %d", e)
	}
	return nil
}
