package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fbClient struct {
	t      *testing.T
	srv    http.Handler
	token  string // visitor bearer token
	cookie string // agent session cookie value
}

func (c *fbClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookie})
		r.Header.Set("X-Helpdesk", "1")
	}
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, r)
	out := map[string]any{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func ideaList(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["ideas"].([]any)
	out := []map[string]any{}
	for _, x := range raw {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestFeedbackBoard(t *testing.T) {
	h := newHarness(t, false)
	srv := h.app.Routes()
	ctx := context.Background()

	// Two customers with visitor sessions; an agent with a cookie session.
	newVisitor := func() *fbClient {
		c := &fbClient{t: t, srv: srv}
		code, d := c.do("POST", "/api/widget/session", map[string]any{"create": true})
		if code != 200 {
			t.Fatalf("session %d %v", code, d)
		}
		c.token = d["token"].(string)
		return c
	}
	alice, bob := newVisitor(), newVisitor()
	anon := &fbClient{t: t, srv: srv}
	tok := newToken()
	if err := h.store.CreateSession(ctx, hashToken(tok), h.agent.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	agent := &fbClient{t: t, srv: srv, cookie: tok}

	// Posting needs a name and email.
	if code, _ := alice.do("POST", "/api/widget/ideas", map[string]string{"title": "Dark mode", "body": "Please"}); code != http.StatusPreconditionRequired {
		t.Fatalf("post without email: %d", code)
	}
	alice.do("POST", "/api/widget/profile", map[string]string{"name": "Alice Jones", "email": "alice@example.com"})
	code, d := alice.do("POST", "/api/widget/ideas", map[string]string{"title": "Dark mode", "body": "Please"})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %v", code, d)
	}
	idea := d["idea"].(map[string]any)
	id := int64(idea["id"].(float64))
	if idea["approved"] != false || idea["votes"].(float64) != 1 || idea["voted"] != true || idea["author_name"] != "Alice J." {
		t.Fatalf("new idea: %v", idea)
	}
	if strings.Contains(mustJSON(idea), "alice@example.com") {
		t.Fatal("customer-facing idea leaks the email")
	}

	// Waiting for approval: only Alice and agents see it.
	_, d = alice.do("GET", "/api/widget/ideas", nil)
	if len(ideaList(t, d)) != 1 {
		t.Fatal("author should see their own pending idea")
	}
	for _, c := range []*fbClient{bob, anon} {
		_, d = c.do("GET", "/api/widget/ideas", nil)
		if len(ideaList(t, d)) != 0 {
			t.Fatal("pending idea visible to others")
		}
		if code, _ := c.do("GET", "/api/widget/ideas/"+idStr(id), nil); code != 404 {
			t.Fatal("pending idea readable by others")
		}
	}
	_, d = agent.do("GET", "/api/ideas?status=pending", nil)
	if len(ideaList(t, d)) != 1 || d["counts"].(map[string]any)["pending"].(float64) != 1 {
		t.Fatalf("agent pending list: %v", d)
	}

	// Approve and move to Planned: public, and the change shows on the timeline.
	if code, d := agent.do("PATCH", "/api/ideas/"+idStr(id), map[string]any{"approved": true, "status": "planned"}); code != 200 {
		t.Fatalf("approve: %d %v", code, d)
	}
	_, d = anon.do("GET", "/api/widget/ideas", nil)
	if l := ideaList(t, d); len(l) != 1 || l[0]["status_label"] != "Planned" || l[0]["voted"] != false {
		t.Fatalf("public list: %v", d)
	}

	// Voting: one per customer, toggles, anonymous readers can't vote.
	if code, _ := anon.do("POST", "/api/widget/ideas/"+idStr(id)+"/vote", map[string]bool{"vote": true}); code != 401 {
		t.Fatal("vote without a session")
	}
	bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/vote", map[string]bool{"vote": true})
	_, d = bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/vote", map[string]bool{"vote": true})
	if d["votes"].(float64) != 2 {
		t.Fatalf("double vote counted: %v", d)
	}
	_, d = bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/vote", map[string]bool{"vote": false})
	if d["votes"].(float64) != 1 {
		t.Fatalf("unvote: %v", d)
	}
	bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/vote", map[string]bool{"vote": true})

	// Comments: Bob needs an email; agent replies; customers see the brand, not emails.
	if code, _ := bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/comments", map[string]string{"body": "+1"}); code != http.StatusPreconditionRequired {
		t.Fatal("comment without email")
	}
	bob.do("POST", "/api/widget/profile", map[string]string{"name": "Bob", "email": "bob@example.com"})
	if code, _ := bob.do("POST", "/api/widget/ideas/"+idStr(id)+"/comments", map[string]string{"body": "Yes please"}); code != 204 {
		t.Fatal("comment")
	}
	agent.do("POST", "/api/ideas/"+idStr(id)+"/comments", map[string]string{"body": "On it"})
	_, d = anon.do("GET", "/api/widget/ideas/"+idStr(id), nil)
	body := mustJSON(d)
	if strings.Contains(body, "@example.com") || !strings.Contains(body, "Status changed to Planned") || !strings.Contains(body, "Admin · Support") {
		t.Fatalf("public idea page: %s", body)
	}
	if d["idea"].(map[string]any)["comments"].(float64) != 2 {
		t.Fatalf("comment count should skip status notes: %v", d["idea"])
	}

	// Agents see voters with emails.
	_, d = agent.do("GET", "/api/ideas/"+idStr(id), nil)
	if v := d["voters"].([]any); len(v) != 2 || !strings.Contains(mustJSON(v), "bob@example.com") {
		t.Fatalf("voters: %v", d["voters"])
	}

	// Sorting by votes, and the board can be switched off.
	agent.do("POST", "/api/ideas", map[string]string{"title": "Second idea"})
	_, d = anon.do("GET", "/api/widget/ideas?sort=top", nil)
	if l := ideaList(t, d); len(l) != 2 || l[0]["title"] != "Dark mode" || l[1]["author_name"] != "Support" {
		t.Fatalf("top sort / agent author: %v", d)
	}
	h.store.PutSettings(ctx, map[string]string{"feedback_enabled": "0"})
	if code, _ := anon.do("GET", "/api/widget/ideas", nil); code != 404 {
		t.Fatal("board still on")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func idStr(n int64) string  { b, _ := json.Marshal(n); return string(b) }

func TestFeedbackPhase2(t *testing.T) {
	h := newHarness(t, false)
	srv := h.app.Routes()
	ctx := context.Background()
	h.store.PutSettings(ctx, map[string]string{"feedback_approval": "0"})
	tok := newToken()
	h.store.CreateSession(ctx, hashToken(tok), h.agent.ID, time.Now().Add(time.Hour))
	agent := &fbClient{t: t, srv: srv, cookie: tok}
	visitor := func(name, email string) *fbClient {
		c := &fbClient{t: t, srv: srv}
		_, d := c.do("POST", "/api/widget/session", map[string]any{"create": true})
		c.token = d["token"].(string)
		if email != "" {
			c.do("POST", "/api/widget/profile", map[string]string{"name": name, "email": email})
		}
		return c
	}
	ann, ben := visitor("Ann Lee", "ann@example.com"), visitor("Ben", "ben@example.com")
	post := func(c *fbClient, title string) int64 {
		_, d := c.do("POST", "/api/widget/ideas", map[string]string{"title": title})
		return int64(d["idea"].(map[string]any)["id"].(float64))
	}
	a := post(ann, "Gift wrapping")
	b := post(ben, "Gift wrap option")
	ann.do("POST", "/api/widget/ideas/"+idStr(b)+"/vote", map[string]bool{"vote": true}) // Ann voted on both

	// Merge b into a: Ben's vote moves, Ann's isn't counted twice, old link redirects.
	code, d := agent.do("POST", "/api/ideas/"+idStr(b)+"/merge", map[string]int64{"into": a})
	if code != 200 || d["votes_added"].(float64) != 1 {
		t.Fatalf("merge: %d %v", code, d)
	}
	_, d = ann.do("GET", "/api/widget/ideas/"+idStr(b), nil)
	if d["merged_into"].(float64) != float64(a) {
		t.Fatalf("merged link should redirect: %v", d)
	}
	_, d = ann.do("GET", "/api/widget/ideas", nil)
	if l := ideaList(t, d); len(l) != 1 || l[0]["votes"].(float64) != 2 {
		t.Fatalf("after merge: %v", d)
	}
	if code, _ := agent.do("POST", "/api/ideas/"+idStr(a)+"/merge", map[string]int64{"into": a}); code != 400 {
		t.Fatal("merge into itself allowed")
	}

	// Vote on a customer's behalf from their case.
	cara := h.contact(t, "Cara", "cara@example.com")
	conv := h.customerSays(t, cara, "Do you gift wrap?")
	if code, _ := agent.do("POST", "/api/ideas/"+idStr(a)+"/votes", map[string]int64{"conversation_id": conv.ID}); code != 204 {
		t.Fatal("vote for case")
	}
	_, d = agent.do("GET", "/api/conversations/"+idStr(conv.ID)+"/ideas", nil)
	if l := ideaList(t, d); len(l) != 1 || l[0]["id"].(float64) != float64(a) {
		t.Fatalf("case ideas: %v", d)
	}
	_, d = agent.do("GET", "/api/ideas/"+idStr(a), nil)
	if d["emailable"].(float64) != 3 {
		t.Fatalf("should email Ann, Ben, Cara: %v", d["emailable"])
	}

	// Status change with a note emails each follower once, with an unsubscribe link.
	h.mu.Lock()
	h.mail = nil
	h.mu.Unlock()
	agent.do("PATCH", "/api/ideas/"+idStr(a), map[string]any{"status": "planned", "note": "Coming in December."})
	mails := waitFeedbackMail(t, h, 3)
	var unsub string
	for _, m := range mails {
		if !strings.Contains(m.subject, "Planned") || !strings.Contains(m.body, "Coming in December.") || !strings.Contains(m.body, "/feedback#/idea/") {
			t.Fatalf("status email: %+v", m)
		}
		if m.to == "ben@example.com" {
			unsub = m.body[strings.Index(m.body, "/feedback/unsubscribe?t="):]
			unsub = strings.TrimSpace(strings.SplitN(unsub, "\n", 2)[0])
		}
	}
	// Ben unsubscribes; the next update skips him.
	r := httptest.NewRequest("GET", unsub, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "any more emails about ideas") {
		t.Fatalf("unsubscribe: %d %s", w.Code, w.Body.String())
	}
	_, d = agent.do("GET", "/api/ideas/"+idStr(a), nil)
	if d["emailable"].(float64) != 2 {
		t.Fatalf("after unsubscribe: %v", d["emailable"])
	}

	// Changelog: drafts are private; publishing marks linked ideas Complete and emails.
	_, d = agent.do("POST", "/api/changelog", map[string]any{"title": "Gift wrapping is here", "body": "Tick the box at checkout.", "idea_ids": []int64{a}})
	eid := int64(d["entry"].(map[string]any)["id"].(float64))
	_, d = ann.do("GET", "/api/widget/changelog", nil)
	if len(d["entries"].([]any)) != 0 {
		t.Fatal("draft visible to customers")
	}
	h.mu.Lock()
	h.mail = nil
	h.mu.Unlock()
	agent.do("POST", "/api/changelog/"+idStr(eid)+"/publish", map[string]bool{"published": true, "notify": true})
	mails = waitFeedbackMail(t, h, 2)
	for _, m := range mails {
		if m.to == "ben@example.com" || !strings.Contains(m.body, "Tick the box at checkout.") {
			t.Fatalf("shipped email: %+v", m)
		}
	}
	_, d = ann.do("GET", "/api/widget/changelog", nil)
	es := d["entries"].([]any)
	if len(es) != 1 || !strings.Contains(mustJSON(es), "Gift wrapping") || strings.Contains(mustJSON(es), "@example.com") {
		t.Fatalf("public changelog: %v", d)
	}
	_, d = ann.do("GET", "/api/widget/ideas/"+idStr(a), nil)
	if d["idea"].(map[string]any)["status"] != "complete" || !strings.Contains(mustJSON(d), "Shipped") {
		t.Fatalf("idea after publish: %v", d)
	}
	// Publishing again doesn't email twice.
	agent.do("POST", "/api/changelog/"+idStr(eid)+"/publish", map[string]bool{"published": false})
	h.mu.Lock()
	h.mail = nil
	h.mu.Unlock()
	agent.do("POST", "/api/changelog/"+idStr(eid)+"/publish", map[string]bool{"published": true, "notify": true})
	time.Sleep(200 * time.Millisecond)
	if n := len(feedbackMail(h)); n != 0 {
		t.Fatalf("republish emailed %d", len(feedbackMail(h)))
	}
	// Roadmap lists planned/in progress/complete.
	_, d = ann.do("GET", "/api/widget/ideas?status=roadmap", nil)
	if len(ideaList(t, d)) != 1 {
		t.Fatalf("roadmap: %v", d)
	}
}

// feedbackMail: emails sent to customers (not the team's new-chat alerts).
func feedbackMail(h *harness) []sentMail {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []sentMail{}
	for _, m := range h.mail {
		if m.to != "orders@example.com" {
			out = append(out, m)
		}
	}
	return out
}

func waitFeedbackMail(t *testing.T, h *harness, n int) []sentMail {
	t.Helper()
	for i := 0; i < 100; i++ {
		got := feedbackMail(h)
		if len(got) >= n {
			time.Sleep(50 * time.Millisecond)
			got = feedbackMail(h)
			if len(got) != n {
				t.Fatalf("want %d emails, got %d: %+v", n, len(got), got)
			}
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("want %d emails, got %d", n, len(h.mail))
	return nil
}
