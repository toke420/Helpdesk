package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed web
var webFS embed.FS

type App struct {
	cfg    Config
	store  *Store
	hub    *Hub
	mailer *Mailer
	limit  *RateLimiter
	llm    *LLM
	aiq    *aiQueue
	// aiDebounce waits for a customer to finish typing a burst of messages.
	aiDebounce time.Duration
}

func NewApp(cfg Config, store *Store, mailer *Mailer, llm *LLM) *App {
	a := &App{cfg: cfg, store: store, hub: NewHub(), mailer: mailer, limit: NewRateLimiter(),
		llm: llm, aiq: newAIQueue(), aiDebounce: 1500 * time.Millisecond}
	// Tell every open widget when the team goes online or offline.
	a.hub.onChange = func(topic string, active bool) {
		if topic == "agents" {
			a.hub.Publish("visitors", Event{Type: "presence", Data: map[string]bool{"online": active}})
		}
	}
	return a
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.db.PingContext(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	// --- customer widget API (cross-origin, bearer token) ---
	wm := http.NewServeMux()
	wm.HandleFunc("POST /api/widget/session", a.widgetSession)
	wm.HandleFunc("GET /api/widget/conversation", a.visitor(a.widgetConversation))
	wm.HandleFunc("GET /api/widget/cases", a.visitor(a.widgetCases))
	wm.HandleFunc("GET /api/widget/cases/{id}", a.visitor(a.widgetCase))
	wm.HandleFunc("POST /api/widget/messages", a.visitor(a.widgetSend))
	wm.HandleFunc("POST /api/widget/profile", a.visitor(a.widgetProfile))
	wm.HandleFunc("GET /api/widget/stream", a.visitor(a.widgetStream))
	mux.Handle("/api/widget/", a.widgetCORS(wm))

	// --- agent API (same-origin, cookie session) ---
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.agent(a.logout))
	mux.HandleFunc("GET /api/me", a.agent(a.me))
	mux.HandleFunc("PUT /api/me/password", a.agent(a.changePassword))
	mux.HandleFunc("GET /api/conversations", a.agent(a.listConversations))
	mux.HandleFunc("GET /api/conversations/counts", a.agent(a.conversationCounts))
	mux.HandleFunc("GET /api/conversations/{id}", a.agent(a.getConversation))
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.agent(a.agentSend))
	mux.HandleFunc("POST /api/conversations/{id}/status", a.agent(a.setStatus))
	mux.HandleFunc("POST /api/conversations/{id}/assign", a.agent(a.assign))
	mux.HandleFunc("POST /api/conversations/{id}/category", a.agent(a.setCategory))
	mux.HandleFunc("GET /api/cases", a.agent(a.searchCases))
	mux.HandleFunc("POST /api/conversations/{id}/copilot", a.agent(a.copilotAction))
	mux.HandleFunc("POST /api/conversations/{id}/ai", a.agent(a.setAIMode))
	mux.HandleFunc("GET /api/knowledge", a.agent(a.listArticles))
	mux.HandleFunc("GET /api/knowledge/{id}", a.agent(a.getArticle))
	mux.HandleFunc("POST /api/knowledge", a.agent(a.createArticle))
	mux.HandleFunc("PUT /api/knowledge/{id}", a.agent(a.updateArticle))
	mux.HandleFunc("DELETE /api/knowledge/{id}", a.admin(a.deleteArticle))
	mux.HandleFunc("GET /api/tasks", a.agent(a.listTasks))
	mux.HandleFunc("POST /api/tasks", a.agent(a.createTask))
	mux.HandleFunc("POST /api/tasks/reorder", a.agent(a.reorderTasks))
	mux.HandleFunc("PATCH /api/tasks/{id}", a.agent(a.updateTask))
	mux.HandleFunc("DELETE /api/tasks/{id}", a.agent(a.deleteTask))
	mux.HandleFunc("POST /api/conversations/{id}/read", a.agent(a.markRead))
	mux.HandleFunc("GET /api/agents", a.agent(a.listAgents))
	mux.HandleFunc("POST /api/agents", a.admin(a.createAgent))
	mux.HandleFunc("PATCH /api/agents/{id}", a.admin(a.updateAgent))
	mux.HandleFunc("GET /api/settings", a.agent(a.getSettings))
	mux.HandleFunc("PUT /api/settings", a.admin(a.putSettings))
	mux.HandleFunc("GET /api/stream", a.agent(a.agentStream))
	mux.HandleFunc("GET /api/admin/backup", a.admin(a.backup))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})

	// --- static: widget script + inbox app ---
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(sub)
	mux.HandleFunc("GET /widget.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFileFS(w, r, sub, "widget/widget.js")
	})
	mux.HandleFunc("GET /demo", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, "widget/demo.html")
	})
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, sub, "index.html")
	})

	return a.secureHeaders(a.logRequests(mux))
}

