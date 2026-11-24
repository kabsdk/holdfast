// holdfast-agent runs on each machine as root, or in a pod as any user (decision 055).
//
//	holdfast-agent enroll --server https://holdfast.example --name web01.example.com --code XXXX-XXXX-XXXX [--ca-sha256 …]
//	holdfast-agent run [--config /etc/holdfast/agent.yaml] [--enroll-code-file /run/holdfast/enroll/code]
//	holdfast-agent version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kabsdk/holdfast/internal/agent"
	"github.com/kabsdk/holdfast/internal/restic"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "enroll":
		err = enroll(os.Args[2:], log)
	case "run":
		err = run(os.Args[2:], log)
	case "version":
		fmt.Println("holdfast-agent", agent.Version)
	default:
		usage()
	}
	if err != nil {
		log.Error("holdfast-agent", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: holdfast-agent enroll|run|version [flags]")
	os.Exit(2)
}

func enroll(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/holdfast/agent.yaml", "where to write the agent configuration")
	var c agent.Config
	fs.StringVar(&c.Server, "server", "", "Holdfast Server URL")
	fs.StringVar(&c.FQDN, "name", "", "this agent's name, as added in Holdfast (agent_name)")
	fs.StringVar(&c.CASHA256, "ca-sha256", "", "pin the server's certificate chain to this sha256")
	fs.StringVar(&c.StateDir, "state-dir", "", "state directory (default /var/lib/holdfast-agent)")
	fs.StringVar(&c.CredentialsDir, "credentials-dir", "", "credentials directory (default /etc/holdfast/credentials)")
	fs.StringVar(&c.ResticBin, "restic", "", "path to restic (default: "+restic.DefaultBin+", where the agent's package puts it)")
	fs.DurationVar(&c.PollTimeout, "poll-timeout", 0, "long-poll timeout (default 25s)")
	fs.DurationVar(&c.Tick, "tick", 0, "scheduler resolution (default 1s)")
	code := fs.String("code", "", "the single-use enrollment code")
	_ = fs.Parse(args)
	if c.Server == "" || c.FQDN == "" || *code == "" {
		return fmt.Errorf("--server, --name and --code are required")
	}
	// Enrolling again keeps what the host's root set: the restore bounds and the PostgreSQL client.
	if old, err := agent.LoadConfig(*cfgPath); err == nil {
		c.Restore, c.PostgreSQL = old.Restore, old.PostgreSQL
	}
	c.Defaults()
	if err := agent.Enroll(context.Background(), c, *code, log); err != nil {
		return err
	}
	return agent.WriteConfig(*cfgPath, c)
}

func run(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/holdfast/agent.yaml", "agent configuration")
	codeFile := fs.String("enroll-code-file", "", "enroll with the code in this file first, if this agent has not enrolled yet; agent.yaml is not rewritten (a pod's ConfigMap)")
	_ = fs.Parse(args)
	c, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	for _, w := range c.Warnings() {
		log.Warn(w)
	}
	if *codeFile != "" && !agent.Enrolled(c) {
		b, err := os.ReadFile(*codeFile)
		if err != nil {
			return fmt.Errorf("not enrolled yet, and the enrollment code cannot be read: %w", err)
		}
		code := strings.TrimSpace(string(b))
		if code == "" {
			return fmt.Errorf("not enrolled yet, and %s is empty: put an enrollment code there (holdfast agent enroll-code %s)", *codeFile, c.FQDN)
		}
		log.Info("not enrolled yet: enrolling with the code in " + *codeFile)
		if err := agent.Enroll(context.Background(), c, code, log); err != nil {
			return fmt.Errorf("enrolling: %w (a code works once, within 15 minutes: holdfast agent enroll-code %s issues a new one)", err, c.FQDN)
		}
	}
	a, err := agent.New(c, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
