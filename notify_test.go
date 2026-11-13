package main

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMail struct{ to, subject, body string }

type harness struct {
	app   *App
	store *Store
	agent *Agent
	mu    sync.Mutex
	ntfy  []string // titles + messages
	mail  []sentMail
	fail  bool
}

func newHarness(t *testing.T, ntfy bool) *harness {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := &Store{db: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, "admin@example.com", "Admin", "admin", "x")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: "https://help.example.com", NotifyEmail: "orders@example.com"}
	if ntfy {
		cfg.Ntfy = NtfyConfig{Server: "https://ntfy.example", Topic: "t"}
	}
	h := &harness{store: store, agent: ag}
	mailer := NewMailer(SMTPConfig{Host: "smtp.example.com", Port: "587", From: "help@example.com"})
	mailer.send = func(to, subject, body string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.mail = append(h.mail, sentMail{to, subject, body})
		return nil
	}
	h.app = NewApp(cfg, store, mailer, NewLLM(cfg.AI))
	h.app.emailDelay = 100 * time.Millisecond
	h.app.notify = func(ctx context.Context, title, message, click string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail {
			return errors.New("ntfy down")
		}
		h.ntfy = append(h.ntfy, title+" | "+message+" | "+click)
		return nil
	}
	return h
}

// customerSays saves a customer message and runs the notification routing.
func (h *harness) customerSays(t *testing.T, c *Contact, body string) *Conversation {
	t.Helper()
	conv, msg, _, err := h.store.AddContactMessage(context.Background(), c, body, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.app.notifyNewMessage(conv, c, msg)
	return conv
}

func (h *harness) contact(t *testing.T, name, email string) *Contact {
	t.Helper()
	ctx := context.Background()
	c, err := h.store.CreateContact(ctx, newToken(), "test", "https://shop.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	n, e := name, email
	h.store.UpdateContactProfile(ctx, c.ID, &n, &e)
	c.Name, c.Email = name, email
	return c
}

func (h *harness) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ntfy), len(h.mail)
}

func waitMail(h *harness, n int) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, m := h.counts(); m >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAvailableAgentGetsOnlyTheSound(t *testing.T) {
	h := newHarness(t, true)
	h.app.pres.connect(h.agent.ID, "desk", true, false)
	h.customerSays(t, h.contact(t, "Sam", ""), "hello?")
	time.Sleep(200 * time.Millisecond)
	if n, m := h.counts(); n != 0 || m != 0 {
		t.Fatalf("want no ntfy/email while someone is available, got ntfy=%d email=%d", n, m)
	}
}

