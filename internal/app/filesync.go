package app

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"magic/internal/proto"
)

const (
	fileChunk      = 256 << 10     // TFileData payload
	fileWindow     = 4 * fileChunk // bytes the sender streams per TFileGet
	fileTimeout    = 30 * time.Second
	fileKeep       = 3 // transfer dirs kept in the cache
	fileMaxPending = 8 // queued TFileGets beyond which requests are dropped
)

const isWindows = runtime.GOOS == "windows"

// fileSync is the file-clipboard state. The sender half serves the latest
// local offer from a worker goroutine so disk reads never stall the
// receive goroutine; the receiver half runs on the receive goroutine and
// its timeout timer.
type fileSync struct {
	base string // transfer root; "" = <UserCacheDir>/magic-clip

	mu sync.Mutex
	// sender: only the latest offer is servable
	offerID uint64
	entries []proto.FileEntry
	abs     []string // source path per entry
	gets    []proto.FileChunk
	serving bool
	// receiver
	rx *fileRecv
}

// fileRecv is one incoming transfer, pulled one file entry at a time.
type fileRecv struct {
	id          uint64
	dir         string
	entries     []proto.FileEntry
	roots       []string // absolute paths of the top-level items
	idx         int      // entry being received
	f           *os.File // open while entry idx is a file in progress
	off         int64    // bytes of entry idx received
	win         int64    // bytes left in the current TFileGet window
	total, done int64
	decile      int64 // last logged progress / 10%
	timer       *time.Timer
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func (c *core) fileMax() int64 { return c.cfg.FileClipMaxMB << 20 }

// prepareFileOffer registers a manifest for serving. The bool reports whether
// there were usable entries, even if they exceeded the transfer limit.
func (c *core) prepareFileOffer(paths []string) (*proto.Msg, bool) {
	f := &c.files
	f.mu.Lock()
	f.offerID, f.entries, f.abs = 0, nil, nil
	f.mu.Unlock()
	ents, abs, total := buildOffer(paths)
	if len(ents) == 0 {
		return nil, false
	}
	if total > c.fileMax() {
		log.Printf("files: copied files total %s, over file_clip_max_mb; not sent", mb(total))
		return nil, true
	}
	id := rand.Uint64()
	for id == 0 {
		id = rand.Uint64()
	}
	f.mu.Lock()
	f.offerID, f.entries, f.abs = id, ents, abs
	f.mu.Unlock()
	log.Printf("files: offering %d entries (%s) to peer", len(ents), mb(total))
	return &proto.Msg{Type: proto.TFileOffer, Files: &proto.FileOffer{ID: id, Entries: ents}}, true
}

// buildOffer walks the copied items into a manifest of directories and
// readable regular files; symlinks and special files are skipped.
func buildOffer(paths []string) (ents []proto.FileEntry, abs []string, total int64) {
	seen := make(map[string]bool)
	for _, root := range paths {
		name := filepath.Base(root)
		if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) || seen[name] {
			log.Printf("files: skipping %s (no usable or duplicate name)", root)
			continue
		}
		seen[name] = true
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				log.Printf("files: skipping %s: %v", p, err)
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			wire := name
			if rel != "." {
				wire += "/" + filepath.ToSlash(rel)
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				log.Printf("files: skipping %s (symlink or special file)", p)
				return nil
			}
			info, err := d.Info()
			if err != nil {
				log.Printf("files: skipping %s: %v", p, err)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			e := proto.FileEntry{Path: wire, Dir: d.IsDir(), Mode: uint32(info.Mode().Perm())}
			if !e.Dir {
				f, err := os.Open(p)
				if err != nil {
					log.Printf("files: skipping %s: %v", p, err)
					return nil
				}
				f.Close()
				e.Size = info.Size()
				total += e.Size
			}
			ents = append(ents, e)
			abs = append(abs, p)
			return nil
		})
	}
	return ents, abs, total
}

