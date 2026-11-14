package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------- auth ----------

func (a *App) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cfg.SecureCookies,
	})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Helpdesk") != "1" {
		writeErr(w, http.StatusForbidden, "missing X-Helpdesk header")
		return
	}
	if !a.limit.Allow("login:"+a.clientIP(r), 10, 10*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts — wait a few minutes")
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ag, err := a.store.GetAgentByEmail(r.Context(), strings.TrimSpace(req.Email))
	if err != nil && !errors.Is(err, errNotFound) {
		serverError(w, err)
		return
	}
	if ag == nil {
		checkPassword(dummyHash, req.Password)
		writeErr(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	if !checkPassword(ag.hash, req.Password) || ag.Disabled {
		writeErr(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	tok := newToken()
	if err := a.store.CreateSession(r.Context(), hashToken(tok), ag.ID, time.Now().Add(sessionTTL)); err != nil {
		serverError(w, err)
		return
	}
	a.setSessionCookie(w, tok, int(sessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"agent": ag})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request, ag *Agent) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	a.setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) me(w http.ResponseWriter, r *http.Request, ag *Agent) {
	writeJSON(w, http.StatusOK, map[string]any{"agent": ag, "email_enabled": a.mailer.Enabled(), "base_url": a.cfg.BaseURL,
		"ntfy_enabled": a.cfg.Ntfy.Enabled(), "notify_email": a.cfg.NotifyEmail, "ai": a.llm.Status()})
}

func (a *App) changePassword(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
		// Only on the first-sign-in screen: replace the placeholder name/email.
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !checkPassword(ag.hash, req.Current) {
		writeErr(w, http.StatusBadRequest, "current password is wrong")
		return
	}
	if err := validPassword(req.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.New == req.Current {
		writeErr(w, http.StatusBadRequest, "choose a new password, not the current one")
		return
	}
	h, err := hashPassword(req.New)
	if err != nil {
		serverError(w, err)
		return
	}
	done := false
	u := AgentUpdate{Hash: &h, MustChangePassword: &done}
	if ag.MustChangePassword {
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" || len(name) > 100 {
				writeErr(w, http.StatusBadRequest, "enter your name")
				return
			}
			u.Name = &name
		}
		if req.Email != nil {
			email, problem := cleanEmail(*req.Email)
			if problem == "" && email == "" {
				problem = "email is required"
			}
			if problem != "" {
				writeErr(w, http.StatusBadRequest, problem)
				return
			}
			email = strings.ToLower(email)
			u.Email = &email
		}
	}
	if err := a.store.UpdateAgent(r.Context(), ag.ID, u); errors.Is(err, errDuplicate) {
		writeErr(w, http.StatusBadRequest, "another agent already uses that email")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	// Sign out every other session.
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteAgentSessions(r.Context(), ag.ID, hashToken(c.Value))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- conversations ----------

func (a *App) listConversations(w http.ResponseWriter, r *http.Request, ag *Agent) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	convs, err := a.store.ListConversations(r.Context(), q.Get("view"), ag.ID, before, 50, truncate(q.Get("q"), 100))
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown view") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		serverError(w, err)
		return
	}
	for _, c := range convs {
		c.Contact.Online = a.hub.Active(topicContact(c.ContactID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": convs})
}

func (a *App) conversationCounts(w http.ResponseWriter, r *http.Request, ag *Agent) {
	counts, err := a.store.ViewCounts(r.Context(), ag.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (a *App) loadConversation(w http.ResponseWriter, r *http.Request) (*Conversation, bool) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return nil, false
	}
	conv, err := a.store.GetConversation(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return nil, false
	}
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	conv.Contact.Online = a.hub.Active(topicContact(conv.ContactID))
	return conv, true
}

func (a *App) getConversation(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	msgs, err := a.store.ListMessages(r.Context(), conv.ID, true)
	if err != nil {
		serverError(w, err)
		return
	}
	history, err := a.store.ContactConversations(r.Context(), conv.ContactID, conv.Contact.Email)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": conv, "messages": msgs, "history": history})
}

func (a *App) broadcastConversation(r *http.Request, id int64) {
	if conv, err := a.store.GetConversation(r.Context(), id); err == nil {
		conv.Contact.Online = a.hub.Active(topicContact(conv.ContactID))
		a.hub.Publish("agents", Event{Type: "conversation", Data: conv})
		a.hub.Publish(topicContact(conv.ContactID), Event{Type: "case", Data: toWidgetCase(conv)})
	}
}

func (a *App) agentSend(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
		Note bool   `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	body, problem := cleanBody(req.Body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	msg, err := a.store.AddAgentMessage(r.Context(), conv.ID, ag, body, req.Note)
	if err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "message", Data: msg})
	if !req.Note {
		a.hub.Publish(topicContact(conv.ContactID), Event{Type: "message", Data: msg})
		// Customer isn't on the site: email them the reply.
		if !a.hub.Active(topicContact(conv.ContactID)) && conv.Contact.Email != "" {
			settings, _ := a.store.Settings(r.Context())
			brand := settings["brand_name"]
			link := conv.Contact.LastPage
			if link == "" {
				link = a.cfg.BaseURL
			}
			a.mailer.Notify(fmt.Sprintf("contact:%d", conv.ContactID), conv.Contact.Email,
				fmt.Sprintf("New reply from %s (case %s)", brand, conv.Number),
				fmt.Sprintf("%s from %s replied:\n\n%s\n\nTo continue the conversation, open the chat on %s\n\nYour case number is %s. Mention it if you contact us again about this.\n",
					ag.Name, brand, body, link, conv.Number))
		}
	}
	a.broadcastConversation(r, conv.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"message": msg})
}

var caseStateLabels = map[string]string{
	"open": "Open", "in_progress": "In Progress", "pending": "Awaiting Customer",
	"pending_vendor": "Awaiting Manufacturer", "awaiting_approval": "Awaiting Approval",
	"scheduled": "Scheduled", "on_hold": "On Hold", "closed": "Closed",
}

// customerStateLabels overrides caseStateLabels where the agent wording
// reads oddly to the customer.
var customerStateLabels = map[string]string{
	"pending": "Waiting for your reply",
}

// caseStateLabel is the full name of a case's state ("Awaiting Approval")
// as the customer sees it in the widget.
func caseStateLabel(c *Conversation) string {
	key := c.Status
	if c.SubStatus != "" {
		key = c.SubStatus
	}
	if l, ok := customerStateLabels[key]; ok {
		return l
	}
	if l, ok := caseStateLabels[key]; ok {
		return l
	}
	return c.Status
}

func (a *App) setStatus(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	st, known := CaseStates[req.Status]
	if !known {
		writeErr(w, http.StatusBadRequest, "unknown status")
		return
	}
	if st[0] == conv.Status && st[1] == conv.SubStatus {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := a.store.SetStatus(r.Context(), conv.ID, req.Status); err != nil {
		serverError(w, err)
		return
	}
	if req.Status == "closed" && conv.Status != "closed" {
		// Resolved cases become examples Copilot can learn from.
		if err := a.store.IndexCase(r.Context(), conv.ID); err != nil {
			slog.Warn("couldn't index case", "id", conv.ID, "err", err)
		}
	}
	text := fmt.Sprintf("%s set the status to %s", ag.Name, caseStateLabels[req.Status])
	switch {
	case req.Status == "closed":
		text = ag.Name + " closed the conversation"
	case conv.Status == "closed":
		text = fmt.Sprintf("%s reopened the conversation as %s", ag.Name, caseStateLabels[req.Status])
	}
	if note, err := a.store.AddSystemNote(r.Context(), conv.ID, text); err == nil {
		a.hub.Publish("agents", Event{Type: "message", Data: note})
	}
	a.broadcastConversation(r, conv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) assign(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	var req struct {
		AgentID *int64 `json:"agent_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := ag.Name + " unassigned the conversation"
	if req.AgentID != nil {
		target, err := a.store.GetAgent(r.Context(), *req.AgentID)
		if errors.Is(err, errNotFound) || (err == nil && target.Disabled) {
			writeErr(w, http.StatusBadRequest, "no such agent")
			return
		} else if err != nil {
			serverError(w, err)
			return
		}
		if target.ID == ag.ID {
			text = ag.Name + " took the conversation"
		} else {
			text = fmt.Sprintf("%s assigned the conversation to %s", ag.Name, target.Name)
		}
	}
	if err := a.store.SetAssignee(r.Context(), conv.ID, req.AgentID); err != nil {
		serverError(w, err)
		return
	}
	if note, err := a.store.AddSystemNote(r.Context(), conv.ID, text); err == nil {
		a.hub.Publish("agents", Event{Type: "message", Data: note})
	}
	a.broadcastConversation(r, conv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) setCategory(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	var req struct {
		Category string `json:"category"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Category = strings.TrimSpace(req.Category)
	if req.Category == conv.Category {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if req.Category != "" {
		settings, err := a.store.Settings(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		if !slices.Contains(Categories(settings), req.Category) {
			writeErr(w, http.StatusBadRequest, "unknown category — add it in Settings first")
			return
		}
	}
	if err := a.store.SetCategory(r.Context(), conv.ID, req.Category); err != nil {
		serverError(w, err)
		return
	}
	text := fmt.Sprintf("%s set the category to %s", ag.Name, req.Category)
	if req.Category == "" {
		text = ag.Name + " cleared the category"
	}
	if note, err := a.store.AddSystemNote(r.Context(), conv.ID, text); err == nil {
		a.hub.Publish("agents", Event{Type: "message", Data: note})
	}
	a.broadcastConversation(r, conv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) searchCases(w http.ResponseWriter, r *http.Request, ag *Agent) {
	q := r.URL.Query()
	num := func(k string) int64 { n, _ := strconv.ParseInt(q.Get(k), 10, 64); return n }
	f := CaseFilter{
		Query:     truncate(q.Get("q"), 200),
		Status:    q.Get("status"),
		Assignee:  q.Get("assignee"),
		Category:  q.Get("category"),
		ContactID: num("contact"),
		Since:     num("since"),
		Before:    num("before"),
		Limit:     100,
	}
	cases, stats, err := a.store.SearchCases(r.Context(), f, ag.ID)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown ") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		serverError(w, err)
		return
	}
	var next int64
	if len(cases) == f.Limit {
		next = cases[len(cases)-1].ID
	}
	for _, c := range cases {
		c.Contact.Online = a.hub.Active(topicContact(c.ContactID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": cases, "stats": stats, "next_before": next})
}

func (a *App) markRead(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := a.store.MarkRead(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "read", Data: map[string]int64{"id": id}})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) agentStream(w http.ResponseWriter, r *http.Request, ag *Agent) {
	// Each open inbox says which device it is and whether it's on screen, so
	// we know whether someone will hear the sound (notify.go).
	q := r.URL.Query()
	client := truncate(q.Get("client"), 64)
	if client == "" {
		client = newToken()
	}
	a.pres.connect(ag.ID, client, q.Get("visible") != "0", q.Get("mobile") == "1")
	a.presenceChanged()
	defer func() {
		a.pres.disconnect(ag.ID, client)
		a.presenceChanged()
	}()
	a.serveSSE(w, r, []Event{{Type: "hello", Data: map[string]int64{"agent_id": ag.ID}}}, "agents", topicAgent(ag.ID))
}

// ---------- team ----------

func (a *App) listAgents(w http.ResponseWriter, r *http.Request, ag *Agent) {
	agents, err := a.store.ListAgents(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	for _, x := range agents {
		x.Online = a.pres.connected(x.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (a *App) createAgent(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	email, problem := cleanEmail(req.Email)
	if problem == "" && email == "" {
		problem = "email is required"
	}
	name, p2 := cleanName(req.Name)
	if problem == "" {
		problem = p2
	}
	if problem == "" && name == "" {
		problem = "name is required"
	}
	if problem == "" && req.Role != "admin" && req.Role != "agent" {
		problem = "role must be admin or agent"
	}
	if problem == "" {
		if err := validPassword(req.Password); err != nil {
			problem = err.Error()
		}
	}
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	h, err := hashPassword(req.Password)
	if err != nil {
		serverError(w, err)
		return
	}
	created, err := a.store.CreateAgent(r.Context(), email, name, req.Role, h)
	if errors.Is(err, errDuplicate) {
		writeErr(w, http.StatusConflict, "an agent with that email already exists")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	// The admin chose this password and has seen it: the new agent replaces
	// it with their own on first sign-in before anything else works.
	must := true
	if err := a.store.UpdateAgent(r.Context(), created.ID, AgentUpdate{MustChangePassword: &must}); err != nil {
		serverError(w, err)
		return
	}
	created.MustChangePassword = true
	writeJSON(w, http.StatusCreated, map[string]any{"agent": created})
}

func (a *App) updateAgent(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var req struct {
		Name     *string `json:"name"`
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	target, err := a.store.GetAgent(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	u := AgentUpdate{Disabled: req.Disabled}
	if req.Name != nil {
		n, problem := cleanName(*req.Name)
		if problem == "" && n == "" {
			problem = "name is required"
		}
		if problem != "" {
			writeErr(w, http.StatusBadRequest, problem)
			return
		}
		u.Name = &n
	}
	if req.Role != nil {
		if *req.Role != "admin" && *req.Role != "agent" {
			writeErr(w, http.StatusBadRequest, "role must be admin or agent")
			return
		}
		u.Role = req.Role
	}
	// Never lock the team out: keep at least one active admin.
	demoting := target.Role == "admin" && !target.Disabled &&
		((req.Role != nil && *req.Role != "admin") || (req.Disabled != nil && *req.Disabled))
	if demoting {
		n, err := a.store.CountActiveAdmins(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		if n <= 1 {
			writeErr(w, http.StatusBadRequest, "you need at least one active admin")
			return
		}
	}
	if req.Password != nil {
		if err := validPassword(*req.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		h, err := hashPassword(*req.Password)
		if err != nil {
			serverError(w, err)
			return
		}
		u.Hash = &h
		// A reset password is temporary too: it must be replaced on next sign-in.
		must := true
		u.MustChangePassword = &must
	}
	if err := a.store.UpdateAgent(r.Context(), id, u); err != nil {
		serverError(w, err)
		return
	}
	if u.Hash != nil || (req.Disabled != nil && *req.Disabled) {
		a.store.DeleteAgentSessions(r.Context(), id, "")
	}
	updated, _ := a.store.GetAgent(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"agent": updated})
}

// ---------- settings ----------

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (a *App) getSettings(w http.ResponseWriter, r *http.Request, ag *Agent) {
	s, err := a.store.Settings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s})
}

func (a *App) putSettings(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req map[string]string
	if !readJSON(w, r, &req) {
		return
	}
	vals := map[string]string{}
	for k, v := range req {
		if _, known := defaultSettings[k]; !known {
			writeErr(w, http.StatusBadRequest, "unknown setting "+k)
			return
		}
		v = strings.TrimSpace(v)
		switch k {
		case "accent_color":
			if !hexColor.MatchString(v) {
				writeErr(w, http.StatusBadRequest, "accent colour must look like #2f6fed")
				return
			}
		case "brand_name":
			if v == "" || len([]rune(v)) > 60 {
				writeErr(w, http.StatusBadRequest, "brand name must be 1–60 characters")
				return
			}
		case "ai_agent":
			if v != "off" && v != "away" && v != "always" {
				writeErr(w, http.StatusBadRequest, "AI agent must be off, away or always")
				return
			}
		case "ai_bot_name":
			if v == "" || len([]rune(v)) > 40 {
				writeErr(w, http.StatusBadRequest, "AI agent name must be 1–40 characters")
				return
			}
		case "feedback_enabled", "feedback_approval":
			if v != "0" && v != "1" {
				writeErr(w, http.StatusBadRequest, k+" must be 0 or 1")
				return
			}
		case "feedback_title":
			if v == "" || len([]rune(v)) > 60 {
				writeErr(w, http.StatusBadRequest, "board title must be 1–60 characters")
				return
			}
		case "ai_max_replies":
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 20 {
				writeErr(w, http.StatusBadRequest, "AI replies per case must be between 1 and 20")
				return
			}
		case "ai_instructions":
			if len([]rune(v)) > 2000 {
				writeErr(w, http.StatusBadRequest, "AI guidance is too long (2,000 characters max)")
				return
			}
		case "categories":
			seen, list := map[string]bool{}, []string{}
			for _, c := range Categories(map[string]string{"categories": v}) {
				c = strings.Join(strings.Fields(c), " ")
				if len([]rune(c)) > 40 {
					writeErr(w, http.StatusBadRequest, "each category must be 40 characters or fewer")
					return
				}
				if !seen[strings.ToLower(c)] {
					seen[strings.ToLower(c)] = true
					list = append(list, c)
				}
			}
			if len(list) > 30 {
				writeErr(w, http.StatusBadRequest, "keep it to 30 categories or fewer")
				return
			}
			v = strings.Join(list, "\n")
		default:
			if len([]rune(v)) > 500 {
				writeErr(w, http.StatusBadRequest, k+" is too long")
				return
			}
		}
		vals[k] = v
	}
	if err := a.store.PutSettings(r.Context(), vals); err != nil {
		serverError(w, err)
		return
	}
	a.getSettings(w, r, ag)
}

// backup streams a consistent copy of the database.
func (a *App) backup(w http.ResponseWriter, r *http.Request, ag *Agent) {
	dir, err := os.MkdirTemp(a.cfg.DataDir, "backup-")
	if err != nil {
		serverError(w, err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "helpdesk.db")
	if err := a.store.Backup(r.Context(), path); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="helpdesk-%s.db"`, time.Now().Format("2006-01-02")))
	http.ServeFile(w, r, path)
}
