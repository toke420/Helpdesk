package main

// Agent availability, Away mode, and what happens when a customer writes:
//
//   - Someone is available (the inbox is open and on screen, or open on a
//     computer, and they're not set to Away): the open inbox plays a sound.
//   - Everyone is Away, or nobody has the inbox open: an ntfy notification
//     (NTFY_URL) with the customer's message.
//   - ntfy isn't set up or fails: an email to NOTIFY_EMAIL with the message.
//     Lines typed in quick succession are collected into one email.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------- presence ----------

type agentConn struct {
	n       int // open streams with this client id (reconnects overlap briefly)
	visible bool
	mobile  bool
}

type presence struct {
	mu         sync.Mutex
	conns      map[int64]map[string]*agentConn // agent id → client id → connection
	away       map[int64]bool
	lastOnline bool
}

func newPresence() *presence {
	return &presence{conns: map[int64]map[string]*agentConn{}, away: map[int64]bool{}}
}

func (p *presence) connect(agentID int64, client string, visible, mobile bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[agentID] == nil {
		p.conns[agentID] = map[string]*agentConn{}
	}
	c := p.conns[agentID][client]
	if c == nil {
		c = &agentConn{}
		p.conns[agentID][client] = c
	}
	c.n++
	c.visible, c.mobile = visible, mobile
}

func (p *presence) disconnect(agentID int64, client string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conns[agentID][client]
	if c == nil {
		return
	}
	if c.n--; c.n <= 0 {
		delete(p.conns[agentID], client)
		if len(p.conns[agentID]) == 0 {
			delete(p.conns, agentID)
		}
	}
}

// setVisible records whether the inbox on that device is on screen.
func (p *presence) setVisible(agentID int64, client string, visible bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conns[agentID][client]
	if c == nil {
		return false
	}
	c.visible = visible
	return true
}

func (p *presence) setAway(agentID int64, away bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if away {
		p.away[agentID] = true
	} else {
		delete(p.away, agentID)
	}
}

func (p *presence) connected(agentID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns[agentID]) > 0
}

// availableLocked: not Away, and the inbox is on screen somewhere or open on a
// computer (a background tab on a computer still plays the sound; a phone with
// the screen off or the app in the background doesn't).
func (p *presence) availableLocked(agentID int64) bool {
	if p.away[agentID] {
		return false
	}
	for _, c := range p.conns[agentID] {
		if c.visible || !c.mobile {
			return true
		}
	}
	return false
}

func (p *presence) anyAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.conns {
		if p.availableLocked(id) {
			return true
		}
	}
	return false
}

// presenceChanged tells every open widget when the team becomes available or
// unavailable.
func (a *App) presenceChanged() {
	online := a.pres.anyAvailable()
	a.pres.mu.Lock()
	changed := online != a.pres.lastOnline
	a.pres.lastOnline = online
	a.pres.mu.Unlock()
	if changed {
		a.hub.Publish("visitors", Event{Type: "presence", Data: map[string]bool{"online": online}})
	}
}

func (a *App) loadAway(ctx context.Context) error {
	away, err := a.store.AwayAgents(ctx)
	if err != nil {
		return err
	}
	for id := range away {
		a.pres.setAway(id, true)
	}
	return nil
}

// ---------- ntfy ----------

type NtfyConfig struct {
	Server string // e.g. https://ntfy.sh
	Topic  string
	Token  string // optional access token for protected topics
}

func (n NtfyConfig) Enabled() bool { return n.Server != "" && n.Topic != "" }

// parseNtfyURL splits https://ntfy.sh/my-topic into server and topic.
func parseNtfyURL(raw string) (NtfyConfig, error) {
	if raw == "" {
		return NtfyConfig{}, nil
	}
	u, err := url.Parse(raw)
	topic := ""
	if err == nil {
		topic = strings.Trim(u.Path, "/")
	}
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || topic == "" || strings.Contains(topic, "/") {
		return NtfyConfig{}, errors.New("NTFY_URL must look like https://ntfy.sh/your-topic")
	}
	return NtfyConfig{Server: u.Scheme + "://" + u.Host, Topic: topic}, nil
}

type ntfyMessage struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
}

