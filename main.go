// Claudia is a control plane for a farm of coding agents running in containers.
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/bcrypt"
)

// Config comes from the environment. See compose.yaml.
type Config struct {
	DataDir      string
	Addr         string
	MCPAddr      string
	MCPURL       string // how agents reach the approval endpoint
	Domain       string
	InsecureHTTP bool
	PublicURL    string
	AgentImage   string
	AgentNetwork string
	AgentMemory  string
	AgentCPUs    string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() Config {
	domain := os.Getenv("CLAUDIA_DOMAIN")
	public := os.Getenv("CLAUDIA_PUBLIC_URL")
	if public == "" && domain != "" {
		public = "https://" + domain
	}
	return Config{
		DataDir:      env("CLAUDIA_DATA", "/data"),
		Addr:         env("CLAUDIA_ADDR", ":8080"),
		MCPAddr:      env("CLAUDIA_MCP_ADDR", ":8081"),
		MCPURL:       env("CLAUDIA_MCP_URL", "http://10.77.0.1:8081/mcp"),
		Domain:       domain,
		InsecureHTTP: os.Getenv("CLAUDIA_INSECURE_HTTP") == "1",
		PublicURL:    public,
		AgentImage:   env("CLAUDIA_AGENT_IMAGE", "claudia-agent:latest"),
		AgentNetwork: env("CLAUDIA_AGENT_NETWORK", "claudia-agents"),
		AgentMemory:  env("CLAUDIA_AGENT_MEMORY", "4g"),
		AgentCPUs:    env("CLAUDIA_AGENT_CPUS", "2"),
	}
}

// App is the running control plane.
type App struct {
	cfg  Config
	db   *sql.DB
	key  []byte
	hub  *Hub
	wake chan struct{}

	containerMu sync.Mutex // serialises creating containers

	mu          sync.Mutex // guards the fields below
	runs        map[int64]*liveRun
	waiting     map[int64]chan decision
	loginFails  int
	loginLocked time.Time
}

func newApp(cfg Config) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "claudia.db"))
	if err != nil {
		return nil, err
	}
	key, err := loadKey(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return nil, err
	}
	a := &App{
		cfg: cfg, db: db, key: key, wake: make(chan struct{}, 1),
		hub:     &Hub{subs: map[chan Message]struct{}{}},
		runs:    map[int64]*liveRun{},
		waiting: map[int64]chan decision{},
	}
	if a.setting("session_key") == "" {
		if err := a.setSetting("session_key", randomToken()); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// setupPassword applies CLAUDIA_PASSWORD, or on first start makes up a password and
// logs it once.
func (a *App) setupPassword(password string) error {
	existing := a.setting("password_hash")
	if password == "" {
		if existing != "" {
			return nil
		}
		password = randomToken()[:24]
		log.Printf("No CLAUDIA_PASSWORD set. Generated password, shown once: %s", password)
	}
	if existing != "" && bcrypt.CompareHashAndPassword([]byte(existing), []byte(password)) == nil {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("password: %w", err)
	}
	return a.setSetting("password_hash", string(hash))
}

// reconcile cleans up after a restart. The docker exec clients died with the last
// process, but the CLIs they started may still be running and spending, so those
// containers restart. Work on the volume is untouched, and tasks resume from the
// CLI's saved session.
func (a *App) reconcile(ctx context.Context) error {
	rows, err := a.db.Query(`SELECT DISTINCT agent_id FROM runs WHERE status=?`, RunRunning)
	if err != nil {
		return err
	}
	var agents []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The usage saved during the run stays, flagged as an estimate.
	if _, err := a.db.Exec(`UPDATE runs SET status=?,error=?,usd_state=?,ended_at=? WHERE status=?`,
		RunInterrupted, "The control plane restarted during this run", USDEstimated, now(), RunRunning); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE tasks SET status=?,attempts=MAX(attempts-1,0),updated_at=? WHERE status=?`, TaskQueued, now(), TaskRunning); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE approvals SET status='expired',message='The control plane restarted',decided_at=? WHERE status='pending'`, now()); err != nil {
		return err
	}
	for _, id := range agents {
		a.addEvent(id, 0, Event{Kind: "error", Text: "The control plane restarted during this run"})
		if _, err := docker(ctx, "restart", containerName(id)); err != nil {
			log.Printf("agent %d: restart: %v", id, err)
		}
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg := loadConfig()
	if cfg.Domain == "" && !cfg.InsecureHTTP {
		return errors.New("set CLAUDIA_DOMAIN for HTTPS, or CLAUDIA_INSECURE_HTTP=1 to serve plain HTTP behind a tunnel or proxy")
	}
	a, err := newApp(cfg)
	if err != nil {
		return err
	}
	defer a.db.Close()
	if err := a.setupPassword(os.Getenv("CLAUDIA_PASSWORD")); err != nil {
		return err
	}
	if err := a.seedPrices(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = a.reconcile(startCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("startup cleanup: %w", err)
	}
	go a.scheduler(ctx)
	a.kick()

	// No write timeouts: feeds, terminals and approvals stay open on purpose.
	ui := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	mcp := &http.Server{Addr: cfg.MCPAddr, Handler: a.mcpHandler(), ReadHeaderTimeout: 10 * time.Second}

	errs := make(chan error, 3)
	go func() { errs <- mcp.ListenAndServe() }()
	if cfg.Domain != "" {
		m := &autocert.Manager{
			Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Cache: autocert.DirCache(filepath.Join(cfg.DataDir, "certs")),
		}
		ui.Addr = ":443"
		ui.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}
		go func() { errs <- http.ListenAndServe(":80", m.HTTPHandler(nil)) }()
		go func() { errs <- ui.ListenAndServeTLS("", "") }()
		log.Printf("serving https://%s", cfg.Domain)
	} else {
		ui.Addr = cfg.Addr
		go func() { errs <- ui.ListenAndServe() }()
		log.Printf("serving plain HTTP on %s: keep it behind a tunnel or proxy", cfg.Addr)
	}

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Print("shutting down")
	shutdown, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	ui.Shutdown(shutdown)
	mcp.Shutdown(shutdown)
	return nil
}
