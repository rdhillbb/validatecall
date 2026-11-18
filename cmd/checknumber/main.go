// Command checknumber is the one service of AD-1, run on the operator's Mac
// in Stage 1 (TS-6). It serves the test page and the check API on one
// loopback listener and the Telnyx events on another; the tunnel reaches only
// the events listener (AD-6). Run guide: docs/RUNNING.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"checknumber/internal/check"
	"checknumber/internal/config"
	"checknumber/internal/httpapi"
	"checknumber/internal/logfile"
	"checknumber/internal/result"
	"checknumber/internal/store"
	"checknumber/internal/telnyx"
	"checknumber/internal/tunnel"
	"checknumber/internal/web"
)

const (
	// apiPrefix holds every check API route (POST /api/check, GET /api/history);
	// everything else on the local listener is the test page.
	apiPrefix  = "/api/"
	pagePrefix = "/"

	// TC-20: the reason given to a check a restart cut off.
	reasonAppRestarted = "app restarted"

	// readHeaderTimeout bounds a slow client on either listener; the events
	// listener is public through the tunnel.
	readHeaderTimeout = 10 * time.Second
	// eventsReadTimeout bounds reading one Telnyx event. The local listener
	// has no read or write limit because a check reply waits for the call (AD-4).
	eventsReadTimeout = 30 * time.Second
	// shutdownGrace is how long a stop waits for open requests; a check cut
	// off is marked Unknown at the next start (TC-20).
	shutdownGrace = 10 * time.Second

	// TC-32: the tunnel program; openTunnel gives it the events address.
	tunnelProgram = "cloudflared"
	// tunnelOrigin is put before the events address: the tunnel reaches the
	// events listener over plain HTTP on the loopback address.
	tunnelOrigin = "http://"

	// localhostName is the one host name, besides the loopback address, by
	// which the local listener may be addressed (TC-41).
	localhostName = "localhost"

	// The words of the start-up line (D-8).
	settingOn  = "on"
	settingOff = "off"
)

// errTunnelExited ends the app when the tunnel program stops: without the
// tunnel no call event arrives (TC-32).
var errTunnelExited = errors.New("tunnel exited")

// callEnder is what serve needs of the runner when the app stops;
// *check.Runner satisfies it.
type callEnder interface {
	Shutdown(ctx context.Context) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// run logs the error that ends it itself, so that line goes into the log file
// while it is still open (TC-44). A refused setting comes before the log file
// is opened and goes to standard error (TC-45).
func run(ctx context.Context) (err error) {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("checknumber: %v", err)
		return err
	}
	out, closeLog := openLog(cfg.LogFile, os.Stderr)
	log.SetOutput(out)
	defer func() {
		if err != nil {
			log.Printf("checknumber: %v", err)
		}
		// A line logged after the close, by a goroutine still ending, must not
		// go to the closed file and be lost.
		log.SetOutput(os.Stderr)
		if closeErr := closeLog(); closeErr != nil {
			log.Printf("checknumber: close log file %s: %v", cfg.LogFile, closeErr)
		}
	}()
	logStartup(cfg)
	st, err := store.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := prepare(ctx, st, time.Now()); err != nil {
		return err
	}

	cfg, tun, err := openTunnel(ctx, cfg, tunnelProgram)
	if err != nil {
		return err
	}
	// Without the tunnel setting there is no tunnel, and a nil channel never
	// ends serve.
	var tunnelExit <-chan struct{}
	if tun != nil {
		defer tun.Stop()
		tunnelExit = tun.Done()
	}

	// The client timeout stops one stuck Telnyx command from holding a call
	// past the check limit (TC-18).
	client := telnyx.NewClient(telnyx.ProductionBaseURL, cfg.TelnyxAPIKey, &http.Client{Timeout: cfg.CheckLimit})
	runner := newRunner(cfg, client)
	svc := check.NewService(st, runner, time.Now)
	local, events := buildHandlers(cfg, svc, runner)

	return serve(ctx, []*http.Server{
		{Addr: cfg.LocalAddr(), Handler: local, ReadHeaderTimeout: readHeaderTimeout},
		{Addr: cfg.EventsAddr(), Handler: events, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: eventsReadTimeout},
	}, runner, tunnelExit)
}

// openLog returns the writer for log lines and the function that closes it.
// With an empty path it is stderr, as before (R-32). Otherwise it is the log
// file at path, which sets itself aside (R-30). A file that cannot be opened
// is reported on stderr and logging stays there: a refusal to start would make
// the login service restart the app every 10 seconds (AD-7, TC-44).
func openLog(path string, stderr io.Writer) (io.Writer, func() error) {
	if path == "" {
		return stderr, closeNothing
	}
	file, err := logfile.Open(path)
	if err != nil {
		// The same time stamp form as every other log line (TC-49).
		log.New(stderr, log.Prefix(), log.Flags()).Printf(
			"checknumber: cannot open log file %s, logging to standard error: %v", path, err)
		return stderr, closeNothing
	}
	return file, file.Close
}

// closeNothing is the close function of standard error, which stays open.
func closeNothing() error { return nil }

