// holdfast-freshness: the independent storage witness for Prometheus (decision 008).
//
//	holdfast-freshness -endpoint https://rgw.example.internal -bucket holdfast -credentials-file /etc/holdfast/witness.env
//	holdfast-freshness ... -once
//
// Its key needs list access to repos/ only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kabsdk/holdfast/internal/freshness"
	"github.com/kabsdk/holdfast/internal/server/storage"
)

func main() {
	endpoint := flag.String("endpoint", "", "S3 endpoint URL")
	bucket := flag.String("bucket", "", "bucket holding repos/<fqdn>/")
	region := flag.String("region", "", "S3 region")
	credFile := flag.String("credentials-file", "/etc/holdfast/witness.env", "file with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (list-only)")
	listen := flag.String("listen", ":9817", "metrics listen address")
	once := flag.Bool("once", false, "print the metrics once and exit")
	staleAfter := flag.Duration("stale-after", 48*time.Hour, "a host is stale after this long without a new snapshot, when the server's schedules are not available")
	flag.Parse()
	if *endpoint == "" || *bucket == "" {
		log.Fatal("-endpoint and -bucket are required")
	}
	key, err := storage.ReadKeyFile(*credFile)
	if err != nil {
		log.Fatal(err)
	}
	l, err := freshness.NewS3Lister(*endpoint, *bucket, *region, key.AccessKeyID, key.SecretAccessKey)
	if err != nil {
		log.Fatal(err)
	}
	scrape := func(ctx context.Context) string {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		st, err := l.List(ctx)
		if err != nil {
			log.Printf("listing storage failed: %v", err)
		}
		return freshness.Metrics(st, err, *staleAfter)
	}
	if *once {
		fmt.Print(scrape(context.Background()))
		return
	}
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(scrape(r.Context())))
	})
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
