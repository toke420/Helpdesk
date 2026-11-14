package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// signIn posts to /api/auth/login and returns the session cookie value ("" on failure).
func signIn(t *testing.T, srv http.Handler, email, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Helpdesk", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

// A password an admin sets (new teammate or reset) is temporary: the agent
// must replace it before anything but the password change works.
func TestAdminSetPasswordsMustBeChanged(t *testing.T) {
	h := newHarness(t, false)
	srv := h.app.Routes()
	ctx := context.Background()
	tok := newToken()
	if err := h.store.CreateSession(ctx, hashToken(tok), h.agent.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	admin := &fbClient{t: t, srv: srv, cookie: tok}

	code, d := admin.do("POST", "/api/agents", map[string]string{"name": "Sam Lee", "email": "sam@example.com", "role": "agent", "password": "Temp-Pass-123"})
	if code != http.StatusCreated {
		t.Fatalf("create agent: %d %v", code, d)
	}
	sam := d["agent"].(map[string]any)
	if sam["must_change_password"] != true {
		t.Fatalf("new agent should have to change password: %v", sam)
	}
	samID := int64(sam["id"].(float64))

	c := signIn(t, srv, "sam@example.com", "Temp-Pass-123")
	if c == "" {
		t.Fatal("sign in with the temporary password failed")
	}
	samC := &fbClient{t: t, srv: srv, cookie: c}
	if code, _ := samC.do("GET", "/api/conversations?view=open", nil); code != http.StatusForbidden {
		t.Fatalf("inbox before changing password: %d, want 403", code)
	}
	if code, d := samC.do("GET", "/api/me", nil); code != 200 || d["agent"].(map[string]any)["must_change_password"] != true {
		t.Fatalf("me: %d %v", code, d)
	}
	if code, d := samC.do("PUT", "/api/me/password", map[string]string{"current": "Temp-Pass-123", "new": "Sams-Own-Pass-1"}); code != http.StatusNoContent {
		t.Fatalf("change password: %d %v", code, d)
	}
	if code, _ := samC.do("GET", "/api/conversations?view=open", nil); code != 200 {
		t.Fatalf("inbox after changing password: %d", code)
	}

	// An admin reset signs Sam out and makes the new password temporary too.
	if code, d := admin.do("PATCH", "/api/agents/"+strconv.FormatInt(samID, 10), map[string]string{"password": "Reset-Pass-456"}); code != 200 {
		t.Fatalf("reset: %d %v", code, d)
	} else if d["agent"].(map[string]any)["must_change_password"] != true {
		t.Fatalf("reset should require a change: %v", d)
	}
	if code, _ := samC.do("GET", "/api/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("old session after reset: %d, want 401", code)
	}
	c = signIn(t, srv, "sam@example.com", "Reset-Pass-456")
	samC = &fbClient{t: t, srv: srv, cookie: c}
	if code, _ := samC.do("GET", "/api/conversations?view=open", nil); code != http.StatusForbidden {
		t.Fatalf("inbox after reset: %d, want 403", code)
	}

	// Changing a name or role doesn't touch the flag.
	if code, d := admin.do("PATCH", "/api/agents/"+strconv.FormatInt(samID, 10), map[string]string{"role": "admin"}); code != 200 || d["agent"].(map[string]any)["must_change_password"] != true {
		t.Fatalf("role change: %d %v", code, d)
	}
}
