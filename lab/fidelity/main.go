// hf-fidelity runs the metadata fixture on a lab VM (M2's "done when"): build it, back it up and restore it
// through Holdfast, then compare. It needs root. It is a lab tool and is not shipped.
//
//	hf-fidelity build <dir>
//	hf-fidelity compare [--json] <source dir> <restored dir>
//
// compare prints the 13 properties and exits 1 if any differs.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kabsdk/holdfast/internal/fidelity"
)

func main() {
	args := os.Args[1:]
	asJSON := false
	if len(args) > 0 && args[0] == "compare" && len(args) > 1 && args[1] == "--json" {
		asJSON, args = true, append([]string{"compare"}, args[2:]...)
	}
	switch {
	case len(args) == 2 && args[0] == "build":
		if err := fidelity.Build(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "hf-fidelity build:", err)
			os.Exit(1)
		}
	case len(args) == 3 && args[0] == "compare":
		src, err1 := fidelity.Scan(args[1])
		dst, err2 := fidelity.Scan(args[2])
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "hf-fidelity compare:", err1, err2)
			os.Exit(2)
		}
		results := fidelity.Compare(src, dst)
		failed := false
		for _, r := range results {
			failed = failed || !r.Pass
		}
		if asJSON {
			b, _ := json.MarshalIndent(results, "", "  ")
			fmt.Println(string(b))
		} else {
			for _, r := range results {
				fmt.Printf("%-50s %-5v %s\n", r.Property, r.Pass, r.Detail)
			}
		}
		if failed {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: hf-fidelity build <dir> | hf-fidelity compare [--json] <source dir> <restored dir>")
		os.Exit(2)
	}
}
