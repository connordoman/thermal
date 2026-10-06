package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/connordoman/escpos/unifont"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // datetime blocks can name any time zone

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/device"
	"github.com/connordoman/thermal/internal/queue"
	"github.com/connordoman/thermal/internal/render"
	"github.com/connordoman/thermal/internal/server"
	"github.com/connordoman/thermal/internal/settings"
	"github.com/connordoman/thermal/internal/store"
	"github.com/connordoman/thermal/internal/store/dbq"
	"github.com/joho/godotenv"
)

func init() {
	if err := godotenv.Load(); err != nil && errors.Is(err, fs.ErrNotExist) {
		console.Warn("no .env or .env.local file found")
		log.Println(err)
	} else if err != nil {
		console.Error("failed to load .env or .env.local file: %v", err)
	}

	if errs := settings.Load(); len(errs) > 0 {
		console.Fatal("failed to load settings: %d error(s): %v", len(errs), errs)
	}
	if errs := settings.Validate(); len(errs) > 0 {
		console.Fatal("invalid settings: %d error(s): %v", len(errs), errs)
	}
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "exit 0 if the server on THERMAL_ADDR is healthy, 1 otherwise (for container health checks)")
	newAdmin := flag.String("new-admin-key", "", "create an admin key with this name, print it and exit")
	resetRoot := flag.Bool("reset-root-password", false, "give the root user a new random password, print it and exit")
	flag.Parse()

	cfg := settings.Global
	console.SetLogLevel(console.LogLevelDebug, cfg.Server.Debug)
	if *healthcheck {
		os.Exit(checkHealth(cfg.Server.Addr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Server.DBPath)
	if err != nil {
		console.Fatal("opening database %s: %v", cfg.Server.DBPath, err)
	}
	defer db.Close()
	keys := auth.NewManager(db.Queries)
	users := auth.NewUsers(db.Queries, cfg.Server.SessionTTL)

	if *newAdmin != "" {
		full, _, err := keys.Create(ctx, *newAdmin, []auth.Scope{auth.ScopeAdmin}, nil, "cli")
		if err != nil {
			console.Fatal("creating key: %v", err)
		}
		fmt.Println(full)
		return
	}
	if err := ensureRoot(ctx, db, users); err != nil {
		console.Fatal("creating the root user: %v", err)
	}
	if *resetRoot {
		u, password, err := users.ResetRootPassword(ctx)
		if err != nil {
			console.Fatal("resetting the root password: %v", err)
		}
		_ = db.InsertAuditEvent(ctx, dbq.InsertAuditEventParams{
			At: store.Now(), Action: "user.password_reset", Target: store.NullString(u.Username),
		})
		fmt.Fprintf(os.Stderr, "New password for %s:\n", u.Username)
		fmt.Println(password)
		return
	}

	dev, err := device.New(device.Config{
		Connection: cfg.Printer.Connection,
		VendorID:   cfg.Printer.VendorId,
		ProductID:  cfg.Printer.ProductId,
		USBSerial:  cfg.Printer.USBSerial,
		PaperWidth: cfg.Printer.PaperWidth,
		Timeout:    cfg.Printer.Timeout,
	})
	if err != nil {
		console.Fatal("%s: %v", settings.EnvEscposConnection, err)
	}
	defer dev.Close()

	q := queue.New(db, dev, cfg.Server.Retention)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		q.Run(ctx)
	}()

	// Parse the Unicode font now rather than during the first request.
	go func() {
		if _, err := unifont.Load(); err != nil {
			console.Error("loading font: %v", err)
		}
	}()

	srv := &server.Server{
		Store:        db,
		Keys:         keys,
		Users:        users,
		Device:       dev,
		Queue:        q,
		Images:       render.NewImageLoader(cfg.Server.ImageAllowPrivateHosts, cfg.Server.ImageMaxBytes, cfg.Server.ImageTimeout),
		MaxBodyBytes: cfg.Server.MaxBodyBytes,
		Debug:        cfg.Server.Debug,
		CutFeed:      cfg.Server.CutFeed,

		SessionSecure:  cfg.Server.SessionSecure,
		TrustedOrigins: cfg.Server.TrustedOrigins,
	}
	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		console.Info("listening on %s (printer: %s, database: %s)", cfg.Server.Addr, dev.ConnectionString(), cfg.Server.DBPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			console.Fatal("serving: %v", err)
		}
	}()

	<-ctx.Done()
	console.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(sctx); err != nil {
		console.Warn("closing connections: %v", err)
	}
	select {
	case <-workerDone:
	case <-sctx.Done():
		console.Warn("gave up waiting for the job in progress")
	}
}

// ensureRoot creates the root user the first time the server starts (or
// the first time after upgrading to a version with user accounts). Like a
// database server's superuser, its name and password come from the
// environment; a password that was not given is generated and printed once.
func ensureRoot(ctx context.Context, db *store.Store, users *auth.Users) error {
	cfg := settings.Global.Server
	u, generated, created, err := users.EnsureRoot(ctx, cfg.RootUser, cfg.RootPassword)
	if err != nil || !created {
		return err
	}
	_ = db.InsertAuditEvent(ctx, dbq.InsertAuditEventParams{
		At: store.Now(), Action: "user.created", Target: store.NullString(u.Username), Detail: store.NullString(`{"root":true}`),
	})
	if generated == "" {
		console.Info("created root user %q with the password from the environment", u.Username)
		return nil
	}
	line := strings.Repeat("─", 72)
	fmt.Fprintf(os.Stderr, "\n%s\n  Root user created (password shown once):\n\n"+
		"    username: %s\n    password: %s\n\n"+
		"  Sign in to the web UI with it, or start a session from the command line:\n\n"+
		"    curl -c cookies.txt -X POST http://localhost%s/v1/session \\\n"+
		"      -H 'Content-Type: application/json' \\\n"+
		"      -d '{\"username\":\"%s\",\"password\":\"...\"}'\n\n"+
		"  Set %s before the first start to choose the password instead.\n"+
		"  Lost it? Run: thermal -reset-root-password\n%s\n\n",
		line, u.Username, generated, addrPort(cfg.Addr), u.Username, settings.EnvRootPassword, line)
	return nil
}

// checkHealth asks the running server's /healthz whether it is healthy.
func checkHealth(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

func addrPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