// ntfySend publishes one notification. Swappable for tests.
func (a *App) ntfySend(ctx context.Context, title, message, click string) error {
	n := a.cfg.Ntfy
	body, _ := json.Marshal(ntfyMessage{
		Topic: n.Topic, Title: title, Message: message,
		Priority: 4, Tags: []string{"speech_balloon"}, Click: click,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Server+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ---------- routing a new customer message ----------

func contactLabel(c *Contact) string {
	switch {
	case c.Name != "" && c.Email != "":
		return fmt.Sprintf("%s <%s>", c.Name, c.Email)
	case c.Name != "":
		return c.Name
	case c.Email != "":
		return c.Email
	}
	return "A customer"
}

// Gather lines typed in quick succession into one email.
const emailDelay = time.Minute

// notifyNewMessage runs in the background after a customer message is saved.
func (a *App) notifyNewMessage(conv *Conversation, contact *Contact, msg *Message) {
	if a.pres.anyAvailable() {
		return // the open inbox plays the sound
	}
	who := contactLabel(contact)
	link := fmt.Sprintf("%s/#/c/%d", a.cfg.BaseURL, conv.ID)

	if a.cfg.Ntfy.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		title := fmt.Sprintf("%s · New message from %s", conv.Number, who)
		body := msg.Body
		if conv.RelatedNumber != "" {
			body += "\n\n(Follows " + conv.RelatedNumber + ")"
		}
		err := a.notify(ctx, title, body, link)
		cancel()
		if err == nil {
			return
		}
		slog.Error("ntfy failed, falling back to email", "err", err)
	}
	a.queueEmail(conv.ID, msg.CreatedAt)
}

// queueEmail sends one email per conversation per burst, with every line the
// customer wrote in it — unless someone replies first.
func (a *App) queueEmail(convID, since int64) {
	if !a.mailer.Enabled() || a.cfg.NotifyEmail == "" {
		slog.Warn("nobody available and no ntfy or email set up: message only in the inbox", "conversation", convID)
		return
	}
	a.emailMu.Lock()
	if _, pending := a.emailPending[convID]; pending {
		a.emailMu.Unlock()
		return // the queued email will include this line
	}
	a.emailPending[convID] = since
	a.emailMu.Unlock()

	time.AfterFunc(a.emailDelay, func() {
		a.emailMu.Lock()
		delete(a.emailPending, convID)
		a.emailMu.Unlock()
		a.sendQueuedEmail(convID, since)
	})
}

func (a *App) sendQueuedEmail(convID, since int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conv, err := a.store.GetConversation(ctx, convID)
	if err != nil {
		slog.Error("notify email: load conversation", "err", err)
		return
	}
	msgs, err := a.store.ListMessages(ctx, convID, false)
	if err != nil {
		slog.Error("notify email: load messages", "err", err)
		return
	}
	var lines []string
	for _, m := range msgs {
		if m.CreatedAt < since {
			continue
		}
		if m.AuthorType == "agent" {
			return // someone already answered
		}
		if m.AuthorType == "contact" {
			lines = append(lines, m.Body)
		}
	}
	if len(lines) == 0 {
		return
	}
	who := contactLabel(conv.Contact)
	var b strings.Builder
	fmt.Fprintf(&b, "%s wrote on the website chat:\n\n", who)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n\n")
	}
	if conv.Contact.LastPage != "" {
		fmt.Fprintf(&b, "Page: %s\n", conv.Contact.LastPage)
	}
	if conv.Contact.Email != "" {
		fmt.Fprintf(&b, "Their email: %s (replies from the inbox are emailed to them once they've left the site)\n", conv.Contact.Email)
	} else {
		b.WriteString("They didn't leave an email address — reply in the inbox and they'll see it on their next visit.\n")
	}
	if conv.RelatedNumber != "" {
		how := map[string]string{"number": "the customer entered the case number", "unsure": "the customer wasn't sure, so we matched their last case"}[conv.RelatedVia]
		fmt.Fprintf(&b, "Follows earlier case %s (%s).\n", conv.RelatedNumber, how)
	}
	fmt.Fprintf(&b, "\nCase %s · Reply: %s/#/c/%d\n", conv.Number, a.cfg.BaseURL, convID)
	a.mailer.SendNow(a.cfg.NotifyEmail, fmt.Sprintf("Website chat %s: %s", conv.Number, preview(lines[0])), b.String())
}

// ---------- agent endpoints ----------

func (a *App) setAway(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Away bool `json:"away"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.store.SetAway(r.Context(), ag.ID, req.Away); err != nil {
		serverError(w, err)
		return
	}
	a.pres.setAway(ag.ID, req.Away)
	a.presenceChanged()
	ag.Away = req.Away
	ag.Online = a.pres.connected(ag.ID)
	a.hub.Publish("agents", Event{Type: "agent", Data: ag})
	writeJSON(w, http.StatusOK, map[string]any{"agent": ag})
}

// setPresence: the inbox reports when it goes on or off screen.
func (a *App) setPresence(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Client  string `json:"client"`
		Visible bool   `json:"visible"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if a.pres.setVisible(ag.ID, truncate(req.Client, 64), req.Visible) {
		a.presenceChanged()
	}
	w.WriteHeader(http.StatusNoContent)
}

// notifyTest (admins) sends a test ntfy notification and a test email.
func (a *App) notifyTest(w http.ResponseWriter, r *http.Request, ag *Agent) {
	if !a.limit.Allow(fmt.Sprintf("notifytest:%d", ag.ID), 5, 10*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	res := map[string]string{"ntfy": "not set up", "email": "not set up"}
	if a.cfg.Ntfy.Enabled() {
		if err := a.notify(r.Context(), "Help desk test", "Notifications are working. New chat messages will arrive like this when everyone is Away or signed out.", a.cfg.BaseURL); err != nil {
			res["ntfy"] = err.Error()
		} else {
			res["ntfy"] = "sent"
		}
	}
	if a.mailer.Enabled() && a.cfg.NotifyEmail != "" {
		if err := a.mailer.send(a.cfg.NotifyEmail, "Help desk test email",
			"Email notifications are working. If ntfy is unavailable, new chat messages will arrive here when everyone is Away or signed out.\n"); err != nil {
			res["email"] = err.Error()
		} else {
			res["email"] = "sent to " + a.cfg.NotifyEmail
		}
	}
	writeJSON(w, http.StatusOK, res)
}
