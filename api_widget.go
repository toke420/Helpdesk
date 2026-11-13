package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

type widgetSettings struct {
	BrandName      string `json:"brand_name"`
	AccentColor    string `json:"accent_color"`
	Greeting       string `json:"greeting"`
	OfflineMessage string `json:"offline_message"`
	Online         bool   `json:"online"`
}

func (a *App) publicSettings(r *http.Request) (widgetSettings, error) {
	s, err := a.store.Settings(r.Context())
	if err != nil {
		return widgetSettings{}, err
	}
	return widgetSettings{
		BrandName: s["brand_name"], AccentColor: s["accent_color"],
		Greeting: s["greeting"], OfflineMessage: s["offline_message"],
		Online: a.hub.Active("agents"),
	}, nil
}

type widgetContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// widgetSession resumes a visitor by token, or creates a new one.
func (a *App) widgetSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token  string `json:"token"`
		Page   string `json:"page"`
		Create bool   `json:"create"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	page := truncate(req.Page, 500)
	ua := truncate(r.UserAgent(), 300)
	ctx := r.Context()

	var contact *Contact
	token := req.Token
	if token != "" {
		c, err := a.store.ContactByToken(ctx, hashToken(token))
		if err == nil {
			contact = c
			a.store.TouchContact(ctx, c.ID, ua, page)
		} else if !errors.Is(err, errNotFound) {
			serverError(w, err)
			return
		}
	}
	if contact == nil && !req.Create {
		// Just the look and presence: no visitor record until they open the chat.
		settings, err := a.publicSettings(r)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": "", "settings": settings})
		return
	}
	if contact == nil {
		if !a.limit.Allow("newvisitor:"+a.clientIP(r), 60, time.Hour) {
			writeErr(w, http.StatusTooManyRequests, "slow down")
			return
		}
		token = newToken()
		c, err := a.store.CreateContact(ctx, hashToken(token), ua, page)
		if err != nil {
			serverError(w, err)
			return
		}
		contact = c
	}
	settings, err := a.publicSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"contact":  widgetContact{Name: contact.Name, Email: contact.Email},
		"settings": settings,
	})
}

func (a *App) widgetConversation(w http.ResponseWriter, r *http.Request, c *Contact) {
	conv, err := a.store.LatestConversation(r.Context(), c.ID)
	if errors.Is(err, errNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"conversation": nil, "messages": []any{}})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	msgs, err := a.store.ListMessages(r.Context(), conv.ID, false)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conversation": map[string]any{"id": conv.ID, "status": conv.Status},
		"messages":     msgs,
	})
}

// widgetCase is what a customer sees about one of their cases: no
// assignee, category or agent-side unread state.
type widgetCase struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	Subject       string `json:"subject"`
	Preview       string `json:"preview"`
	CreatedAt     int64  `json:"created_at"`
	LastMessageAt int64  `json:"last_message_at"`
	ClosedAt      *int64 `json:"closed_at"`
}

func toWidgetCase(c *Conversation) widgetCase {
	return widgetCase{ID: c.ID, Status: c.Status, Subject: c.Subject, Preview: c.Preview,
		CreatedAt: c.CreatedAt, LastMessageAt: c.LastMessageAt, ClosedAt: c.ClosedAt}
}

func (a *App) widgetCases(w http.ResponseWriter, r *http.Request, c *Contact) {
	convs, err := a.store.ContactConversations(r.Context(), c.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]widgetCase, 0, len(convs))
	for _, x := range convs {
		out = append(out, toWidgetCase(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

func (a *App) widgetCase(w http.ResponseWriter, r *http.Request, c *Contact) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	conv, err := a.store.GetConversation(r.Context(), id)
	// Someone else's case looks exactly like a missing one.
	if errors.Is(err, errNotFound) || (err == nil && conv.ContactID != c.ID) {
		writeErr(w, http.StatusNotFound, "case not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	msgs, err := a.store.ListMessages(r.Context(), conv.ID, false)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case": toWidgetCase(conv), "messages": msgs})
}

func (a *App) widgetSend(w http.ResponseWriter, r *http.Request, c *Contact) {
	var req struct {
		Body   string `json:"body"`
		Page   string `json:"page"`
		CaseID int64  `json:"case_id"`
		New    bool   `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	body, problem := cleanBody(req.Body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	ctx := r.Context()
	if req.Page != "" {
		a.store.TouchContact(ctx, c.ID, "", truncate(req.Page, 500))
	}
	conv, msg, created, err := a.store.AddContactMessage(ctx, c, body, req.CaseID, req.New)
	if err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "message", Data: msg})
	a.hub.Publish("agents", Event{Type: "conversation", Data: conv})
	a.hub.Publish(topicContact(c.ID), Event{Type: "message", Data: msg})

	// Let the AI agent answer, if it's on for this case.
	if a.shouldAnswer(ctx, conv) {
		a.queueAIReply(conv)
	}

	// Email the assigned agent if they aren't in the inbox right now.
	if conv.AssigneeID != nil && !a.hub.Active(topicAgent(*conv.AssigneeID)) {
		if ag, err := a.store.GetAgent(ctx, *conv.AssigneeID); err == nil && !ag.Disabled {
			who := c.Name
			if who == "" {
				who = "A customer"
			}
			a.mailer.Notify(fmt.Sprintf("agent:%d:conv:%d", ag.ID, conv.ID), ag.Email,
				fmt.Sprintf("New message from %s (#%d)", who, conv.ID),
				fmt.Sprintf("%s wrote:\n\n%s\n\nOpen the conversation: %s/#/c/%d\n", who, body, a.cfg.BaseURL, conv.ID))
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message":      msg,
		"conversation": map[string]any{"id": conv.ID, "status": conv.Status},
		"case":         toWidgetCase(conv),
		"new":          created,
	})
}

func (a *App) widgetProfile(w http.ResponseWriter, r *http.Request, c *Contact) {
	var req struct {
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		n, problem := cleanName(*req.Name)
		if problem != "" {
			writeErr(w, http.StatusBadRequest, problem)
			return
		}
		req.Name = &n
		c.Name = n
	}
	if req.Email != nil {
		e, problem := cleanEmail(*req.Email)
		if problem != "" {
			writeErr(w, http.StatusBadRequest, problem)
			return
		}
		req.Email = &e
		c.Email = e
	}
	if err := a.store.UpdateContactProfile(r.Context(), c.ID, req.Name, req.Email); err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "contact", Data: c})
	writeJSON(w, http.StatusOK, map[string]any{"contact": widgetContact{Name: c.Name, Email: c.Email}})
}

func (a *App) widgetStream(w http.ResponseWriter, r *http.Request, c *Contact) {
	first := []Event{{Type: "presence", Data: map[string]bool{"online": a.hub.Active("agents")}}}
	a.hub.Publish("agents", Event{Type: "visitor", Data: map[string]any{"contact_id": c.ID, "online": true}})
	defer a.hub.Publish("agents", Event{Type: "visitor", Data: map[string]any{"contact_id": c.ID, "online": false}})
	a.serveSSE(w, r, first, topicContact(c.ID), "visitors")
}
