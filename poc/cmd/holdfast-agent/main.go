// holdfast-agent: runs on each protected Linux host as root (systemd unit in deploy/).
//
//	holdfast-agent -config /etc/holdfast/agent.json
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"holdfast/internal/agent"
)

func main() {
	cfgPath := flag.String("config", "/etc/holdfast/agent.json", "agent bootstrap config")
	flag.Parse()
	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	a, err := agent.New(cfg, nil)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
