package main

import (
	"errors"
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
		Online: a.pres.anyAvailable(),
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
		"conversation": widgetConv(conv),
		"messages":     msgs,
	})
}

// widgetCase is what a customer sees about one of their cases: no
// assignee, category or agent-side unread state.
type widgetCase struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	StatusLabel   string `json:"status_label"` // full case state, e.g. "Awaiting Approval"
	Subject       string `json:"subject"`
	Preview       string `json:"preview"`
	CreatedAt     int64  `json:"created_at"`
	LastMessageAt int64  `json:"last_message_at"`
	ClosedAt      *int64 `json:"closed_at"`
	Number        string `json:"number"`
	RelatedNumber string `json:"related_number"`
}

func toWidgetCase(c *Conversation) widgetCase {
	return widgetCase{ID: c.ID, Status: c.Status, StatusLabel: caseStateLabel(c), Subject: c.Subject, Preview: c.Preview,
		CreatedAt: c.CreatedAt, LastMessageAt: c.LastMessageAt, ClosedAt: c.ClosedAt,
		Number: c.Number, RelatedNumber: c.RelatedNumber}
}

func (a *App) widgetCases(w http.ResponseWriter, r *http.Request, c *Contact) {
	convs, err := a.store.ContactConversations(r.Context(), c.ID, "") // never by email: a visitor's email is unverified
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
		// Only used when this message starts a new case.
		Related string `json:"related"`
		Via     string `json:"via"`
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
	link, problem, err := a.resolveCaseLink(r, c, req.Related, req.Via)
	if err != nil {
		serverError(w, err)
		return
	}
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	if req.Page != "" {
		a.store.TouchContact(ctx, c.ID, "", truncate(req.Page, 500))
	}
	conv, msg, created, err := a.store.AddContactMessage(ctx, c, body, req.CaseID, req.New, link)
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

	// Sound in the open inbox, else phones, else email (notify.go).
	contact := *c
	go a.notifyNewMessage(conv, &contact, msg)
	writeJSON(w, http.StatusCreated, map[string]any{
		"message":      msg,
		"conversation": widgetConv(conv),
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
	first := []Event{{Type: "presence", Data: map[string]bool{"online": a.pres.anyAvailable()}}}
	a.hub.Publish("agents", Event{Type: "visitor", Data: map[string]any{"contact_id": c.ID, "online": true}})
	defer a.hub.Publish("agents", Event{Type: "visitor", Data: map[string]any{"contact_id": c.ID, "online": false}})
	a.serveSSE(w, r, first, topicContact(c.ID), "visitors")
}

// widgetConv is what a visitor may know about their own conversation.
func widgetConv(c *Conversation) map[string]any {
	return map[string]any{"id": c.ID, "status": c.Status, "status_label": caseStateLabel(c), "number": c.Number, "related_number": c.RelatedNumber}
}

// resolveCaseLink checks the visitor's answer to "is this about a previous
// case?". A typed case number only links the new chat for the team: the
// visitor never sees that case's messages, so guessing numbers reveals nothing.
func (a *App) resolveCaseLink(r *http.Request, c *Contact, related, via string) (*caseLink, string, error) {
	switch via {
	case "", "new":
		return nil, "", nil
	case "number":
		id, ok := parseCaseNumber(related)
		if !ok {
			return nil, "That doesn't look like a case number. It looks like HD-1234.", nil
		}
		exists, err := a.store.ConversationExists(r.Context(), id)
		if err != nil {
			return nil, "", err
		}
		if !exists {
			return nil, "We couldn't find that case number. Check it, or choose \u201cI'm not sure\u201d.", nil
		}
		return &caseLink{ID: id, Via: "number"}, "", nil
	case "unsure":
		// Only ever this visitor's own last case.
		last, err := a.store.LatestConversation(r.Context(), c.ID)
		if errors.Is(err, errNotFound) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		return &caseLink{ID: last.ID, Via: "unsure"}, "", nil
	}
	return nil, "unknown answer", nil
}

// widgetLastCase is the visitor's own most recent case, for "I'm not sure".
func (a *App) widgetLastCase(w http.ResponseWriter, r *http.Request, c *Contact) {
	last, err := a.store.LatestConversation(r.Context(), c.ID)
	if errors.Is(err, errNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"case": nil})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case": map[string]any{
		"number": last.Number, "subject": last.Subject, "status": last.Status, "status_label": caseStateLabel(last), "created_at": last.CreatedAt,
	}})
}

// widgetCaseCheck says whether a typed case number exists, and nothing else.
func (a *App) widgetCaseCheck(w http.ResponseWriter, r *http.Request, c *Contact) {
	if !a.limit.Allow("casecheck:"+a.clientIP(r), 20, 10*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "Too many tries. Wait a few minutes, or choose \u201cI'm not sure\u201d.")
		return
	}
	var req struct {
		Number string `json:"number"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	link, problem, err := a.resolveCaseLink(r, c, req.Number, "number")
	if err != nil {
		serverError(w, err)
		return
	}
	if problem != "" {
		writeErr(w, http.StatusNotFound, problem)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": caseNumber(link.ID)})
}
