package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- Feedback board ----------
// Customers use it from the chat widget and the public /feedback page, both
// through /api/widget/ideas (visitor token optional for reading). Agents
// manage it through /api/ideas.

func (a *App) feedbackSettings(r *http.Request) (on, approval bool, brand string, err error) {
	s, err := a.store.Settings(r.Context())
	if err != nil {
		return false, false, "", err
	}
	return s["feedback_enabled"] == "1", s["feedback_approval"] == "1", s["brand_name"], nil
}

// optionalVisitor resolves the bearer token if there is one; readers needn't have one.
func (a *App) optionalVisitor(h func(http.ResponseWriter, *http.Request, *Contact)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var c *Contact
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
			if found, err := a.store.ContactByToken(r.Context(), hashToken(tok)); err == nil {
				c = found
			}
		}
		h(w, r, c)
	}
}

func contactID(c *Contact) int64 {
	if c == nil {
		return 0
	}
	return c.ID
}

func cleanIdeaText(title, body string) (string, string, string) {
	title = strings.Join(strings.Fields(title), " ")
	body = strings.TrimSpace(body)
	switch {
	case utf8.RuneCountInString(title) < 3:
		return "", "", "give your idea a short title"
	case utf8.RuneCountInString(title) > 120:
		return "", "", "keep the title under 120 characters"
	case utf8.RuneCountInString(body) > 5000:
		return "", "", "the description is too long (5,000 characters max)"
	}
	return title, body, ""
}

func cleanComment(body string) (string, string) {
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return "", "write a comment first"
	case utf8.RuneCountInString(body) > 3000:
		return "", "keep comments under 3,000 characters"
	}
	return body, ""
}

// canSeeIdea: customers see approved ideas and their own; merged ones are gone.
func canSeeIdea(it *Idea, c *Contact) bool {
	return it.MergedInto == nil && (it.Approved || it.Mine) && (c != nil || it.Approved)
}

func (a *App) ideasChanged(id int64) {
	a.hub.Publish("agents", Event{Type: "ideas", Data: map[string]any{"id": id}})
}

// --- customer side ---

func (a *App) widgetIdeas(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, _, brand, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	if !on {
		writeErr(w, http.StatusNotFound, "the feedback board is off")
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "active"
	}
	if status != "active" && !validIdeaStatus(status) {
		writeErr(w, http.StatusBadRequest, "unknown status")
		return
	}
	ideas, err := a.store.ListIdeas(r.Context(), IdeaFilter{Status: status, Query: truncate(q.Get("q"), 100), Sort: q.Get("sort"), Viewer: contactID(c), Limit: 200})
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]*Idea, 0, len(ideas))
	for _, it := range ideas {
		out = append(out, it.ForCustomer(brand))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ideas": out, "statuses": IdeaStatuses})
}