func TestEveryoneAwaySendsNtfy(t *testing.T) {
	h := newHarness(t, true)
	h.app.pres.connect(h.agent.ID, "desk", true, false)
	h.app.pres.setAway(h.agent.ID, true)
	conv := h.customerSays(t, h.contact(t, "Sam", "sam@x.com"), "Do you deliver to Burleigh?")
	if n, _ := h.counts(); n != 1 {
		t.Fatalf("want 1 ntfy, got %d", n)
	}
	got := h.ntfy[0]
	for _, want := range []string{"Sam <sam@x.com>", "Do you deliver to Burleigh?", "/#/c/" + itoa(conv.ID)} {
		if !strings.Contains(got, want) {
			t.Errorf("ntfy %q missing %q", got, want)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if _, m := h.counts(); m != 0 {
		t.Fatalf("ntfy worked, want no email, got %d", m)
	}
}

func TestNobodySignedInSendsNtfy(t *testing.T) {
	h := newHarness(t, true)
	h.customerSays(t, h.contact(t, "", ""), "hi")
	if n, _ := h.counts(); n != 1 || !strings.Contains(h.ntfy[0], "A customer") {
		t.Fatalf("want 1 ntfy from 'A customer', got %v", h.ntfy)
	}
}

func TestPhoneInBackgroundIsNotAvailable(t *testing.T) {
	h := newHarness(t, true)
	h.app.pres.connect(h.agent.ID, "phone", false, true) // screen off
	h.customerSays(t, h.contact(t, "", ""), "one")
	h.app.pres.setVisible(h.agent.ID, "phone", true) // looking at it
	h.customerSays(t, h.contact(t, "", ""), "two")
	if n, _ := h.counts(); n != 1 {
		t.Fatalf("want ntfy only while the phone was in the background, got %d", n)
	}
}

func TestNtfyDownFallsBackToOneEmailWithEveryLine(t *testing.T) {
	h := newHarness(t, true)
	h.fail = true
	c := h.contact(t, "Jo", "jo@x.com")
	h.customerSays(t, c, "first question")
	h.customerSays(t, c, "second line")
	waitMail(h, 1)
	time.Sleep(150 * time.Millisecond)
	_, m := h.counts()
	if m != 1 {
		t.Fatalf("want exactly 1 email for the burst, got %d", m)
	}
	e := h.mail[0]
	if e.to != "orders@example.com" {
		t.Errorf("email to %q", e.to)
	}
	for _, want := range []string{"Jo <jo@x.com>", "first question", "second line", "https://help.example.com/#/c/"} {
		if !strings.Contains(e.body, want) {
			t.Errorf("email body missing %q:\n%s", want, e.body)
		}
	}
	if !strings.Contains(e.subject, "first question") {
		t.Errorf("subject %q should quote the question", e.subject)
	}
}

func TestNoNtfyConfiguredEmailsOrders(t *testing.T) {
	h := newHarness(t, false)
	h.customerSays(t, h.contact(t, "", ""), "anyone there?")
	waitMail(h, 1)
	if n, m := h.counts(); n != 0 || m != 1 || !strings.Contains(h.mail[0].body, "anyone there?") {
		t.Fatalf("want 0 ntfy and 1 email with the question, got ntfy=%d email=%v", n, h.mail)
	}
}

func TestReplyBeforeEmailCancelsIt(t *testing.T) {
	h := newHarness(t, false)
	conv := h.customerSays(t, h.contact(t, "", ""), "hello")
	if _, err := h.store.AddAgentMessage(context.Background(), conv.ID, h.agent, "Hi! How can I help?", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, m := h.counts(); m != 0 {
		t.Fatalf("an agent answered first, want no email, got %d", m)
	}
}

func TestWidgetPresenceFollowsAwayMode(t *testing.T) {
	h := newHarness(t, true)
	ch, unsub := h.app.hub.Subscribe("visitors")
	defer unsub()
	h.app.pres.connect(h.agent.ID, "desk", true, false)
	h.app.presenceChanged()
	h.app.pres.setAway(h.agent.ID, true)
	h.app.presenceChanged()
	var got []string
	for i := 0; i < 2; i++ {
		select {
		case b := <-ch:
			got = append(got, string(b))
		case <-time.After(time.Second):
			t.Fatalf("missing presence event, got %v", got)
		}
	}
	if !strings.Contains(got[0], `"online":true`) || !strings.Contains(got[1], `"online":false`) {
		t.Fatalf("want online then offline, got %v", got)
	}
}

func TestParseNtfyURL(t *testing.T) {
	c, err := parseNtfyURL("https://ntfy.sh/example-help")
	if err != nil || c.Server != "https://ntfy.sh" || c.Topic != "example-help" {
		t.Fatalf("got %+v %v", c, err)
	}
	for _, bad := range []string{"ntfy.sh/x", "https://ntfy.sh/", "https://ntfy.sh/a/b", "ftp://ntfy.sh/x"} {
		if _, err := parseNtfyURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if c, err := parseNtfyURL(""); err != nil || c.Enabled() {
		t.Errorf("empty should mean off")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestParseCaseNumber(t *testing.T) {
	for in, want := range map[string]int64{"HD-1035": 35, "hd 1035": 35, "#1035": 35, "1035": 35, " HD1001 ": 1} {
		if got, ok := parseCaseNumber(in); !ok || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "HD-", "HD-1000", "12", "HD-12a", "abc"} {
		if _, ok := parseCaseNumber(bad); ok {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if caseNumber(35) != "HD-1035" {
		t.Errorf("caseNumber(35) = %s", caseNumber(35))
	}
}

func TestNewCaseLinksToEarlierCaseAndSearchFindsIt(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	jo := h.contact(t, "Jo", "jo@x.com")
	first := h.customerSays(t, jo, "discount code broken")
	h.store.SetStatus(ctx, first.ID, "closed")

	other := h.contact(t, "Kim", "")
	conv, _, created, err := h.store.AddContactMessage(ctx, other, "about my old case", 0, false, &caseLink{ID: first.ID, Via: "number"})
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if conv.RelatedNumber != first.Number || conv.RelatedVia != "number" || conv.Number != caseNumber(conv.ID) {
		t.Fatalf("link not stored: %+v", conv)
	}
	// Searching the earlier case number finds both the case and the one that follows it.
	got, err := h.store.ListConversations(ctx, "open", h.agent.ID, 0, 50, first.Number)
	if err != nil || len(got) != 2 {
		t.Fatalf("search %s: %v %d", first.Number, err, len(got))
	}
	got, _ = h.store.ListConversations(ctx, "open", h.agent.ID, 0, 50, "discount")
	if len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("text search: %v", got)
	}
	got, _ = h.store.ListConversations(ctx, "open", h.agent.ID, 0, 50, "50%")
	if len(got) != 0 {
		t.Fatalf("%% must be literal, got %d", len(got))
	}
	// Earlier cases from the same email on another browser show in history.
	jo2 := h.contact(t, "Jo", "JO@x.com")
	h.customerSays(t, jo2, "hi again")
	hist, _ := h.store.ContactConversations(ctx, jo2.ID, jo2.Email)
	if len(hist) != 2 {
		t.Fatalf("want 2 cases for jo@x.com, got %d", len(hist))
	}
}
