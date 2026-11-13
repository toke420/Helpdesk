package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Addr           string
	DataDir        string
	BaseURL        string // public URL of this service, e.g. https://help.example.com
	BaseOrigin     string
	AllowedOrigins []string
	TrustProxy     bool
	SecureCookies  bool
	AdminEmail     string
	AdminPassword  string
	SMTP           SMTPConfig
	AI             AIConfig
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (Config, error) {
	c := Config{
		Addr:          env("HELPDESK_ADDR", ":8080"),
		DataDir:       env("HELPDESK_DATA_DIR", "./data"),
		BaseURL:       strings.TrimRight(env("HELPDESK_BASE_URL", "http://localhost:8080"), "/"),
		TrustProxy:    env("HELPDESK_TRUST_PROXY", "false") == "true",
		AdminEmail:    env("HELPDESK_ADMIN_EMAIL", ""),
		AdminPassword: os.Getenv("HELPDESK_ADMIN_PASSWORD"),
		SMTP: SMTPConfig{
			Host:     env("SMTP_HOST", ""),
			Port:     env("SMTP_PORT", "587"),
			Username: env("SMTP_USERNAME", ""),
			Password: os.Getenv("SMTP_PASSWORD"),
			From:     env("SMTP_FROM", ""),
		},
		AI: AIConfig{
			Provider: env("AI_PROVIDER", "anthropic"),
			APIKey:   os.Getenv("AI_API_KEY"),
			Model:    env("AI_MODEL", ""),
			BaseURL:  env("AI_BASE_URL", ""),
		},
	}
	if c.AI.Provider != "anthropic" && c.AI.Provider != "openai" {
		return c, fmt.Errorf("AI_PROVIDER must be anthropic or openai (openai = any OpenAI-compatible API, including Ollama)")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return c, fmt.Errorf("HELPDESK_BASE_URL must be a full URL like https://help.example.com")
	}
	c.BaseOrigin = u.Scheme + "://" + u.Host
	c.SecureCookies = u.Scheme == "https"
	for _, o := range strings.Split(env("HELPDESK_ALLOWED_ORIGINS", ""), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	return c, nil
}

func main() {
	// `helpdesk healthcheck` lets the distroless image run a Docker HEALTHCHECK
	// without curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "helpdesk.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	store := &Store{db: db}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx); err != nil {
		return err
	}
	if err := bootstrapAdmin(ctx, store, cfg); err != nil {
		return err
	}

	if err := store.BackfillCaseIndex(ctx); err != nil {
		slog.Warn("couldn't index past cases for Copilot", "err", err)
	}

	mailer := NewMailer(cfg.SMTP)
	llm := NewLLM(cfg.AI)
	app := NewApp(cfg, store, mailer, llm)

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				store.PurgeExpiredSessions(ctx)
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Request contexts end on shutdown, which closes open event streams.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("helpdesk listening", "addr", cfg.Addr, "base_url", cfg.BaseURL,
		"widget_origins", cfg.AllowedOrigins, "email", mailer.Enabled(),
		"ai", llm.Enabled(), "ai_provider", llm.cfg.Provider, "ai_model", llm.cfg.Model)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}
	return nil
}

// bootstrapAdmin creates the first admin when the agents table is empty.
func bootstrapAdmin(ctx context.Context, s *Store, cfg Config) error {
	n, err := s.CountAgents(ctx)
	if err != nil || n > 0 {
		return err
	}
	email, pw := strings.ToLower(cfg.AdminEmail), cfg.AdminPassword
	if email == "" {
		email = "admin@example.com"
	}
	generated := pw == ""
	if generated {
		pw = newToken()[:20]
	} else if err := validPassword(pw); err != nil {
		return fmt.Errorf("HELPDESK_ADMIN_PASSWORD: %w", err)
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	if _, err := s.CreateAgent(ctx, email, "Admin", "admin", h); err != nil {
		return err
	}
	if generated {
		slog.Warn("created first admin — sign in and change this password", "email", email, "password", pw)
	} else {
		slog.Info("created first admin", "email", email)
	}
	return nil
}

func healthcheck() int {
	addr := env("HELPDESK_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}
