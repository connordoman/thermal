package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/connordoman/thermal/internal/render/font"
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
	newAdmin := flag.String("new-admin-key", "", "create an admin key with this name, print it and exit (for when every admin key is lost)")
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

	if *newAdmin != "" {
		full, _, err := keys.Create(ctx, *newAdmin, []auth.Scope{auth.ScopeAdmin}, nil, "cli")
		if err != nil {
			console.Fatal("creating key: %v", err)
		}
		fmt.Println(full)
		return
	}
	if err := bootstrap(ctx, db, keys); err != nil {
		console.Fatal("creating the bootstrap key: %v", err)
	}

	target, err := device.ParseConnection(cfg.Printer.Connection)
	if err != nil {
		console.Fatal("%s: %v", settings.EnvEscposConnection, err)
	}
	dev := device.New(device.Config{
		Target:     target,
		VendorID:   cfg.Printer.VendorId,
		ProductID:  cfg.Printer.ProductId,
		USBSerial:  cfg.Printer.USBSerial,
		PaperWidth: cfg.Printer.PaperWidth,
		Timeout:    cfg.Printer.Timeout,
	})
	defer dev.Close()

	q := queue.New(db, dev, cfg.Server.Retention)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		q.Run(ctx)
	}()

	// Parse the Unicode font now rather than during the first request.
	go func() {
		if _, err := font.Load(); err != nil {
			console.Error("loading font: %v", err)
		}
	}()

	srv := &server.Server{
		Store:        db,
		Keys:         keys,
		Device:       dev,
		Queue:        q,
		Images:       render.NewImageLoader(cfg.Server.ImageAllowPrivateHosts, cfg.Server.ImageMaxBytes, cfg.Server.ImageTimeout),
		MaxBodyBytes: cfg.Server.MaxBodyBytes,
		Debug:        cfg.Server.Debug,
	}
	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		console.Info("listening on %s (printer: %s, database: %s)", cfg.Server.Addr, target, cfg.Server.DBPath)
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

// bootstrap creates the first admin key when the database is new (or has no
// usable keys) and prints it once.
func bootstrap(ctx context.Context, db *store.Store, keys *auth.Manager) error {
	n, err := db.CountActiveAPIKeys(ctx)
	if err != nil {
		return err
	}
	if !db.Created && n > 0 {
		return nil
	}
	full, k, err := keys.Create(ctx, "bootstrap", []auth.Scope{auth.ScopeAdmin}, nil, "server")
	if err != nil {
		return err
	}
	_ = db.InsertAuditEvent(ctx, dbq.InsertAuditEventParams{
		At: store.Now(), Action: "key.bootstrap", Target: store.NullString(k.ID),
	})
	line := strings.Repeat("─", 72)
	fmt.Fprintf(os.Stderr, "\n%s\n  Bootstrap admin API key (shown once):\n\n    %s\n\n"+
		"  Use it to create keys for your apps:\n\n"+
		"    curl -X POST http://localhost%s/v1/keys \\\n"+
		"      -H 'Authorization: Bearer %s' \\\n"+
		"      -d '{\"name\":\"my-app\",\"scopes\":[\"print\",\"read\"]}'\n\n"+
		"  Then revoke or rotate it: DELETE /v1/keys/%s once you have another admin key.\n%s\n\n",
		line, full, addrPort(settings.Global.Server.Addr), full, k.ID, line)
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
