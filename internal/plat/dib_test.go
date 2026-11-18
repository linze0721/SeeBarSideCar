package plat

import (
	"bytes"
	"encoding/binary"
	"image"
	"testing"
)

func TestDIBRoundTrip(t *testing.T) {
	// Use nonzero bounds and include both partial and full transparency.
	original := &image.NRGBA{
		Pix:    []byte{10, 20, 30, 255, 40, 50, 60, 127, 70, 80, 90, 0, 100, 110, 120, 255},
		Stride: 8,
		Rect:   image.Rect(5, 6, 7, 8),
	}
	data, err := encodeDIB(original)
	if err != nil {
		t.Fatal(err)
	}
	// Check the wire layout independently of the decoder: 40-byte header,
	// positive height, BI_RGB, and bottom row first with straight alpha.
	want := []byte{
		40, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0, 1, 0, 32, 0,
		0, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		90, 80, 70, 0, 120, 110, 100, 255,
		30, 20, 10, 255, 60, 50, 40, 127,
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("DIB = %v, want %v", data, want)
	}
	got, err := decodeDIB(data)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Bounds() != image.Rect(0, 0, 2, 2) || !bytes.Equal(got.Pix, original.Pix) {
		t.Fatalf("decodeDIB = %+v, want pixels %v", got, original.Pix)
	}
}

func TestDecodeDIB(t *testing.T) {
	for _, tt := range []struct {
		name          string
		header        uint32
		width, height int32
		depth         uint16
		masks         []uint32
		palette       []byte
		pixels, want  []byte
	}{
		{
			name: "24-bit padded bottom-up", header: 40, width: 1, height: 2, depth: 24,
			pixels: []byte{60, 50, 40, 0xCC, 30, 20, 10, 0xDD},
			want:   []byte{10, 20, 30, 255, 40, 50, 60, 255},
		},
		{
			name: "32-bit top-down", header: 40, width: 1, height: -2, depth: 32,
			pixels: []byte{30, 20, 10, 255, 60, 50, 40, 127},
			want:   []byte{10, 20, 30, 255, 40, 50, 60, 127},
		},
		{
			name: "all-zero alpha", header: 40, width: 2, height: 1, depth: 32,
			pixels: []byte{30, 20, 10, 0, 60, 50, 40, 0},
			want:   []byte{10, 20, 30, 255, 40, 50, 60, 255},
		},
		{
			name: "24-bit trailing bitfields and color table", header: 40, width: 1, height: 1, depth: 24,
			masks:   []uint32{0x0000ff, 0x00ff00, 0xff0000},
			palette: []byte{0xaa, 0xbb, 0xcc, 0},
			pixels:  []byte{10, 20, 30, 0xDD},
			want:    []byte{10, 20, 30, 255},
		},
		{
			name: "32-bit V5 10:10:10:2", header: 124, width: 2, height: -1, depth: 32,
			masks:  []uint32{0x000003ff, 0x000ffc00, 0x3ff00000, 0xc0000000},
			pixels: []byte{0xff, 0x03, 0, 0x60, 0, 0xfc, 0x0f, 0},
			want:   []byte{255, 0, 128, 85, 0, 255, 0, 0},
		},
		{
			name: "16-bit unsupported", header: 40, width: 1, height: 1, depth: 16,
			pixels: []byte{0, 0, 0, 0},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			le := binary.LittleEndian
			data := make([]byte, tt.header)
			le.PutUint32(data[0:4], tt.header)
			le.PutUint32(data[4:8], uint32(tt.width))
			le.PutUint32(data[8:12], uint32(tt.height))
			le.PutUint16(data[12:14], 1)
			le.PutUint16(data[14:16], tt.depth)
			le.PutUint32(data[32:36], uint32(len(tt.palette)/4))
			if len(tt.masks) != 0 {
				le.PutUint32(data[16:20], 3) // BI_BITFIELDS
				if tt.header == 40 {
					for _, m := range tt.masks {
						data = le.AppendUint32(data, m)
					}
				} else {
					for i, m := range tt.masks {
						le.PutUint32(data[40+i*4:], m)
					}
				}
			}
			data = append(data, tt.palette...)
			data = append(data, tt.pixels...)
			got, err := decodeDIB(data)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("unsupported depth decoded as %+v", got)
				}
				return
			}
			h := max(tt.height, -tt.height)
			if got == nil || got.Bounds() != image.Rect(0, 0, int(tt.width), int(h)) || !bytes.Equal(got.Pix, tt.want) {
				t.Fatalf("decodeDIB = %+v, want pixels %v", got, tt.want)
			}
		})
	}
}

func TestDecodeDIBTruncatedPixels(t *testing.T) {
	data := []byte{
		40, 0, 0, 0, 255, 255, 255, 127, 0, 0, 0, 128, 1, 0, 32, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	if got, err := decodeDIB(data); err == nil || got != nil {
		t.Fatalf("decodeDIB returned (%v, %v), want truncated pixels error", got, err)
	}
}
