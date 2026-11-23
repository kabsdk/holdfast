package serverd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/recovery"
	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
)

// recoveryFile is `holdfast-server recovery-file` (plan-m10 item 3):
//
//	recovery-file -new -credentials-key <key file> <path>   the installer: a new age key and credentials key (the
//	                                                         key file is made unless it exists), the recovery file,
//	                                                         and the age public key printed as "recipient age1…"
//	recovery-file [-config server.yaml] <path>              a running server's: its credentials key; the age key is
//	                                                         the admin's own, to add to the file
//	recovery-file -recipient <path>                         prints the file's age public key, for server.yaml
func recoveryFile(name, defaultConfig string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(name+" recovery-file", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", defaultConfig, "settings file")
	fresh := fs.Bool("new", false, "make a new age key and, unless -credentials-key names a key file that exists, a new credentials key")
	keyPath := fs.String("credentials-key", "", "with -new: the credentials key file, made if it does not exist")
	showRecipient := fs.Bool("recipient", false, "print the recovery file's age public key")
	fs.Usage = func() {
		fmt.Fprintf(out, "usage: %s recovery-file [-new -credentials-key <key file> | -config server.yaml | -recipient] <path>\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("give the recovery file's path")
	}
	path := fs.Arg(0)
	if *showRecipient {
		f, err := recovery.ReadFile(path)
		if err != nil {
			return err
		}
		if len(f.Recipients) == 0 {
			return fmt.Errorf("%s holds no age key: add its AGE-SECRET-KEY-… line", path)
		}
		fmt.Fprintln(out, f.Recipients[0])
		return nil
	}
	node, _ := os.Hostname()
	now := time.Now()
	if *fresh {
		if *keyPath == "" {
			return errors.New("-new needs -credentials-key, the credentials key file to make or use")
		}
		key, err := creds.ReadKeyFile(*keyPath)
		if errors.Is(err, os.ErrNotExist) {
			key = creds.Random(creds.KeySize)
			if err := creds.WriteKeyFile(*keyPath, key); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return err
		}
		if err := recovery.WriteFile(path, recovery.Render(node, now, id, key)); err != nil {
			return err
		}
		fmt.Fprintln(out, "recipient", id.Recipient())
		return nil
	}
	set, err := server.LoadSettings(*cfgPath)
	if err != nil {
		return err
	}
	key, err := creds.ReadKeyFile(set.CredentialsKeyFile)
	if err != nil {
		return err
	}
	if err := recovery.WriteFile(path, recovery.Render(node, now, nil, key)); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (mode 0600) with this server's credentials key. Add your age key's AGE-SECRET-KEY-… line to it,\n"+
		"copy it offline, and delete it here.\n", path)
	return nil
}