func (a *App) widgetIdea(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, _, brand, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	id, ok := pathID(r)
	if !on || !ok {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	}
	it, err := a.store.GetIdea(r.Context(), id, contactID(c))
	if errors.Is(err, errNotFound) || (err == nil && !canSeeIdea(it, c)) {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	comments, err := a.store.ListIdeaComments(r.Context(), id, contactID(c))
	if err != nil {
		serverError(w, err)
		return
	}
	for _, m := range comments {
		m.AuthorID = nil
		switch m.AuthorType {
		case "agent":
			m.AuthorName += " · " + brand
		case "contact":
			m.AuthorName = publicName(m.AuthorName)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"idea": it.ForCustomer(brand), "comments": comments})
}

// needsEmail: posting and commenting ask for a name and email first.
func needsEmail(w http.ResponseWriter, c *Contact) bool {
	if c.Email == "" || strings.TrimSpace(c.Name) == "" {
		writeErr(w, http.StatusPreconditionRequired, "add your name and email first")
		return true
	}
	return false
}

func (a *App) widgetCreateIdea(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, approval, brand, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	if !on {
		writeErr(w, http.StatusNotFound, "the feedback board is off")
		return
	}
	if needsEmail(w, c) {
		return
	}
	var req struct{ Title, Body string }
	if !readJSON(w, r, &req) {
		return
	}
	title, body, problem := cleanIdeaText(req.Title, req.Body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	if !a.limit.Allow("idea:"+a.clientIP(r), 10, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "that's a lot of ideas — try again later")
		return
	}
	cid := c.ID
	id, err := a.store.CreateIdea(r.Context(), NewIdea{Title: title, Body: body, Approved: !approval, ContactID: &cid})
	if err != nil {
		serverError(w, err)
		return
	}
	it, err := a.store.GetIdea(r.Context(), id, c.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(id)
	writeJSON(w, http.StatusCreated, map[string]any{"idea": it.ForCustomer(brand)})
}

func (a *App) widgetVote(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, _, _, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	id, ok := pathID(r)
	if !on || !ok {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	}
	var req struct {
		Vote bool `json:"vote"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	it, err := a.store.GetIdea(r.Context(), id, c.ID)
	if errors.Is(err, errNotFound) || (err == nil && !canSeeIdea(it, c)) {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	if it.Status == "closed" && req.Vote {
		writeErr(w, http.StatusBadRequest, "this idea is closed")
		return
	}
	n, err := a.store.SetVote(r.Context(), id, c.ID, req.Vote)
	if err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(id)
	writeJSON(w, http.StatusOK, map[string]any{"votes": n, "voted": req.Vote})
}

func (a *App) widgetIdeaComment(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, _, _, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	id, ok := pathID(r)
	if !on || !ok {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	}
	if needsEmail(w, c) {
		return
	}
	var req struct{ Body string }
	if !readJSON(w, r, &req) {
		return
	}
	body, problem := cleanComment(req.Body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	it, err := a.store.GetIdea(r.Context(), id, c.ID)
	if errors.Is(err, errNotFound) || (err == nil && !canSeeIdea(it, c)) {
		writeErr(w, http.StatusNotFound, "idea not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	if !a.limit.Allow("ideacomment:"+a.clientIP(r), 30, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	cid := c.ID
	if _, err := a.store.AddIdeaComment(r.Context(), id, "contact", &cid, body); err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(id)
	w.WriteHeader(http.StatusNoContent)
}

// --- agent side ---

func (a *App) listIdeas(w http.ResponseWriter, r *http.Request, ag *Agent) {
	q := r.URL.Query()
	f := IdeaFilter{Status: q.Get("status"), Query: truncate(q.Get("q"), 100), Sort: q.Get("sort"), Agent: true, Pending: q.Get("status") == "pending"}
	if f.Pending {
		f.Status = ""
	} else if f.Status != "" && f.Status != "active" && !validIdeaStatus(f.Status) {
		writeErr(w, http.StatusBadRequest, "unknown status")
		return
	}
	ideas, err := a.store.ListIdeas(r.Context(), f)
	if err != nil {
		serverError(w, err)
		return
	}
	counts, err := a.store.IdeaCounts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ideas": ideas, "counts": counts, "statuses": IdeaStatuses})
}

func (a *App) ideaCounts(w http.ResponseWriter, r *http.Request, ag *Agent) {
	counts, err := a.store.IdeaCounts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts})
}

func (a *App) loadIdea(w http.ResponseWriter, r *http.Request) (*Idea, bool) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return nil, false
	}
	it, err := a.store.GetIdea(r.Context(), id, 0)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "idea not found")
		return nil, false
	} else if err != nil {
		serverError(w, err)
		return nil, false
	}
	return it, true
}

func (a *App) getIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	comments, err := a.store.ListIdeaComments(r.Context(), it.ID, 0)
	if err != nil {
		serverError(w, err)
		return
	}
	voters, err := a.store.IdeaVoters(r.Context(), it.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"idea": it, "comments": comments, "voters": voters})
}

func (a *App) createIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Title, Body, Status string
		SourceCase          *int64 `json:"source_case"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	title, body, problem := cleanIdeaText(req.Title, req.Body)
	if problem == "" && req.Status != "" && !validIdeaStatus(req.Status) {
		problem = "unknown status"
	}
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	agID := ag.ID
	id, err := a.store.CreateIdea(r.Context(), NewIdea{Title: title, Body: body, Status: req.Status, Approved: true, AgentID: &agID, SourceCase: req.SourceCase})
	if err != nil {
		serverError(w, err)
		return
	}
	it, err := a.store.GetIdea(r.Context(), id, 0)
	if err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(id)
	writeJSON(w, http.StatusCreated, map[string]any{"idea": it})
}

func (a *App) updateIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	var req struct {
		Title    *string `json:"title"`
		Body     *string `json:"body"`
		Status   *string `json:"status"`
		Approved *bool   `json:"approved"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := IdeaUpdate{Approved: req.Approved}
	if req.Title != nil || req.Body != nil {
		title, body := it.Title, it.Body
		if req.Title != nil {
			title = *req.Title
		}
		if req.Body != nil {
			body = *req.Body
		}
		t, b, problem := cleanIdeaText(title, body)
		if problem != "" {
			writeErr(w, http.StatusBadRequest, problem)
			return
		}
		u.Title, u.Body = &t, &b
	}
	statusChanged := false
	if req.Status != nil && *req.Status != it.Status {
		if !validIdeaStatus(*req.Status) {
			writeErr(w, http.StatusBadRequest, "unknown status")
			return
		}
		u.Status, statusChanged = req.Status, true
	}
	if err := a.store.UpdateIdea(r.Context(), it.ID, u); err != nil {
		serverError(w, err)
		return
	}
	// Status changes show on the idea's timeline for everyone following it.
	if statusChanged {
		agID := ag.ID
		a.store.AddIdeaComment(r.Context(), it.ID, "system", &agID, "Status changed to "+ideaStatusLabel(*req.Status))
	}
	a.ideasChanged(it.ID)
	saved, err := a.store.GetIdea(r.Context(), it.ID, 0)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"idea": saved})
}

func (a *App) deleteIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteIdea(r.Context(), it.ID); err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(it.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) agentIdeaComment(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	var req struct{ Body string }
	if !readJSON(w, r, &req) {
		return
	}
	body, problem := cleanComment(req.Body)
	if problem != "" {
		writeErr(w, http.StatusBadRequest, problem)
		return
	}
	agID := ag.ID
	if _, err := a.store.AddIdeaComment(r.Context(), it.ID, "agent", &agID, body); err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(it.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteIdeaComment(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	cid, err := parseInt64(r.PathValue("cid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad comment id")
		return
	}
	if err := a.store.DeleteIdeaComment(r.Context(), it.ID, cid); errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "comment not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(it.ID)
	w.WriteHeader(http.StatusNoContent)
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