// recvFileGet queues a peer request for the serving goroutine.
func (c *core) recvFileGet(g *proto.FileChunk) {
	f := &c.files
	f.mu.Lock()
	if len(f.gets) >= fileMaxPending {
		f.mu.Unlock()
		log.Printf("files: too many pending requests; dropped one")
		return
	}
	f.gets = append(f.gets, *g)
	start := !f.serving
	f.serving = true
	f.mu.Unlock()
	if start {
		go c.serveFileGets()
	}
}

func (c *core) serveFileGets() {
	f := &c.files
	for {
		f.mu.Lock()
		if len(f.gets) == 0 {
			f.serving = false
			f.mu.Unlock()
			return
		}
		g := f.gets[0]
		f.gets = f.gets[1:]
		id, ents, abs := f.offerID, f.entries, f.abs
		f.mu.Unlock()
		c.serveFileGet(g, id, ents, abs)
	}
}

// serveFileGet streams up to fileWindow bytes of the requested entry. Any
// refusal is an empty EOF chunk, which the receiver sees as a size
// mismatch and aborts on.
func (c *core) serveFileGet(g proto.FileChunk, id uint64, ents []proto.FileEntry, abs []string) {
	reply := func(off int64, data []byte, eof bool) bool {
		return c.Send(&proto.Msg{Type: proto.TFileData, Chunk: &proto.FileChunk{
			ID: g.ID, Index: g.Index, Offset: off, Data: data, EOF: eof,
		}}) == nil
	}
	if g.ID != id || id == 0 || g.Index < 0 || g.Index >= len(ents) || ents[g.Index].Dir ||
		g.Offset < 0 || g.Offset > ents[g.Index].Size {
		log.Printf("files: peer asked for %016x/%d which is not offered", g.ID, g.Index)
		reply(g.Offset, nil, true)
		return
	}
	e, src := ents[g.Index], abs[g.Index]
	fh, err := os.Open(src)
	if err != nil {
		log.Printf("files: %v", err)
		reply(g.Offset, nil, true)
		return
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() != e.Size {
		log.Printf("files: %s changed since it was copied", src)
		reply(g.Offset, nil, true)
		return
	}
	off := g.Offset
	for range fileWindow / fileChunk {
		buf := make([]byte, min(fileChunk, e.Size-off))
		if _, err := fh.ReadAt(buf, off); err != nil {
			log.Printf("files: %s: %v", src, err)
			reply(off, nil, true)
			return
		}
		off += int64(len(buf))
		eof := off == e.Size
		if !reply(off-int64(len(buf)), buf, eof) || eof {
			return
		}
	}
}

// recvFileOffer replaces any transfer in progress with o and starts
// pulling it.
func (c *core) recvFileOffer(o *proto.FileOffer) {
	f := &c.files
	f.mu.Lock()
	c.abortRecvLocked("superseded by a newer copy")
	m, rx, err := c.startRecvLocked(o)
	f.mu.Unlock()
	switch {
	case err != nil:
		log.Printf("files: offer %016x rejected: %v", o.ID, err)
	case m != nil:
		c.sendFileGet(m)
	default:
		c.finishRecv(rx)
	}
}

// startRecvLocked validates o, prepares its transfer dir and returns the
// first TFileGet, or nil (with rx) when o holds no file data at all.
func (c *core) startRecvLocked(o *proto.FileOffer) (*proto.Msg, *fileRecv, error) {
	if err := checkOffer(o, c.fileMax()); err != nil {
		return nil, nil, err
	}
	base := c.files.base
	if base == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return nil, nil, err
		}
		base = filepath.Join(d, "magic-clip")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("%016x", o.ID)
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, err
	}
	pruneTransfers(base, name)

	rx := &fileRecv{id: o.ID, dir: dir, entries: o.Entries}
	seen := make(map[string]bool)
	for _, e := range o.Entries {
		top, _, _ := strings.Cut(e.Path, "/")
		if !seen[top] {
			seen[top] = true
			rx.roots = append(rx.roots, filepath.Join(dir, top))
		}
		rx.total += e.Size
	}
	rx.timer = time.AfterFunc(fileTimeout, func() {
		c.files.mu.Lock()
		if c.files.rx == rx {
			c.abortRecvLocked("no data for " + fileTimeout.String())
		}
		c.files.mu.Unlock()
	})
	c.files.rx = rx
	log.Printf("files: receiving %d entries (%s) into %s", len(o.Entries), mb(rx.total), dir)
	m, err := rx.next()
	if err != nil {
		c.abortRecvLocked(err.Error())
		return nil, nil, err
	}
	if m == nil {
		c.files.rx = nil
		rx.timer.Stop()
	}
	return m, rx, nil
}

