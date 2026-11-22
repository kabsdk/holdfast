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
	test := flag.Bool("test", false, "for the end-to-end tests only: allow HOLDFAST_TEST_NOW_OFFSET")
	flag.Parse()
	now, err := clock(*test, log)
	if err == nil {
		err = run(*cfgPath, now, log)
	}
	if err != nil {
		log.Error("holdfast-server stopped", "err", err)
		os.Exit(1)
	}
}

// clock is the server's clock. The end-to-end tests move it forward with HOLDFAST_TEST_NOW_OFFSET, such as
// 192h, to see the 7-day protection end (plan-m3 Q6). A server started without -test refuses the variable,
// so a production server's clock can never be moved this way.
func clock(test bool, log *slog.Logger) (func() time.Time, error) {
	v := os.Getenv("HOLDFAST_TEST_NOW_OFFSET")
	if v == "" {
		return time.Now, nil
	}
	if !test {
		return nil, errors.New("HOLDFAST_TEST_NOW_OFFSET is set, but holdfast-server was not started with -test; it is for the end-to-end tests only")
	}
	off, err := time.ParseDuration(v)
	if err != nil {
		return nil, fmt.Errorf("HOLDFAST_TEST_NOW_OFFSET: %w", err)
	}
	log.Warn("TEST CLOCK: the server's clock is moved", "offset", off)
	return func() time.Time { return time.Now().Add(off) }, nil
}

func run(cfgPath string, now func() time.Time, log *slog.Logger) error {
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
	// Decision 029: the admin gives each host's S3 key. A provisioner command is optional; with one,
	// a host may be added without a key, and Holdfast generates it and runs the command.
	var prov storage.Provisioner
	if len(set.Storage.ProvisionerCommand) > 0 {
		prov = storage.ExecProvisioner{Command: set.Storage.ProvisionerCommand, Bucket: set.Storage.Bucket}
	}
	srv, err := server.New(set, server.Deps{Store: st, Sealer: sealer, Provisioner: prov, Maintenance: maint, Logger: log, Now: now})
	if err != nil {
		return err
	}
	srv.Run(ctx)

	errc := make(chan error, 3)
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
	// People: the UI and /api/v1 with sessions, on a listener of their own (decision 034).
	var uiSrv *http.Server
	if set.UI.Listen != "" {
		uiSrv = &http.Server{Addr: set.UI.Listen, Handler: srv.UIHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			var err error
			if set.UI.TLS.CertFile != "" {
				log.Info("serving the UI", "addr", set.UI.Listen, "url", set.UI.PublicURL)
				err = uiSrv.ListenAndServeTLS(set.UI.TLS.CertFile, set.UI.TLS.KeyFile)
			} else {
				log.Warn("serving the UI WITHOUT TLS: for tests only", "addr", set.UI.Listen)
				err = uiSrv.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
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
	if uiSrv != nil {
		_ = uiSrv.Shutdown(sctx)
	}
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
