// Command magic shares one keyboard/mouse, the clipboard and selected app
// windows between a Windows PC and a Mac on the same LAN.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"magic/internal/app"
	"magic/internal/plat"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to the JSON config")
	flag.Parse()
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := app.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	var runErr error
	err = plat.Run(func() {
		p, err := plat.New()
		if err != nil {
			runErr = err
			return
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runErr = app.Run(ctx, cfg, p)
		if ctx.Err() != nil {
			log.Printf("shutting down: %v", context.Cause(ctx))
		}
	})
	if err == nil {
		err = runErr
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
