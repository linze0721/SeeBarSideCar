package plat

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfDIB        = 8
	cfDIBV5      = 17
	pngSignature = "\x89PNG\r\n\x1a\n"
)

var pngClipboardFormat = sync.OnceValues(func() (uintptr, error) {
	format, _, e := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("PNG"))))
	if format == 0 {
		return 0, callErr("RegisterClipboardFormatW (PNG)", e)
	}
	return format, nil
})

func (p *winPlat) ClipboardImage() ([]byte, uint64, error) {
	data, dib, seq, err := readClipboardImage()
	if dib == nil {
		return data, seq, err
	}
	// PNG encoding can be slow. Do it after freeing the clipboard for other apps.
	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, dib); err != nil {
		return nil, seq, fmt.Errorf("plat: encode clipboard PNG: %w", err)
	}
	return buf.Bytes(), seq, nil
}

func isClipFormatAvailable(format uintptr) bool {
	r, _, _ := procIsClipboardFormatAvailable.Call(format)
	return r != 0
}

func readClipboardImage() ([]byte, *image.NRGBA, uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	seq := clipSeq()
	pngFormat, err := pngClipboardFormat()
	if err != nil {
		return nil, nil, seq, err
	}
	if !isClipFormatAvailable(pngFormat) && !isClipFormatAvailable(cfDIBV5) && !isClipFormatAvailable(cfDIB) {
		return nil, nil, seq, nil
	}
	if err := openClipboard(0); err != nil {
		return nil, nil, seq, err
	}
	defer procCloseClipboard.Call()

	var firstErr error
	for _, format := range [...]uintptr{pngFormat, cfDIBV5, cfDIB} {
		if !isClipFormatAvailable(format) {
			continue
		}
		data, dib, err := readClipboardImageFormat(format, format == pngFormat)
		if data != nil || dib != nil {
			return data, dib, clipSeq(), nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, nil, clipSeq(), firstErr
}

// readClipboardImageFormat uses the borrowed global memory only while locked.
func readClipboardImageFormat(format uintptr, isPNG bool) ([]byte, *image.NRGBA, error) {
	h, _, e := procGetClipboardData.Call(format)
	if h == 0 {
		return nil, nil, callErr("GetClipboardData", e)
	}
	size, _, e := procGlobalSize.Call(h)
	if size == 0 {
		return nil, nil, callErr("GlobalSize", e)
	}
	if size > math.MaxInt {
		return nil, nil, errors.New("plat: clipboard image too large")
	}
	ptr, _, e := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, nil, callErr("GlobalLock", e)
	}
	defer procGlobalUnlock.Call(h)
	data := unsafe.Slice((*byte)(uptr(ptr)), int(size))
	if isPNG {
		if !bytes.HasPrefix(data, []byte(pngSignature)) {
			return nil, nil, errors.New("plat: clipboard PNG has an invalid signature")
		}
		return bytes.Clone(data), nil, nil
	}
	img, err := decodeDIB(data)
	return nil, img, err
}

func (p *winPlat) SetClipboardImage(data []byte, text string) (uint64, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("plat: decode clipboard PNG: %w", err)
	}
	dib, err := encodeDIB(img)
	if err != nil {
		return 0, err
	}
	pngFormat, err := pngClipboardFormat()
	if err != nil {
		return 0, err
	}
	return setClipboard(func() error {
		if err := setClipData(pngFormat, data); err != nil {
			return err
		}
		if err := setClipData(cfDIB, dib); err != nil {
			return err
		}
		if text != "" {
			return setClipText(text)
		}
		return nil
	})
}
