// Package plat is the OS boundary. Each supported OS provides New and Run
// (plat_windows.go, plat_darwin.go); every other OS gets plat_other.go,
// which returns ErrUnsupported so pure-Go logic stays testable on Linux.
package plat

import (
	"errors"
	"image"
)

var ErrUnsupported = errors.New("plat: unsupported OS")

type EventKind uint8

const (
	MouseMove EventKind = iota + 1
	MouseButton
	Wheel
	Key
)

// Mouse buttons.
const (
	ButtonLeft uint8 = iota + 1
	ButtonRight
	ButtonMiddle
	ButtonBack
	ButtonForward
)

// WheelNotch is one detent of a classic wheel, in Event.DX/DY wheel units.
const WheelNotch = 120

// Event is the OS-neutral input event used both for capture and injection.
//
//   - MouseMove: X,Y absolute global coordinates (logical points on macOS,
//     physical pixels on Windows). In grab mode Capture fills DX,DY with the
//     relative motion and X,Y are meaningless. Inject uses X,Y only.
//   - MouseButton: Button, Down. Inject presses at the current cursor position.
//   - Wheel: DX,DY in WheelNotch units, device-relative: positive DY = wheel
//     rolled away from the user (classic "scroll up"), positive DX = right.
//     Each platform removes its natural-scrolling inversion on Capture and
//     applies its own on Inject, so each machine keeps its own preference.
//   - Key: HID is a USB HID usage ID on page 0x07 (see internal/keymap), Down.
type Event struct {
	Kind   EventKind
	X, Y   int32
	DX, DY int32
	Button uint8
	Down   bool
	HID    uint16
}

type Rect struct{ X, Y, W, H int32 }

func (r Rect) Contains(x, y int32) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

type WindowInfo struct {
	ID     uint64 // stable for the window's lifetime (CGWindowID / HWND)
	PID    uint32
	App    string // process / application name
	Title  string
	Bounds Rect // global coordinates, same space as Event.X/Y
}

// Monitor is one display in global coordinates. UnitsPerLogical converts
// global units to resolution-independent logical units: on Windows global
// units are physical pixels, so it is DPI/96; on macOS they are points, so
// it is 1. Scale is the backing pixels per global unit (macOS HiDPI 2,
// Windows 1).
type Monitor struct {
	Rect            Rect
	UnitsPerLogical float64
	Scale           float64
	Primary         bool
}

// VirtualDisplay asks for an extra local display at Rect (global
// coordinates, adjacent to a real monitor). UnitsPerLogical as in Monitor:
// Windows applies it as the display's DPI scaling (Rect.W/H are physical
// pixels); macOS creates a HiDPI display of Rect.W x Rect.H points.
type VirtualDisplay struct {
	Rect            Rect
	UnitsPerLogical float64
}

// Platform is created by New and is safe for concurrent use unless a
// method says otherwise.
type Platform interface {
	OS() string // "windows" | "darwin"

	// Bounds is the bounding box of the real displays in global coordinates
	// (the virtual display is excluded).
	Bounds() Rect
	// Monitors lists the real displays (the virtual display is excluded).
	Monitors() []Monitor
	CursorPos() (x, y int32)

	// SetVirtualDisplay creates, moves/resizes (by recreating if needed) or
	// with nil removes the virtual display. It returns the rect actually
	// applied in global coordinates (the OS may snap position/size). The
	// display is removed automatically when the process exits, even on a
	// crash, so windows the user dragged onto it return to a real monitor.
	SetVirtualDisplay(v *VirtualDisplay) (Rect, error)

	// Capture installs global input hooks on its own OS thread and returns
	// once they are installed (or the install error); a second call fails.
	// fn is then called for every local input event until the process
	// exits. fn runs on the hook thread and must return quickly. Events
	// produced by Inject are never delivered.
	Capture(fn func(Event)) error
	// SetGrab(true): swallow all local input (it must not reach local apps),
	// hide the cursor and park it so motion keeps producing relative
	// MouseMove DX/DY (computed against the cursor's actual position, which
	// Inject may change while grabbed). SetGrab(false) restores the cursor
	// and normal input.
	SetGrab(on bool)

	// Inject synthesizes an input event as if from local hardware.
	Inject(ev Event) error

	// ClipboardSeq returns a counter that changes whenever the clipboard
	// content changes (any format); cheap enough to poll.
	ClipboardSeq() uint64
	// ClipboardText returns the current text and the same seq as
	// ClipboardSeq.
	ClipboardText() (text string, seq uint64, err error)
	// SetClipboardText replaces the clipboard and returns the new seq.
	SetClipboardText(text string) (seq uint64, err error)
	// ClipboardFiles returns the absolute paths of files/directories on the
	// clipboard (empty if it holds none) and the same seq as ClipboardText.
	ClipboardFiles() (paths []string, seq uint64, err error)
	// SetClipboardFiles puts paths on the clipboard as a file copy (pasteable
	// in Explorer/Finder) and returns the new seq.
	SetClipboardFiles(paths []string) (seq uint64, err error)
	// ClipboardImage returns the clipboard's bitmap image as PNG (nil if it
	// holds none) and the same seq as ClipboardText. Converting may be
	// costly; callers read it only after the seq changed.
	ClipboardImage() (png []byte, seq uint64, err error)
	// SetClipboardImage replaces the clipboard with the PNG image, offered
	// in the formats native apps paste, plus text if non-empty (one
	// clipboard change), and returns the new seq.
	SetClipboardImage(png []byte, text string) (seq uint64, err error)

	// ListWindows returns visible, titled, normal-layer top-level windows,
	// front to back, excluding this process's own. App is the exe name
	// ("chrome.exe") on Windows and the application name on macOS. Windows
	// on the virtual display are included.
	ListWindows() ([]WindowInfo, error)
	// CaptureWindow returns a freshly allocated image (caller keeps it) of
	// the window's current pixels (backing-store resolution, may exceed
	// Bounds on HiDPI) and its current bounds. Must work while the window
	// sits (partly) on the virtual display.
	CaptureWindow(id uint64) (*image.RGBA, Rect, error)
	// FocusWindow brings the window's app to front and the window on top.
	FocusWindow(id uint64) error

	// NewOverlay opens a borderless, shadowed top-level window at rect
	// (global coordinates) showing a peer window's pixels scaled to fill it,
	// so it looks like that app's own window. It appears in the taskbar /
	// Dock / Alt-Tab / Cmd-Tab under title. input receives MouseMove (X,Y
	// in global coordinates), MouseButton, Wheel and Key events while the
	// overlay is hovered/focused. closed is called once when the user closes
	// it through the OS (taskbar/Dock; keys like Alt+F4/Cmd+W are forwarded
	// as input, not a close); never after a programmatic Overlay.Close.
	NewOverlay(title string, rect Rect, input func(Event), closed func()) (Overlay, error)
}

type Overlay interface {
	// SetFrame displays img (copied before return), scaled to the overlay.
	SetFrame(img *image.RGBA)
	// SetBounds moves/resizes the overlay to rect (global coordinates).
	SetBounds(rect Rect)
	SetTitle(title string)
	Close()
}
