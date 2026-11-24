package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// Replacing a lost server (M9, decision 040): the nightly export, and a host brought back from its escrow file.

type exportRecord struct {
	ID           int64     `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	Actor        string    `json:"actor"`
	Name         string    `json:"name"`
	Bytes        int64     `json:"bytes"`
	Error        string    `json:"error"`
	Offsite      bool      `json:"offsite"`
	OffsiteError string    `json:"offsite_error"`
}

func (e exportRecord) line() string {
	s := fmt.Sprintf("%s  meta/%s  %s  by %s", e.StartedAt.Local().Format("2006-01-02 15:04"), e.Name, sizeOf(e.Bytes), e.Actor)
	switch {
	case e.Error != "":
		s = fmt.Sprintf("%s  meta/%s  FAILED by %s: %s", e.StartedAt.Local().Format("2006-01-02 15:04"), e.Name, e.Actor, e.Error)
	case e.Offsite && e.OffsiteError != "":
		s += "; NOT off-site: " + e.OffsiteError
	case e.Offsite:
		s += "; off-site too"
	}
	return s
}

func exportCmd() *cobra.Command {
	c := &cobra.Command{Use: "export", Short: "The nightly export of the server's database to the bucket, which a new server is restored from (decision 040)"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "The latest exports, and when the next is due", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		var v struct {
			On       bool           `json:"on"`
			Offsite  bool           `json:"offsite"`
			Keep     int            `json:"keep"`
			NextAt   *time.Time     `json:"next_at"`
			Recent   []exportRecord `json:"recent"`
			Restored *struct {
				RestoredAt time.Time `json:"restored_at"`
				ExportName string    `json:"export_name"`
				Head       int64     `json:"head"`
			} `json:"restored"`
		}
		raw, err := call("GET", "/api/v1/exports", nil, &v)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		if !v.On {
			fmt.Println("no export is written: server.yaml names no escrow.age_recipient (decision 040)")
		} else {
			where := "the bucket"
			if v.Offsite {
				where = "the bucket and the off-site bucket"
			}
			fmt.Printf("every night at 00:30 to %s, the newest %d kept", where, v.Keep)
			if v.NextAt != nil {
				fmt.Printf("; next %s", v.NextAt.Local().Format("2006-01-02 15:04"))
			}
			fmt.Println()
		}
		if v.Restored != nil {
			fmt.Printf("restored from %s on %s, at revision %d\n", v.Restored.ExportName, v.Restored.RestoredAt.Local().Format("2006-01-02 15:04"), v.Restored.Head)
		}
		for _, e := range v.Recent {
			fmt.Println(e.line())
		}
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "now", Short: "Write an export now (admin)", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		var e exportRecord
		raw, err := call("POST", "/api/v1/exports", nil, &e)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Println(e.line())
		if e.OffsiteError != "" {
			return errors.New("the export was written to the bucket, but not off-site")
		}
		return nil
	}})
	return c
}

func hostImportCmd() *cobra.Command {
	var file, msg string
	c := &cobra.Command{Use: "import <agent> --escrow-file <file>",
		Short: "Add a host back from its decrypted escrow file, with its repository and password, when no export has it (admin)",
		Long: "Add a host back from its escrow file, decrypted with the offline key (age -d -i key.txt escrow/<agent>.age), when the\n" +
			"server was rebuilt without an export that has it. The repository stays; the password must open it. The host gets\n" +
			"no job: set them again. --escrow-file - reads the file from stdin. Its contents are never printed.",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			if file == "" {
				return errors.New("--escrow-file is needed: the host's escrow file, decrypted")
			}
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			var res struct {
				Revision           int64      `json:"revision"`
				CredentialsVersion int        `json:"credentials_version"`
				RepositoryKeys     int        `json:"repository_keys"`
				Warning            string     `json:"warning"`
				Enrollment         enrollment `json:"enrollment"`
			}
			raw, err := call("POST", "/api/v1/agents/import", map[string]any{"fqdn": a[0], "escrow_file": string(data), "message": msg}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("imported %s in revision %d: its repository opens with the escrowed password (credentials version %d, %d repository keys)\n"+
				"it has no job yet: set them again (holdfast job set)\n%s",
				a[0], res.Revision, res.CredentialsVersion, res.RepositoryKeys, res.Enrollment.howTo())
			if res.Warning != "" {
				fmt.Println("\nwarning:", res.Warning)
			}
			return nil
		}}
	c.Flags().StringVar(&file, "escrow-file", "", "the host's escrow file, decrypted; - for stdin")
	c.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	return c
}
