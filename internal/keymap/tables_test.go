package keymap

import "testing"

func TestRoundTrip(t *testing.T) {
	for _, r := range rows {
		if r.win != 0 {
			s, ok := HIDToWin(r.hid)
			if !ok || s != (WinScan{r.win, r.ext}) {
				t.Errorf("HIDToWin(%#x) = %+v, %v", r.hid, s, ok)
			}
			if h, ok := WinToHID(s); !ok || h != r.hid {
				t.Errorf("WinToHID(%+v) = %#x, %v; want %#x", s, h, ok, r.hid)
			}
		}
		if r.mac != noMac {
			c, ok := HIDToMac(r.hid)
			if !ok || c != r.mac {
				t.Errorf("HIDToMac(%#x) = %#x, %v", r.hid, c, ok)
			}
			if h, ok := MacToHID(c); !ok || h != r.hid {
				t.Errorf("MacToHID(%#x) = %#x, %v; want %#x", c, h, ok, r.hid)
			}
		}
	}
	if len(hidToWin) != len(winToHID) || len(hidToMac) != len(macToHID) {
		t.Errorf("map sizes differ: win %d/%d mac %d/%d", len(hidToWin), len(winToHID), len(hidToMac), len(macToHID))
	}
}

func TestEveryRowMapped(t *testing.T) {
	for _, r := range rows {
		if r.win == 0 && r.mac == noMac {
			t.Errorf("HID %#x maps to neither OS", r.hid)
		}
	}
}

func TestBuildRejectsDuplicates(t *testing.T) {
	cases := map[string][]row{
		"hid": {{0x04, 0x1E, false, 0x00}, {0x04, 0x30, false, 0x0B}},
		"win": {{0x04, 0x1E, false, 0x00}, {0x05, 0x1E, false, 0x0B}},
		"mac": {{0x04, 0x1E, false, 0x00}, {0x05, 0x30, false, 0x00}},
	}
	for name, rs := range cases {
		saved := [4]any{hidToWin, winToHID, hidToMac, macToHID}
		hidToWin, winToHID = map[uint16]WinScan{}, map[WinScan]uint16{}
		hidToMac, macToHID = map[uint16]uint16{}, map[uint16]uint16{}
		if err := build(rs); err == nil {
			t.Errorf("%s duplicate not rejected", name)
		}
		hidToWin, winToHID = saved[0].(map[uint16]WinScan), saved[1].(map[WinScan]uint16)
		hidToMac, macToHID = saved[2].(map[uint16]uint16), saved[3].(map[uint16]uint16)
	}
}

func TestKnownValues(t *testing.T) {
	win := []struct {
		hid uint16
		s   WinScan
	}{
		{0x04, WinScan{0x1E, false}},    // A
		{0x28, WinScan{0x1C, false}},    // Enter
		{0x58, WinScan{0x1C, true}},     // KP Enter
		{LeftGUI, WinScan{0x5B, true}},  // LWin
		{RightAlt, WinScan{0x38, true}}, // AltGr
		{LeftAlt, WinScan{0x38, false}},
		{0x52, WinScan{0x48, true}},  // Up
		{0x60, WinScan{0x48, false}}, // KP 8
		{0x53, WinScan{0x45, true}},  // NumLock
		{0x48, WinScan{0x45, false}}, // Pause
		{0x46, WinScan{0x37, true}},  // PrintScreen
		{0x64, WinScan{0x56, false}}, // ISO \
	}
	for _, c := range win {
		if s, ok := HIDToWin(c.hid); !ok || s != c.s {
			t.Errorf("HIDToWin(%#x) = %+v, %v; want %+v", c.hid, s, ok, c.s)
		}
	}
	mac := []struct{ hid, code uint16 }{
		{0x04, 0x00},     // kVK_ANSI_A
		{0x28, 0x24},     // kVK_Return
		{LeftGUI, 0x37},  // kVK_Command
		{RightGUI, 0x36}, // kVK_RightCommand
		{RightAlt, 0x3D}, // kVK_RightOption
		{0x52, 0x7E},     // kVK_UpArrow
		{0x35, 0x32},     // kVK_ANSI_Grave
		{0x64, 0x0A},     // kVK_ISO_Section
		{0x53, 0x47},     // kVK_ANSI_KeypadClear
		{0x39, 0x39},     // kVK_CapsLock
	}
	for _, c := range mac {
		if code, ok := HIDToMac(c.hid); !ok || code != c.code {
			t.Errorf("HIDToMac(%#x) = %#x, %v; want %#x", c.hid, code, ok, c.code)
		}
	}
	if _, ok := MacToHID(0x3F); ok {
		t.Error("kVK_Function must not map")
	}
	if _, ok := HIDToMac(0x46); ok {
		t.Error("PrintScreen must not map to macOS")
	}
}
