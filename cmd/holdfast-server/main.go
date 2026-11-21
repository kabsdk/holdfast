// holdfast-server: the configurator and the maintenance leader.
//
//	holdfast-server -config /etc/holdfast/server.yaml
//	holdfast-server keygen /etc/holdfast/credentials.key
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) == 3 && os.Args[1] == "keygen" {
		if err := creds.WriteNewKeyFile(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "keygen:", err)
			os.Exit(1)
		}
		fmt.Println("wrote a new credentials key to", os.Args[2], "(mode 0600); keep a copy offline with the escrow bundle")
		return
	}
	cfgPath := flag.String("config", "/etc/holdfast/server.yaml", "settings file")
	flag.Parse()
	if err := run(*cfgPath, log); err != nil {
		log.Error("holdfast-server stopped", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	set, err := server.LoadSettings(cfgPath)
	if err != nil {
		return err
	}
	sealer, err := creds.LoadKeyFile(set.CredentialsKeyFile)
	if err != nil {
		return err
	}
	maint, err := storage.ReadKeyFile(set.Storage.MaintenanceCredentialsFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, set.DatabaseDSN)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	var prov storage.Provisioner = storage.ExecProvisioner{Command: set.Storage.ProvisionerCommand, Bucket: set.Storage.Bucket}
	if len(set.Storage.ProvisionerCommand) == 0 {
		log.Warn("storage.provisioner_command is not set: hosts' S3 identities must be made outside Holdfast")
		prov = storage.NoopProvisioner{}
	}
	srv, err := server.New(set, server.Deps{Store: st, Sealer: sealer, Provisioner: prov, Maintenance: maint, Logger: log})
	if err != nil {
		return err
	}
	srv.Run(ctx)

	errc := make(chan error, 2)
	agentSrv := &http.Server{Addr: set.Listen, Handler: srv.AgentHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		var err error
		if set.TLS.CertFile != "" {
			log.Info("listening for agents", "addr", set.Listen, "tls", true)
			err = agentSrv.ListenAndServeTLS(set.TLS.CertFile, set.TLS.KeyFile)
		} else {
			log.Warn("listening for agents WITHOUT TLS: for tests only", "addr", set.Listen)
			err = agentSrv.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	var apiSrv *http.Server
	if set.APISocket != "" {
		l, err := listenSocket(set.APISocket)
		if err != nil {
			return err
		}
		apiSrv = &http.Server{Handler: srv.APIHandler(true), ConnContext: server.PeerActor(), ReadHeaderTimeout: 10 * time.Second}
		log.Info("serving /api/v1 on the root-only socket", "path", set.APISocket)
		go func() {
			if err := apiSrv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = agentSrv.Shutdown(sctx)
	if apiSrv != nil {
		_ = apiSrv.Shutdown(sctx)
	}
	return nil
}

// listenSocket opens the API socket: its directory is 0700 and the socket 0600, so only its owner
// (root, in production) can use it.
func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path) // a socket left by a previous run
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
