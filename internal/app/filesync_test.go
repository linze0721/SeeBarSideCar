package app

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"magic/internal/proto"
)

func setFileBase(n *node, dir string) {
	n.c.files.mu.Lock()
	n.c.files.base = dir
	n.c.files.mu.Unlock()
}

func writeFile(t *testing.T, p string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestFileClipboardTransfer(t *testing.T) {
	a, b := pair(t, "darwin", "windows", true, testSecret)
	connected(t, a, b)
	setFileBase(a, t.TempDir())
	dst := t.TempDir()
	setFileBase(b, dst)
	eventually(t, "baseline polls", func() bool { return a.p.seqPolls() > 0 && b.p.seqPolls() > 0 })

	src := t.TempDir()
	big := make([]byte, 3<<20+12345)
	r := rand.NewChaCha8([32]byte{1})
	r.Read(big)
	folder, top := filepath.Join(src, "folder"), filepath.Join(src, "top.txt")
	writeFile(t, filepath.Join(folder, "sub", "big.bin"), big, 0o640)
	writeFile(t, filepath.Join(folder, "sub", "empty.txt"), nil, 0o600)
	if err := os.Mkdir(filepath.Join(folder, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(top, filepath.Join(folder, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, top, []byte("hello"), 0o644)

	a.p.copyFiles(folder, top)
	var got []string
	eventually(t, "files on B's clipboard", func() bool { got = b.p.files(); return len(got) == 2 })

	dirs, err := os.ReadDir(dst)
	if err != nil || len(dirs) != 1 {
		t.Fatalf("transfer dirs %v, %v", dirs, err)
	}
	root := filepath.Join(dst, dirs[0].Name())
	if want := []string{filepath.Join(root, "folder"), filepath.Join(root, "top.txt")}; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("B clipboard %v, want %v", got, want)
	}
	check := func(rel string, want []byte, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, rel)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("%s: %d bytes differ from the %d sent", rel, len(data), len(want))
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != mode {
			t.Fatalf("%s: mode %v, want %v", rel, st.Mode().Perm(), mode)
		}
	}
	check("folder/sub/big.bin", big, 0o640)
	check("folder/sub/empty.txt", nil, 0o600)
	check("top.txt", []byte("hello"), 0o644)
	if st, err := os.Stat(filepath.Join(root, "folder", "emptydir")); err != nil || !st.IsDir() {
		t.Fatalf("emptydir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "folder", "link")); !os.IsNotExist(err) {
		t.Fatalf("symlink was transferred: %v", err)
	}

	// Several more polls on both sides: B must not offer the files back.
	pa, pb := a.p.seqPolls(), b.p.seqPolls()
	eventually(t, "more polls", func() bool { return a.p.seqPolls() >= pa+3 && b.p.seqPolls() >= pb+3 })
	if _, sets, _ := a.p.clipboard(); sets != 0 {
		t.Fatalf("A clipboard set %d times, want 0 (echo)", sets)
	}
	if _, sets, _ := b.p.clipboard(); sets != 1 {
		t.Fatalf("B clipboard set %d times, want 1", sets)
	}
}

func TestFileOfferRejectsUnsafePaths(t *testing.T) {
	a := newNode(t, testCfg("alpha", "right", testSecret, true), newFakePlat("darwin", macBounds))
	base := t.TempDir()
	setFileBase(a, base)
	pc, msgs := rawPeer(t, a)
	eventually(t, "connection", func() bool { return a.c.Peer() != nil })

	for _, p := range []string{"../evil", "ok/../../evil", "/abs", "a//b", "a/./b", "nul\x00"} {
		bad := &proto.FileOffer{ID: 1, Entries: []proto.FileEntry{{Path: "x", Dir: true}, {Path: p, Size: 3}}}
		if err := pc.Send(&proto.Msg{Type: proto.TFileOffer, Files: bad}); err != nil {
			t.Fatal(err)
		}
	}
	good := &proto.FileOffer{ID: 2, Entries: []proto.FileEntry{{Path: "f.txt", Size: 3, Mode: 0o644}}}
	if err := pc.Send(&proto.Msg{Type: proto.TFileOffer, Files: good}); err != nil {
		t.Fatal(err)
	}
	// Messages are handled in order, so the first request is for the
	// good offer only if every bad one was rejected.
	select {
	case m := <-msgs:
		if m.Type != proto.TFileGet || m.Chunk.ID != 2 || m.Chunk.Index != 0 || m.Chunk.Offset != 0 {
			t.Fatalf("got %+v %+v, want TFileGet for offer 2", m, m.Chunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no TFileGet for the good offer")
	}
	if err := pc.Send(&proto.Msg{Type: proto.TFileData, Chunk: &proto.FileChunk{ID: 2, Data: []byte("abc"), EOF: true}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "clipboard set", func() bool { return len(a.p.files()) == 1 })
	ents, _ := os.ReadDir(base)
	if len(ents) != 1 || ents[0].Name() != "0000000000000002" {
		t.Fatalf("transfer dirs %v, want only offer 2", ents)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(base), "evil")); !os.IsNotExist(err) {
		t.Fatal("file written outside the transfer dir")
	}
}

func TestCheckPath(t *testing.T) {
	for _, tc := range []struct {
		p       string
		windows bool
		ok      bool
	}{
		{"a/b c/d.txt", true, true},
		{"résumé.pdf", true, true},
		{"..", false, false},
		{"a/..", false, false},
		{"", false, false},
		{"a/", false, false},
		{"/etc/passwd", false, false},
		{`a\..\b`, false, true}, // a plain name on macOS
		{`a\..\b`, true, false},
		{"C:/x", true, false},
		{"a?.txt", true, false},
		{"CON", true, false},
		{"nul.txt", true, false},
		{"com7.log", true, false},
		{"lpt1", true, false},
		{"COM0", true, true},
		{"console", true, true},
		{"con", false, true},
		{"name.", true, false},
		{"name ", true, false},
		{"name.", false, true},
	} {
		if err := checkPath(tc.p, tc.windows); (err == nil) != tc.ok {
			t.Errorf("checkPath(%q, windows=%v) = %v, want ok=%v", tc.p, tc.windows, err, tc.ok)
		}
	}
}