// checkOffer rejects offers this side cannot store safely inside the
// transfer dir.
func checkOffer(o *proto.FileOffer, max int64) error {
	if len(o.Entries) == 0 {
		return errors.New("no entries")
	}
	var total int64
	for _, e := range o.Entries {
		if err := checkPath(e.Path, isWindows); err != nil {
			return err
		}
		if e.Size < 0 || (e.Dir && e.Size != 0) {
			return fmt.Errorf("%q: bad size %d", e.Path, e.Size)
		}
		if e.Size > max-total {
			return fmt.Errorf("total exceeds file_clip_max_mb (%s)", mb(max))
		}
		total += e.Size
	}
	return nil
}

// checkPath accepts only a slash-separated relative path whose elements
// are valid file names on this OS (windows: Windows naming rules).
func checkPath(p string, windows bool) error {
	bad := fmt.Errorf("unsafe path %q", p)
	if p == "" || len(p) > 4096 || strings.IndexByte(p, 0) >= 0 {
		return bad
	}
	for el := range strings.SplitSeq(p, "/") {
		if el == "" || el == "." || el == ".." || len(el) > 255 {
			return bad
		}
		if !windows {
			continue
		}
		// Windows strips trailing dots and spaces, so such names could
		// collide with another entry.
		if strings.ContainsAny(el, `<>:"|?*\`) || strings.HasSuffix(el, ".") || strings.HasSuffix(el, " ") || reservedName(el) {
			return bad
		}
		for _, r := range el {
			if r < 32 {
				return bad
			}
		}
	}
	return nil
}

// reservedName reports DOS device names, which Windows reserves with any
// extension.
func reservedName(el string) bool {
	base, _, _ := strings.Cut(el, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) &&
		base[3] >= '1' && base[3] <= '9'
}

// pruneTransfers removes old transfer dirs so that keep plus the newest
// fileKeep-1 others remain.
func pruneTransfers(base, keep string) {
	ents, err := os.ReadDir(base)
	if err != nil {
		log.Printf("files: %v", err)
		return
	}
	type dirAge struct {
		name string
		t    time.Time
	}
	var old []dirAge
	for _, e := range ents {
		if !e.IsDir() || e.Name() == keep || len(e.Name()) != 16 {
			continue
		}
		if _, err := strconv.ParseUint(e.Name(), 16, 64); err != nil {
			continue
		}
		if info, err := e.Info(); err == nil {
			old = append(old, dirAge{e.Name(), info.ModTime()})
		}
	}
	slices.SortFunc(old, func(a, b dirAge) int { return b.t.Compare(a.t) })
	for _, d := range old[min(len(old), fileKeep-1):] {
		if err := os.RemoveAll(filepath.Join(base, d.name)); err != nil {
			log.Printf("files: %v", err)
		}
	}
}

// next creates directories and empty files up to the next file with data
// and returns the TFileGet for it, or nil when every entry is done.
func (rx *fileRecv) next() (*proto.Msg, error) {
	for ; rx.idx < len(rx.entries); rx.idx++ {
		e := rx.entries[rx.idx]
		p := filepath.Join(rx.dir, filepath.FromSlash(e.Path))
		if e.Dir {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if e.Size == 0 {
			if err := closeFile(fh, e.Mode); err != nil {
				return nil, err
			}
			continue
		}
		rx.f, rx.off = fh, 0
		return rx.get(), nil
	}
	return nil, nil
}

func (rx *fileRecv) get() *proto.Msg {
	rx.win = fileWindow
	rx.timer.Reset(fileTimeout)
	return &proto.Msg{Type: proto.TFileGet, Chunk: &proto.FileChunk{ID: rx.id, Index: rx.idx, Offset: rx.off}}
}

func closeFile(fh *os.File, mode uint32) error {
	var err error
	if !isWindows {
		err = fh.Chmod(fs.FileMode(mode&0o777 | 0o400)) // owner must be able to paste it
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	return err
}

// recvFileData appends a chunk to the current file and requests the next
// window or file.
func (c *core) recvFileData(ch *proto.FileChunk) {
	f := &c.files
	f.mu.Lock()
	rx := f.rx
	if rx == nil || ch.ID != rx.id {
		f.mu.Unlock()
		return // late chunk of an aborted transfer
	}
	m, err := rx.write(ch)
	done := err == nil && m == nil && rx.idx == len(rx.entries)
	if err != nil {
		c.abortRecvLocked(err.Error())
	} else if done {
		f.rx = nil
		rx.timer.Stop()
	}
	f.mu.Unlock()
	if m != nil {
		c.sendFileGet(m)
	}
	if done {
		c.finishRecv(rx)
	}
}

// write applies ch and returns the next TFileGet when one is due.
func (rx *fileRecv) write(ch *proto.FileChunk) (*proto.Msg, error) {
	if rx.f == nil || ch.Index != rx.idx || ch.Offset != rx.off {
		return nil, fmt.Errorf("unexpected chunk %d@%d", ch.Index, ch.Offset)
	}
	e := rx.entries[rx.idx]
	n := int64(len(ch.Data))
	if n > e.Size-rx.off {
		return nil, fmt.Errorf("%s: larger than announced", e.Path)
	}
	if _, err := rx.f.Write(ch.Data); err != nil {
		return nil, err
	}
	rx.off += n
	rx.done += n
	rx.win -= n
	rx.timer.Reset(fileTimeout)
	if d := rx.done * 10 / rx.total; d > rx.decile {
		rx.decile = d
		log.Printf("files: %d%% of %s received", d*10, mb(rx.total))
	}
	if !ch.EOF {
		if rx.win <= 0 {
			return rx.get(), nil
		}
		return nil, nil
	}
	if rx.off != e.Size {
		return nil, fmt.Errorf("%s: got %d of %d bytes (changed or unreadable on the peer)", e.Path, rx.off, e.Size)
	}
	err := closeFile(rx.f, e.Mode)
	rx.f = nil
	if err != nil {
		return nil, err
	}
	rx.idx++
	return rx.next()
}

func (c *core) sendFileGet(m *proto.Msg) {
	if err := c.Send(m); err != nil {
		log.Printf("files: %v", err)
	}
}

// finishRecv applies directory modes and puts the received items on the
// clipboard, recording the seq so pollClipboard does not offer them back.
func (c *core) finishRecv(rx *fileRecv) {
	if !isWindows {
		// Deepest first; owner keeps rwx so the cache can be pruned.
		for i := len(rx.entries) - 1; i >= 0; i-- {
			if e := rx.entries[i]; e.Dir {
				p := filepath.Join(rx.dir, filepath.FromSlash(e.Path))
				if err := os.Chmod(p, fs.FileMode(e.Mode&0o777|0o700)); err != nil {
					log.Printf("files: %v", err)
				}
			}
		}
	}
	s := &c.clip
	s.mu.Lock()
	seq, err := c.p.SetClipboardFiles(rx.roots)
	if err == nil {
		s.seq, s.haveSeq = seq, true
		s.synced = ""
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("files: set clipboard: %v", err)
		return
	}
	log.Printf("files: received %s; ready to paste", mb(rx.total))
}

// abortRecvLocked drops the transfer in progress and its partial copy.
func (c *core) abortRecvLocked(why string) {
	rx := c.files.rx
	if rx == nil {
		return
	}
	c.files.rx = nil
	rx.timer.Stop()
	if rx.f != nil {
		rx.f.Close()
	}
	if err := os.RemoveAll(rx.dir); err != nil {
		log.Printf("files: %v", err)
	}
	log.Printf("files: transfer %016x aborted: %s", rx.id, why)
}

// filesLost aborts transfers in both directions after a disconnect.
func (c *core) filesLost() {
	c.files.mu.Lock()
	c.abortRecvLocked("disconnected")
	c.files.gets = nil
	c.files.mu.Unlock()
}
