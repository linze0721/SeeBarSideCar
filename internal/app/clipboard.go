package app

import (
	"context"
	"log"
	"sync"
	"time"

	"magic/internal/proto"
)

const (
	clipPoll     = 500 * time.Millisecond
	clipMax      = 1 << 20
	clipImageMax = 64 << 20
)

// clipState suppresses echo: a clipboard change we made ourselves (from the
// peer) is not sent back. Only changes after startup are synced, so
// connecting never overwrites either side's clipboard.
type clipState struct {
	mu      sync.Mutex
	seq     uint64 // last clipboard seq observed or produced by us
	haveSeq bool
	synced  string // text both sides last agreed on
}

func (c *core) clipLoop(ctx context.Context) {
	t := time.NewTicker(clipPoll)
	defer t.Stop()
	for {
		c.pollClipboard()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (c *core) pollClipboard() {
	m := c.clipboardUpdate()
	if m == nil {
		return
	}
	if err := c.Send(m); err != nil {
		log.Printf("clipboard: %v", err)
	} else if m.Type == proto.TClipImage {
		log.Printf("clipboard: sent image (%s)", mb(int64(len(m.Image))))
	}
}

// clipboardUpdate picks the first usable format: files, image, then text.
// Keep the seq check and content reads under the same lock as remote writes.
func (c *core) clipboardUpdate() *proto.Msg {
	s := &c.clip
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := c.p.ClipboardSeq()
	if s.haveSeq && seq == s.seq {
		return nil
	}
	if !s.haveSeq || c.link() == nil {
		s.seq, s.haveSeq = seq, true
		return nil
	}

	if c.cfg.FileClipMaxMB > 0 {
		paths, fileSeq, err := c.p.ClipboardFiles()
		if err == nil {
			if fileSeq != seq {
				return nil // clipboard changed during the read; retry next poll
			}
			if len(paths) > 0 {
				if m, haveFiles := c.prepareFileOffer(paths); haveFiles {
					s.seq, s.synced = seq, ""
					return m
				}
			}
		}
	}
	png, imageSeq, err := c.p.ClipboardImage()
	if err != nil {
		png = nil
	} else if imageSeq != seq {
		return nil
	}
	text, textSeq, err := c.p.ClipboardText()
	if err == nil && textSeq != seq {
		return nil
	}
	if err != nil {
		if len(png) == 0 {
			return nil // retry the text read next poll
		}
		text = ""
	}
	s.seq = seq
	if len(png) > clipImageMax {
		log.Printf("clipboard: image (%s) exceeds %s; not sent", mb(int64(len(png))), mb(clipImageMax))
	} else if len(png) > 0 {
		s.synced = ""
		return &proto.Msg{Type: proto.TClipImage, Image: png, Text: text}
	}
	if text == "" || text == s.synced || len(text) > clipMax {
		return nil
	}
	s.synced = text
	return &proto.Msg{Type: proto.TClipboard, Text: text}
}

func (c *core) recvClipboard(text string) {
	if len(text) > clipMax {
		return
	}
	s := &c.clip
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.synced {
		return
	}
	seq, err := c.p.SetClipboardText(text)
	if err != nil {
		log.Printf("clipboard: %v", err)
		return
	}
	s.seq, s.haveSeq = seq, true
	s.synced = text
}

func (c *core) recvClipImage(png []byte, text string) {
	if len(png) == 0 || len(png) > clipImageMax {
		return
	}
	s := &c.clip
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := c.p.SetClipboardImage(png, text)
	if err != nil {
		log.Printf("clipboard: %v", err)
		return
	}
	s.seq, s.haveSeq, s.synced = seq, true, ""
	log.Printf("clipboard: received image (%s)", mb(int64(len(png))))
}