// ---------- middleware ----------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int)        { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if strings.HasSuffix(r.URL.Path, "/stream") || r.URL.Path == "/healthz" {
			return
		}
		if sw.status >= 400 || r.Method != http.MethodGet {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (a *App) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if !strings.HasPrefix(r.URL.Path, "/api/widget/") && r.URL.Path != "/widget.js" && r.URL.Path != "/demo" {
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) originAllowed(origin string) bool {
	if origin == "" || origin == a.cfg.BaseOrigin {
		return true
	}
	return slices.Contains(a.cfg.AllowedOrigins, "*") || slices.Contains(a.cfg.AllowedOrigins, origin)
}

func (a *App) widgetCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !a.originAllowed(origin) {
			writeErr(w, http.StatusForbidden, "origin not allowed")
			return
		}
		if origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		limit := 120
		if r.Method == http.MethodPost {
			limit = 30
		}
		if !a.limit.Allow("w:"+r.Method+":"+a.clientIP(r), limit, time.Minute) {
			writeErr(w, http.StatusTooManyRequests, "slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

func (a *App) visitor(h func(http.ResponseWriter, *http.Request, *Contact)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing visitor token")
			return
		}
		c, err := a.store.ContactByToken(r.Context(), hashToken(tok))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown visitor")
			return
		}
		h(w, r, c)
	}
}

func (a *App) agent(h func(http.ResponseWriter, *http.Request, *Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Custom header on writes: browsers can't send it cross-site without
		// a CORS preflight we never approve, so this blocks CSRF.
		if r.Method != http.MethodGet && r.Header.Get("X-Helpdesk") != "1" {
			writeErr(w, http.StatusForbidden, "missing X-Helpdesk header")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		ag, err := a.store.SessionAgent(r.Context(), hashToken(c.Value))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		h(w, r, ag)
	}
}

func (a *App) admin(h func(http.ResponseWriter, *http.Request, *Agent)) http.HandlerFunc {
	return a.agent(func(w http.ResponseWriter, r *http.Request, ag *Agent) {
		if ag.Role != "admin" {
			writeErr(w, http.StatusForbidden, "admins only")
			return
		}
		h(w, r, ag)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	writeErr(w, http.StatusInternalServerError, "something went wrong")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "empty body")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
		}
		return false
	}
	return true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

const maxMessageRunes = 5000

func cleanBody(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "message is empty"
	}
	if !utf8.ValidString(s) {
		return "", "message is not valid text"
	}
	if utf8.RuneCountInString(s) > maxMessageRunes {
		return "", "message is too long (5000 characters max)"
	}
	return s, ""
}

func cleanName(s string) (string, string) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 100 {
		return "", "name is too long"
	}
	return s, ""
}

func cleanEmail(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || len(s) > 254 || !strings.Contains(s[strings.LastIndex(s, "@"):], ".") {
		return "", "that email address doesn't look right"
	}
	return strings.ToLower(s), ""
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