// logStartup writes the one line that says how this run is configured (D-8).
// It names the database, the tunnel mode, whether the check API asks for its
// key, and the two listeners; never a key or a line of the settings file (C-4).
func logStartup(cfg config.Config) {
	tunnelMode := cfg.Tunnel
	if tunnelMode == "" {
		tunnelMode = settingOff
	}
	keyCheck := settingOff
	if cfg.APIKey != "" {
		keyCheck = settingOn
	}
	log.Printf("checknumber: starting with database %q, tunnel %s, API key check %s, local %s, events %s",
		cfg.Database, tunnelMode, keyCheck, cfg.LocalAddr(), cfg.EventsAddr())
}

// openTunnel starts the tunnel when the tunnel setting asks for it and
// returns cfg with the tunnel's address as the public address, which changes
// at every start (TC-32). The tunnel points at the events listener only
// (AD-6). Without the setting it returns cfg as it is and no tunnel.
func openTunnel(ctx context.Context, cfg config.Config, program string) (config.Config, *tunnel.Tunnel, error) {
	if cfg.Tunnel != config.TunnelCloudflared {
		return cfg, nil, nil
	}
	tun, err := tunnel.Start(ctx, program, []string{"tunnel", "--no-autoupdate", "--url", tunnelOrigin + cfg.EventsAddr()})
	if err != nil {
		return cfg, nil, err
	}
	cfg.PublicURL = tun.URL()
	return cfg, tun, nil
}

// newRunner builds the runner from the settings. cfg must already hold the
// public address: with the tunnel setting it is known only after openTunnel.
func newRunner(cfg config.Config, cmd check.Commands) *check.Runner {
	return check.NewRunner(cmd, check.RunnerConfig{
		From:          cfg.TelnyxFromNumber,
		ConnectionID:  cfg.TelnyxConnectionID,
		WebhookURL:    cfg.WebhookURL(),
		RecordingsDir: cfg.RecordingsDir,
		Limit:         cfg.CheckLimit,
		Combine:       cfg.CombineDisconnected,
	})
}

// prepare creates the schema and closes every check a previous run left in
// progress as Unknown, "app restarted", so the next request for that number
// dials again (TC-20, R-21). No call can be in flight yet.
func prepare(ctx context.Context, st *store.Store, now time.Time) error {
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("prepare database: %w", err)
	}
	n, err := st.MarkUnfinished(ctx, string(result.Unknown), reasonAppRestarted, now)
	if err != nil {
		return fmt.Errorf("prepare database: %w", err)
	}
	log.Printf("checknumber: %d unfinished check(s) marked %s, reason %q", n, result.Unknown, reasonAppRestarted)
	return nil
}

// buildHandlers returns the local handler (test page and check API) and the
// events handler, which is the only one the tunnel reaches (AD-6). The events
// handler has no host check: Telnyx addresses it by the tunnel's name.
func buildHandlers(cfg config.Config, svc httpapi.Checker, runner httpapi.EventSink) (local, events http.Handler) {
	mux := http.NewServeMux()
	api := httpapi.NewAPI(svc)
	// M-2: with CHECKNUMBER_API_KEY set, every API request must carry the key
	// the fax system sends (R-12). The page is never wrapped: a key on the
	// page was not chosen (AD-6).
	if cfg.APIKey != "" {
		api = httpapi.RequireKey(cfg.APIKey, api)
	}
	// The page already refuses cross-origin POSTs. The API gets the same guard:
	// a stranger's web page must not make the operator's browser place a paid
	// call on the local listener (AD-6). Requests from other programs carry no
	// browser origin headers and pass.
	mux.Handle(apiPrefix, http.NewCrossOriginProtection().Handler(api))
	mux.Handle(pagePrefix, web.NewPage(svc, cfg.RecordingsDir))
	return requireLocalHost(cfg, mux), httpapi.NewEvents(cfg.TelnyxPublicKey, runner, time.Now)
}

// requireLocalHost answers 403 unless the request is addressed to the local
// listener as 127.0.0.1 or localhost with its port (D-9, TC-41). A web page
// on another site can give a foreign host name the loopback address and so
// reach this listener from the operator's browser; its requests still carry
// the foreign name, and must not place a paid call (AD-6).
func requireLocalHost(cfg config.Config, next http.Handler) http.Handler {
	loopback := cfg.LocalAddr()
	localhost := net.JoinHostPort(localhostName, strconv.Itoa(cfg.Port))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != loopback && r.Host != localhost {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serve runs every server until ctx ends, the tunnel exits or one of the
// servers fails. It then hangs up the call in progress and shuts all servers
// down.
func serve(ctx context.Context, servers []*http.Server, calls callEnder, tunnelExit <-chan struct{}) error {
	errs := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			log.Printf("checknumber: listening on %s", srv.Addr)
			errs <- srv.ListenAndServe()
		}()
	}
	var err error
	select {
	case <-ctx.Done():
	case <-tunnelExit:
		err = errTunnelExited
	case err = <-errs:
		err = fmt.Errorf("serve: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	// D-6, TC-37: a call left connected keeps costing, so it is hung up
	// first, while both listeners are still open.
	if hangupErr := calls.Shutdown(shutdownCtx); hangupErr != nil {
		err = errors.Join(err, hangupErr)
	}
	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, fmt.Errorf("shut down %s: %w", srv.Addr, shutdownErr))
		}
	}
	return err
}
