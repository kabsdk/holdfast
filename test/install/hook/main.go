// Command hook is the install test's alert receiver: it answers every request with 200 and prints one line for it, the
// method, the path and the body, for the test to read with docker logs.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	log.Fatal(http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		fmt.Fprintf(os.Stdout, "%s %s %s\n", r.Method, r.URL.Path, strings.ReplaceAll(string(b), "\n", " "))
	})))
}
