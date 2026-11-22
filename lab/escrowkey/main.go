// escrowkey makes the lab's offline escrow key (decision 036) once, in the format age-keygen writes:
//
//	go run ./lab/escrowkey <file>
//
// The file (0600) holds the private key, and a "# public key:" line with the recipient that server.yaml
// names. An existing file is left alone. Outside the lab, use age-keygen from the age package.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"filippo.io/age"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: escrowkey <file>")
		os.Exit(2)
	}
	if _, err := os.Stat(os.Args[1]); err == nil {
		fmt.Println("kept", os.Args[1])
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", time.Now().UTC().Format(time.RFC3339), id.Recipient(), id)
	if err := os.WriteFile(os.Args[1], []byte(body), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("made", os.Args[1])
}
