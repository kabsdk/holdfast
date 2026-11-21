// holdfast-freshness: independent storage witness for Prometheus.
//
//	holdfast-freshness -repos /srv/holdfast/repos -listen :9817
//	holdfast-freshness -repos /srv/holdfast/repos -once
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"holdfast/internal/freshness"
)

func main() {
	root := flag.String("repos", "", "root directory holding one restic repository per host")
	listen := flag.String("listen", ":9817", "metrics listen address")
	once := flag.Bool("once", false, "print metrics once and exit")
	flag.Parse()
	if *root == "" {
		log.Fatal("-repos is required")
	}
	l := freshness.LocalLister{Root: *root}
	if *once {
		st, err := l.List()
		fmt.Print(freshness.Metrics(st, err))
		return
	}
	http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		st, err := l.List()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(freshness.Metrics(st, err)))
	})
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
