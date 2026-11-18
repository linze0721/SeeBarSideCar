package app

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testClipboardPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, c)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClipboardImageBothWays(t *testing.T) {
	a, b := pair(t, "darwin", "windows", true, testSecret)
	connected(t, a, b)
	eventually(t, "baseline polls", func() bool { return a.p.seqPolls() > 0 && b.p.seqPolls() > 0 })

	fromA := testClipboardPNG(t, color.Black)
	a.p.copyImage(fromA)
	eventuallyWithin(t, 2*time.Second, "A's image on B", func() bool {
		got, sets := b.p.clipboardImage()
		return sets == 1 && bytes.Equal(got, fromA)
	})

	fromB := testClipboardPNG(t, color.White)
	b.p.copyImage(fromB)
	eventuallyWithin(t, 2*time.Second, "B's image on A", func() bool {
		got, sets := a.p.clipboardImage()
		return sets == 1 && bytes.Equal(got, fromB)
	})

	_, _, readsA := a.p.clipboard()
	_, _, readsB := b.p.clipboard()
	time.Sleep(1500 * time.Millisecond)
	for _, check := range []struct {
		name  string
		p     *fakePlat
		reads int
	}{{"A", a.p, readsA}, {"B", b.p, readsB}} {
		_, sets, reads := check.p.clipboard()
		if sets != 1 {
			t.Errorf("%s clipboard set %d times, want 1 (echo)", check.name, sets)
		}
		if reads != check.reads {
			t.Errorf("%s reread clipboard contents without a sequence change", check.name)
		}
	}
}

func TestClipboardUnreadableFilesFallBackToImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unreadable.png")
	if !isWindows && os.Geteuid() != 0 {
		writeFile(t, path, []byte("unreadable"), 0)
	}
	// Root can read a mode-000 file; a missing path tests fallback there.
	a, b := pair(t, "darwin", "windows", true, testSecret)
	connected(t, a, b)
	setFileBase(b, t.TempDir())
	eventually(t, "baseline polls", func() bool { return a.p.seqPolls() > 0 && b.p.seqPolls() > 0 })

	const caption = "a shared caption"
	a.p.copyText(caption)
	eventually(t, "caption on B", func() bool {
		text, _, _ := b.p.clipboard()
		return text == caption
	})

	// Some apps offer both a file URL and an inline image with text.
	img := testClipboardPNG(t, color.White)
	a.p.mu.Lock()
	a.p.clipFiles, a.p.clipImage, a.p.clipText = []string{path}, img, caption
	a.p.clipSeq++
	a.p.mu.Unlock()
	eventuallyWithin(t, 2*time.Second, "inline image on B", func() bool {
		got, _ := b.p.clipboardImage()
		return bytes.Equal(got, img)
	})
	if files := b.p.files(); len(files) != 0 {
		t.Errorf("B clipboard has files %v, want only image and text", files)
	}
	if text, _, _ := b.p.clipboard(); text != caption {
		t.Errorf("B clipboard text = %q, want image caption %q", text, caption)
	}

	// The same text must still sync when copied without the image.
	a.p.copyText(caption)
	eventually(t, "text-only clipboard on B", func() bool {
		got, sets := b.p.clipboardImage()
		return sets == 3 && got == nil
	})
}
