// Package keymap converts between USB HID keyboard usages (page 0x07, the
// wire format) and native key codes: Windows set-1 scan codes and macOS
// virtual key codes (kVK_*).
package keymap

// Modifier usages.
const (
	LeftCtrl   uint16 = 0xE0
	LeftShift  uint16 = 0xE1
	LeftAlt    uint16 = 0xE2 // Option on macOS
	LeftGUI    uint16 = 0xE3 // Win / Cmd
	RightCtrl  uint16 = 0xE4
	RightShift uint16 = 0xE5
	RightAlt   uint16 = 0xE6
	RightGUI   uint16 = 0xE7
)

// WinScan is a set-1 scan code; Extended marks the E0 prefix
// (KEYEVENTF_EXTENDEDKEY / LLKHF_EXTENDED).
type WinScan struct {
	Code     uint16
	Extended bool
}

func HIDToWin(hid uint16) (WinScan, bool) { s, ok := hidToWin[hid]; return s, ok }
func WinToHID(s WinScan) (uint16, bool)   { h, ok := winToHID[s]; return h, ok }
func HIDToMac(hid uint16) (uint16, bool)  { c, ok := hidToMac[hid]; return c, ok }
func MacToHID(code uint16) (uint16, bool) { h, ok := macToHID[code]; return h, ok }

// SwapCmdCtrl swaps Ctrl and GUI on each side so muscle memory carries over
// between macOS (Cmd+C) and Windows (Ctrl+C). Other usages pass through.
func SwapCmdCtrl(hid uint16) uint16 {
	switch hid {
	case LeftCtrl:
		return LeftGUI
	case LeftGUI:
		return LeftCtrl
	case RightCtrl:
		return RightGUI
	case RightGUI:
		return RightCtrl
	}
	return hid
}
