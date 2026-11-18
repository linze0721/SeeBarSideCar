package app

import (
	"context"
	"log"
	"sync"

	"magic/internal/plat"
)

// Run connects to the peer and serves KVM, clipboard and window sharing
// until ctx ends. cfg must be normalized (LoadConfig does that).
func Run(ctx context.Context, cfg *Config, p plat.Platform) error {
	c := newCore(cfg, p)
	sh := NewSharing(cfg, p, c)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := sh.ServePanel(ctx); err != nil {
			log.Printf("panel: %v", err)
		}
	})
	err := c.run(ctx, sh)
	cancel()
	wg.Wait()
	if *cfg.VirtualDisplay {
		// Windows dragged onto it return to a real monitor.
		if _, err := p.SetVirtualDisplay(nil); err != nil {
			log.Printf("virtual display: remove: %v", err)
		}
	}
	return err
}
