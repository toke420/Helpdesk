package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
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
	if status != "active" && status != "roadmap" && !validIdeaStatus(status) {
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
	if err == nil && it.MergedInto != nil {
		// Old links to a merged idea lead to the one it was folded into.
		writeJSON(w, http.StatusOK, map[string]any{"merged_into": *it.MergedInto})
		return
	}
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
	} else if f.Status != "" && f.Status != "active" && f.Status != "roadmap" && !validIdeaStatus(f.Status) {
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
	writeJSON(w, http.StatusOK, map[string]any{"idea": it, "comments": comments, "voters": voters,
		"emailable": a.ideaFollowerCount(r.Context(), it.ID), "email_enabled": a.mailer.Enabled()})
}

func (a *App) createIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var req struct {
		Title, Body, Status string
		SourceCase          *int64 `json:"source_case"`
		AddVote             bool   `json:"add_vote"` // vote for the source case's customer
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
	var voter int64
	if req.SourceCase != nil {
		conv, err := a.store.GetConversation(r.Context(), *req.SourceCase)
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusBadRequest, "that case doesn't exist")
			return
		} else if err != nil {
			serverError(w, err)
			return
		}
		voter = conv.ContactID
	}
	id, err := a.store.CreateIdea(r.Context(), NewIdea{Title: title, Body: body, Status: req.Status, Approved: true, AgentID: &agID, SourceCase: req.SourceCase})
	if err != nil {
		serverError(w, err)
		return
	}
	if req.AddVote && voter > 0 {
		if err := a.store.AddVoteFor(r.Context(), id, voter); err != nil {
			serverError(w, err)
			return
		}
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
		Note     string  `json:"note"` // posted as a reply with a status change
		Notify   *bool   `json:"notify"`
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
	// Status changes show on the idea's timeline, and voters hear about them.
	if statusChanged {
		agID := ag.ID
		a.store.AddIdeaComment(r.Context(), it.ID, "system", &agID, "Status changed to "+ideaStatusLabel(*req.Status))
		note, _ := cleanComment(req.Note)
		if note != "" {
			a.store.AddIdeaComment(r.Context(), it.ID, "agent", &agID, note)
		}
		visible := it.Approved || (req.Approved != nil && *req.Approved)
		if visible && (req.Notify == nil || *req.Notify) {
			msg := fmt.Sprintf("“%s” is now %s.", it.Title, ideaStatusLabel(*req.Status))
			if note != "" {
				msg += "\n\n" + ag.Name + " wrote:\n" + note
			}
			a.notifyFollowers(it.ID, it.Title, fmt.Sprintf("Update: %s is now %s", clip(it.Title, 60), ideaStatusLabel(*req.Status)), msg)
		}
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

// ---------- phase 2: merge, votes for customers, emails, changelog ----------

func (a *App) mergeIdea(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	var req struct {
		Into int64 `json:"into"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	dst, err := a.store.GetIdea(r.Context(), req.Into, 0)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusBadRequest, "pick an idea to merge into")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	moved, err := a.store.MergeIdea(r.Context(), it.ID, dst.ID)
	var bad mergeError
	if errors.As(err, &bad) {
		writeErr(w, http.StatusBadRequest, string(bad))
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	agID := ag.ID
	a.store.AddIdeaComment(r.Context(), dst.ID, "system", &agID, fmt.Sprintf("Merged “%s” into this idea (+%d votes)", clip(it.Title, 80), moved))
	a.ideasChanged(it.ID)
	a.ideasChanged(dst.ID)
	writeJSON(w, http.StatusOK, map[string]any{"into": dst.ID, "votes_added": moved})
}

// voteFor adds a customer's vote from the inbox: by contact, or by a case's customer.
func (a *App) voteFor(w http.ResponseWriter, r *http.Request, ag *Agent) {
	it, ok := a.loadIdea(w, r)
	if !ok {
		return
	}
	var req struct {
		ContactID      int64 `json:"contact_id"`
		ConversationID int64 `json:"conversation_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	cid := req.ContactID
	if req.ConversationID > 0 {
		conv, err := a.store.GetConversation(r.Context(), req.ConversationID)
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusBadRequest, "that case doesn't exist")
			return
		} else if err != nil {
			serverError(w, err)
			return
		}
		cid = conv.ContactID
	}
	if cid <= 0 {
		writeErr(w, http.StatusBadRequest, "whose vote is it?")
		return
	}
	if it.MergedInto != nil {
		writeErr(w, http.StatusBadRequest, "that idea was merged into another one")
		return
	}
	if err := a.store.AddVoteFor(r.Context(), it.ID, cid); err != nil {
		serverError(w, err)
		return
	}
	a.ideasChanged(it.ID)
	w.WriteHeader(http.StatusNoContent)
}

// caseIdeas: ideas this case's customer voted for (details panel).
func (a *App) caseIdeas(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	ideas, err := a.store.IdeasForContact(r.Context(), conv.ContactID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ideas": ideas})
}

// notifyFollowers emails an idea's voters (and author) in the background,
// one after another, each with their own unsubscribe link.
func (a *App) notifyFollowers(ideaID int64, title, subject, message string) {
	if !a.mailer.Enabled() {
		return
	}
	followers, err := a.store.IdeaFollowers(context.Background(), ideaID)
	if err != nil {
		slog.Error("feedback followers", "idea", ideaID, "err", err)
		return
	}
	if len(followers) == 0 {
		return
	}
	s, _ := a.store.Settings(context.Background())
	brand := s["brand_name"]
	link := a.cfg.BaseURL + "/feedback#/idea/" + strconv.FormatInt(ideaID, 10)
	go func() {
		sent := 0
		for _, f := range followers {
			hi := "Hi,"
			if first := strings.Fields(f.Name); len(first) > 0 {
				hi = "Hi " + first[0] + ","
			}
			body := hi + "\n\n" + message + "\n\nSee the idea: " + link + "\n\n— " + brand +
				"\n\n--\nYou're getting this because you posted or voted for “" + clip(title, 80) + "”." +
				"\nStop feedback emails: " + a.cfg.BaseURL + "/feedback/unsubscribe?t=" + f.Token + "\n"
			if err := a.mailer.send(f.Email, subject, body); err != nil {
				slog.Error("feedback email failed", "idea", ideaID, "to", f.Email, "err", err)
				continue
			}
			sent++
		}
		slog.Info("feedback emails sent", "idea", ideaID, "sent", sent, "of", len(followers))
	}()
}

func (a *App) feedbackUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ok, err := a.store.FeedbackUnsubscribe(r.Context(), r.URL.Query().Get("t"))
	if err != nil {
		serverError(w, err)
		return
	}
	msg := "You won't get any more emails about ideas on our feedback board."
	if !ok {
		msg = "That link didn't work — it may be out of date. Reply to any of our emails and we'll take you off the list."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light dark"><title>Email preferences</title>`+
		`<style>body{font:16px/1.5 system-ui,sans-serif;max-width:520px;margin:15vh auto;padding:0 20px}a{color:inherit}</style></head>`+
		`<body><h1>Email preferences</h1><p>%s</p><p><a href="/feedback">Back to the feedback board</a></p></body></html>`, html.EscapeString(msg))
}

// --- changelog ---

func (a *App) widgetChangelog(w http.ResponseWriter, r *http.Request, c *Contact) {
	on, _, brand, err := a.feedbackSettings(r)
	if err != nil {
		serverError(w, err)
		return
	}
	if !on {
		writeErr(w, http.StatusNotFound, "the feedback board is off")
		return
	}
	entries, err := a.store.ListChangelog(r.Context(), true)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, e := range entries {
		e.CreatedBy = nil
		shown := []*Idea{}
		for _, it := range e.Ideas {
			if it.Approved {
				shown = append(shown, it.ForCustomer(brand))
			}
		}
		e.Ideas = shown
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (a *App) listChangelog(w http.ResponseWriter, r *http.Request, ag *Agent) {
	entries, err := a.store.ListChangelog(r.Context(), false)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (a *App) getChangelog(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	e, err := a.store.GetChangelog(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "entry not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (a *App) saveChangelog(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var id int64
	if r.PathValue("id") != "" {
		var ok bool
		if id, ok = pathID(r); !ok {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
	}
	var req struct {
		Title   string  `json:"title"`
		Body    string  `json:"body"`
		IdeaIDs []int64 `json:"idea_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	title := strings.Join(strings.Fields(req.Title), " ")
	body := strings.TrimSpace(req.Body)
	switch {
	case utf8.RuneCountInString(title) < 3 || utf8.RuneCountInString(title) > 120:
		writeErr(w, http.StatusBadRequest, "give it a title (3–120 characters)")
		return
	case utf8.RuneCountInString(body) > 10000:
		writeErr(w, http.StatusBadRequest, "that's too long (10,000 characters max)")
		return
	case len(req.IdeaIDs) > 50:
		writeErr(w, http.StatusBadRequest, "link 50 ideas or fewer")
		return
	}
	saved, err := a.store.SaveChangelog(r.Context(), id, title, body, req.IdeaIDs, ag.ID)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "entry not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	e, err := a.store.GetChangelog(r.Context(), saved)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

// publishChangelog: the first publish marks linked ideas Complete and tells their voters.
func (a *App) publishChangelog(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var req struct {
		Published bool `json:"published"`
		Notify    bool `json:"notify"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	first, err := a.store.SetChangelogPublished(r.Context(), id, req.Published)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "entry not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	e, err := a.store.GetChangelog(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if first {
		agID := ag.ID
		for _, it := range e.Ideas {
			if it.Status != "complete" {
				done := "complete"
				if err := a.store.UpdateIdea(r.Context(), it.ID, IdeaUpdate{Status: &done}); err != nil {
					serverError(w, err)
					return
				}
				a.store.AddIdeaComment(r.Context(), it.ID, "system", &agID, "Status changed to Complete")
			}
			a.store.AddIdeaComment(r.Context(), it.ID, "system", &agID, "Shipped — announced in What's new: "+clip(e.Title, 80))
			if req.Notify && it.Approved {
				msg := fmt.Sprintf("Good news — “%s” is done.\n\n%s", it.Title, e.Title)
				if e.Body != "" {
					msg += "\n\n" + e.Body
				}
				a.notifyFollowers(it.ID, it.Title, "Done: "+clip(it.Title, 70), msg)
			}
			a.ideasChanged(it.ID)
		}
	}
	a.hub.Publish("agents", Event{Type: "changelog", Data: map[string]any{"id": id}})
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (a *App) deleteChangelog(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := a.store.DeleteChangelog(r.Context(), id); errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "entry not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// followerCount: how many people an update to this idea would email.
func (a *App) ideaFollowerCount(ctx context.Context, id int64) int {
	var n int
	a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts k WHERE k.email <> '' AND k.feedback_optout = 0 AND (
		EXISTS (SELECT 1 FROM idea_votes v WHERE v.idea_id = ? AND v.contact_id = k.id)
		OR EXISTS (SELECT 1 FROM ideas i WHERE i.id = ? AND i.contact_id = k.id))`, id, id).Scan(&n)
	return n
}
