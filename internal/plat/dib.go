package plat

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"math"
	"math/bits"
)

const (
	dibInfoHeaderSize = 40
	dibBIRGB          = 0
	dibBIBitfields    = 3
)

// decodeDIB reads a packed DIB (no BMP file header). Unsupported depths and
// compression methods return (nil, nil).
func decodeDIB(data []byte) (*image.NRGBA, error) {
	le := binary.LittleEndian
	if len(data) < dibInfoHeaderSize {
		return nil, errors.New("DIB: truncated header")
	}
	header := int64(le.Uint32(data[:4]))
	if header < dibInfoHeaderSize || header > int64(len(data)) {
		return nil, errors.New("DIB: invalid header size")
	}
	depth := le.Uint16(data[14:16])
	compression := le.Uint32(data[16:20])
	if (depth != 24 && depth != 32) || (compression != dibBIRGB && compression != dibBIBitfields) {
		return nil, nil
	}
	width := int64(int32(le.Uint32(data[4:8])))
	height := int64(int32(le.Uint32(data[8:12])))
	if width <= 0 || height == 0 || le.Uint16(data[12:14]) != 1 {
		return nil, errors.New("DIB: invalid bitmap dimensions or planes")
	}
	topDown := height < 0
	if topDown {
		height = -height
	}

	offset := header
	var masks [4]dibChannel
	if compression == dibBIBitfields {
		if header == dibInfoHeaderSize {
			offset += 12 // BITMAPINFOHEADER stores RGB masks after the header.
		} else if header < 52 {
			return nil, errors.New("DIB: invalid bitfields header size")
		}
		if len(data) < 52 {
			return nil, errors.New("DIB: truncated bitfields masks")
		}
		raw := [4]uint32{le.Uint32(data[40:44]), le.Uint32(data[44:48]), le.Uint32(data[48:52]), 0}
		if depth == 32 {
			if header >= 56 {
				raw[3] = le.Uint32(data[52:56])
			} else if (raw[0]|raw[1]|raw[2])&0xff000000 == 0 {
				// A legacy header has no explicit alpha mask. Use the spare high byte.
				raw[3] = 0xff000000
			}
		}
		var used uint32
		for i, mask := range raw {
			if i == 3 && mask == 0 {
				break // No alpha channel.
			}
			shift := bits.TrailingZeros32(mask)
			max := mask >> shift
			if mask == 0 || mask&used != 0 || uint64(mask)>>depth != 0 || max&(max+1) != 0 {
				return nil, errors.New("DIB: invalid bitfields mask")
			}
			masks[i] = dibChannel{mask: mask, shift: shift, max: max}
			used |= mask
		}
	}

	// A high-color DIB can still carry an optional table of RGBQUADs.
	offset += int64(le.Uint32(data[32:36])) * 4
	pixelBytes := int64(depth / 8)
	rowBytes := width * pixelBytes
	stride := (rowBytes + 3) &^ 3
	// Divide before multiplying: even signed 64-bit arithmetic can overflow
	// on the product of a DIB's dimensions and bytes per pixel.
	if offset > int64(len(data)) || stride > (int64(len(data))-offset)/height {
		return nil, errors.New("DIB: truncated pixels")
	}
	if width*4 > int64(math.MaxInt)/height {
		return nil, errors.New("DIB: image too large")
	}

	img := image.NewNRGBA(image.Rect(0, 0, int(width), int(height)))
	pos, step := offset, stride
	if !topDown {
		pos += (height - 1) * stride
		step = -stride
	}
	var alphaSeen byte
	for y := range int(height) {
		src := data[pos : pos+rowBytes]
		dst := img.Pix[y*img.Stride : (y+1)*img.Stride]
		pos += step
		if compression == dibBIRGB {
			if depth == 32 {
				for i := 0; i < len(dst); i += 4 {
					b, g, r, a := src[i], src[i+1], src[i+2], src[i+3]
					dst[i], dst[i+1], dst[i+2], dst[i+3] = r, g, b, a
					alphaSeen |= a
				}
			} else {
				for i, j := 0, 0; i < len(dst); i, j = i+4, j+3 {
					dst[i], dst[i+1], dst[i+2], dst[i+3] = src[j+2], src[j+1], src[j], 255
				}
			}
			continue
		}
		for i, j := 0, 0; i < len(dst); i, j = i+4, j+int(pixelBytes) {
			v := uint32(src[j]) | uint32(src[j+1])<<8 | uint32(src[j+2])<<16
			if depth == 32 {
				v |= uint32(src[j+3]) << 24
			}
			a := byte(255)
			if masks[3].mask != 0 {
				a = masks[3].value(v)
			}
			dst[i], dst[i+1], dst[i+2], dst[i+3] = masks[0].value(v), masks[1].value(v), masks[2].value(v), a
			alphaSeen |= a
		}
	}
	if depth == 32 && alphaSeen == 0 {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	return img, nil
}

type dibChannel struct {
	mask  uint32
	shift int
	max   uint32
}

func (c dibChannel) value(pixel uint32) byte {
	v := (pixel & c.mask) >> c.shift
	if c.max == 255 {
		return byte(v)
	}
	return byte((uint64(v)*255 + uint64(c.max)/2) / uint64(c.max))
}

// encodeDIB writes a bottom-up 32bpp BI_RGB DIB with straight BGRA pixels.
func encodeDIB(src image.Image) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w > math.MaxInt32 || h > math.MaxInt32 {
		return nil, errors.New("DIB: invalid bitmap dimensions")
	}
	size := uint64(w) * uint64(h) * 4
	if size > math.MaxUint32 || size > math.MaxInt-dibInfoHeaderSize {
		return nil, errors.New("DIB: image too large")
	}
	data := make([]byte, dibInfoHeaderSize+int(size))
	le := binary.LittleEndian
	le.PutUint32(data[0:4], dibInfoHeaderSize)
	le.PutUint32(data[4:8], uint32(w))
	le.PutUint32(data[8:12], uint32(h))
	le.PutUint16(data[12:14], 1)
	le.PutUint16(data[14:16], 32)
	le.PutUint32(data[20:24], uint32(size))

	// Avoid boxing each pixel of the concrete image types returned by png.Decode.
	var pixel func(x, y int) color.NRGBA
	switch src := src.(type) {
	case *image.NRGBA:
		pixel = src.NRGBAAt
	case *image.NRGBA64:
		pixel = func(x, y int) color.NRGBA {
			p := src.NRGBA64At(x, y)
			return color.NRGBA{R: byte(p.R >> 8), G: byte(p.G >> 8), B: byte(p.B >> 8), A: byte(p.A >> 8)}
		}
	case *image.Paletted:
		pixel = func(x, y int) color.NRGBA {
			return color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
		}
	case image.RGBA64Image:
		pixel = func(x, y int) color.NRGBA {
			p := src.RGBA64At(x, y)
			if p.A == 0 {
				return color.NRGBA{}
			}
			if p.A != 0xffff {
				p.R = uint16(uint32(p.R) * 0xffff / uint32(p.A))
				p.G = uint16(uint32(p.G) * 0xffff / uint32(p.A))
				p.B = uint16(uint32(p.B) * 0xffff / uint32(p.A))
			}
			return color.NRGBA{R: byte(p.R >> 8), G: byte(p.G >> 8), B: byte(p.B >> 8), A: byte(p.A >> 8)}
		}
	default:
		pixel = func(x, y int) color.NRGBA {
			return color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
		}
	}
	i := dibInfoHeaderSize
	for row := range h {
		y := b.Max.Y - 1 - row
		for x := b.Min.X; x < b.Max.X; x++ {
			p := pixel(x, y)
			data[i], data[i+1], data[i+2], data[i+3] = p.B, p.G, p.R, p.A
			i += 4
		}
	}
	return data, nil
}
