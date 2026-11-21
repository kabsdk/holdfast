// holdfast-server: configurator and coordinator.
//
//	holdfast-server -config /etc/holdfast/server.json
//	holdfast-server -config /etc/holdfast/server.json -escrow-export /secure/keys.json
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"holdfast/internal/server"
)

func main() {
	cfgPath := flag.String("config", "/etc/holdfast/server.json", "desired-state config file")
	escrow := flag.String("escrow-export", "", "write all repository passwords to this file (0600) and exit")
	flag.Parse()

	cfg, err := server.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	s, err := server.New(cfg, nil)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	if *escrow != "" {
		if err := s.EscrowExport(*escrow); err != nil {
			log.Fatalf("escrow export: %v", err)
		}
		log.Printf("escrow bundle written to %s; store it offline", *escrow)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.MaintenanceLoop(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("holdfast-server listening on %s (PoC: plain HTTP; production terminates TLS)", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
