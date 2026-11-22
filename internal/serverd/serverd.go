// Package serverd is the main of holdfast-server and of holdfast-vault, which is holdfast-server in the vault role
// (decision 039): the same program, with its own name, settings file and package.
package serverd

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
	"strconv"
	"syscall"
	"time"

	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Main runs the program called name, whose settings are at defaultConfig unless -config says otherwise, and must
// be for role.
func Main(name, defaultConfig, role string) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) == 3 && os.Args[1] == "keygen" {
		if err := creds.WriteNewKeyFile(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "keygen:", err)
			os.Exit(1)
		}
		fmt.Println("wrote a new credentials key to", os.Args[2], "(mode 0600); keep a copy offline with the escrow bundle")
		return
	}
	cfgPath := flag.String("config", defaultConfig, "settings file")
	test := flag.Bool("test", false, "for the end-to-end tests only: allow HOLDFAST_TEST_NOW_OFFSET and HOLDFAST_TEST_MIN_PROTOCOL")
	flag.Parse()
	now, err := clock(name, *test, log)
	var minProto int
	if err == nil {
		minProto, err = testMinProtocol(name, *test, log)
	}
	if err == nil {
		err = run(name, *cfgPath, role, now, minProto, log)
	}
	if err != nil {
		log.Error(name+" stopped", "err", err)
		os.Exit(1)
	}
}

// clock is the server's clock. The end-to-end tests move it forward with HOLDFAST_TEST_NOW_OFFSET, such as
// 192h, to see the 7-day protection end (plan-m3 Q6). A server started without -test refuses the variable,
// so a production server's clock can never be moved this way.
func clock(name string, test bool, log *slog.Logger) (func() time.Time, error) {
	v := os.Getenv("HOLDFAST_TEST_NOW_OFFSET")
	if v == "" {
		return time.Now, nil
	}
	if !test {
		return nil, fmt.Errorf("HOLDFAST_TEST_NOW_OFFSET is set, but %s was not started with -test; it is for the end-to-end tests only", name)
	}
	off, err := time.ParseDuration(v)
	if err != nil {
		return nil, fmt.Errorf("HOLDFAST_TEST_NOW_OFFSET: %w", err)
	}
	log.Warn("TEST CLOCK: the server's clock is moved", "offset", off)
	return func() time.Time { return time.Now().Add(off) }, nil
}

// testMinProtocol raises the oldest agent protocol the server accepts, so the end-to-end tests can see an
// agent that is too old (plan-m5 item 1). Like the test clock, it needs -test.
func testMinProtocol(name string, test bool, log *slog.Logger) (int, error) {
	v := os.Getenv("HOLDFAST_TEST_MIN_PROTOCOL")
	if v == "" {
		return 0, nil
	}
	if !test {
		return 0, fmt.Errorf("HOLDFAST_TEST_MIN_PROTOCOL is set, but %s was not started with -test; it is for the end-to-end tests only", name)
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("HOLDFAST_TEST_MIN_PROTOCOL is %q, not a protocol version", v)
	}
	log.Warn("TEST: the server accepts only agents of this protocol or later", "min_protocol", n)
	return n, nil
}

func run(name, cfgPath, role string, now func() time.Time, minProto int, log *slog.Logger) error {
	set, err := server.LoadSettings(cfgPath)
	if err != nil {
		return err
	}
	if set.Role != role {
		other := map[string]string{server.RoleServer: "holdfast-server", server.RoleVault: "holdfast-vault"}[set.Role]
		return fmt.Errorf("%s says role: %s, which is %s's; %s needs role: %s", cfgPath, set.Role, other, name, role)
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
	srv, err := server.New(set, server.Deps{Store: st, Sealer: sealer, Provisioner: prov, Maintenance: maint, Logger: log, Now: now, MinProtocol: minProto})
	if err != nil {
		return err
	}
	srv.Run(ctx)

	errc := make(chan error, 3)
	agentSrv := &http.Server{Addr: set.Listen, Handler: srv.AgentHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		var err error
		if set.TLS.CertFile != "" {
			log.Info("listening for agents and vaults, and /metrics", "addr", set.Listen, "tls", true)
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
