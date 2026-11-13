package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- Copilot ----------

func (a *App) copilotAction(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	if !a.llm.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "AI isn't set up on the server yet (see Settings → AI)")
		return
	}
	var req struct {
		Action string `json:"action"`
		Draft  string `json:"draft"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !a.limit.Allow(fmt.Sprintf("copilot:%d", ag.ID), 30, time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "slow down — too many Copilot requests")
		return
	}
	res, err := a.copilot(r.Context(), conv, ag, req.Action, req.Draft)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown action") || err.Error() == "write a draft first" {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// setAIMode lets an agent stop the AI agent on a case ("off") or hand it back ("auto").
func (a *App) setAIMode(w http.ResponseWriter, r *http.Request, ag *Agent) {
	conv, ok := a.loadConversation(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	state, text := "", ""
	switch req.Mode {
	case "off":
		state, text = "off", ag.Name+" took over from the AI agent"
	case "auto":
		state, text = "", ag.Name+" let the AI agent answer again"
	default:
		writeErr(w, http.StatusBadRequest, "mode must be off or auto")
		return
	}
	if err := a.store.SetAIState(r.Context(), conv.ID, state); err != nil {
		serverError(w, err)
		return
	}
	if note, err := a.store.AddSystemNote(r.Context(), conv.ID, text); err == nil {
		a.hub.Publish("agents", Event{Type: "message", Data: note})
	}
	a.broadcastConversation(r, conv.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- knowledge base ----------

func (a *App) listArticles(w http.ResponseWriter, r *http.Request, ag *Agent) {
	arts, err := a.store.ListArticles(r.Context(), truncate(r.URL.Query().Get("q"), 200))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"articles": arts})
}

func (a *App) getArticle(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	art, err := a.store.GetArticle(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "article not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"article": art})
}

type articleInput struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	Published  *bool  `json:"published"`
	SourceCase *int64 `json:"source_case"`
}

func (in *articleInput) clean() string {
	in.Title = strings.Join(strings.Fields(in.Title), " ")
	in.Body = strings.TrimSpace(in.Body)
	switch {
	case in.Title == "":
		return "add a title"
	case utf8.RuneCountInString(in.Title) > 150:
		return "title is too long (150 characters max)"
	case in.Body == "":
		return "the article is empty"
	case utf8.RuneCountInString(in.Body) > 20000:
		return "article is too long (20,000 characters max)"
	case !utf8.ValidString(in.Body):
		return "article is not valid text"
	}
	return ""
}

func (a *App) createArticle(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var in articleInput
	if !readJSON(w, r, &in) {
		return
	}
	if p := in.clean(); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	art := &Article{Title: in.Title, Body: in.Body, Published: in.Published == nil || *in.Published, CreatedBy: &ag.ID}
	if in.SourceCase != nil && *in.SourceCase > 0 {
		if _, err := a.store.GetConversation(r.Context(), *in.SourceCase); err == nil {
			art.SourceCase = in.SourceCase
		}
	}
	if err := a.store.CreateArticle(r.Context(), art); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"article": art})
}

func (a *App) updateArticle(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in articleInput
	if !readJSON(w, r, &in) {
		return
	}
	if p := in.clean(); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	art, err := a.store.GetArticle(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "article not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	art.Title, art.Body = in.Title, in.Body
	if in.Published != nil {
		art.Published = *in.Published
	}
	if err := a.store.UpdateArticle(r.Context(), art); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"article": art})
}

func (a *App) deleteArticle(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := a.store.DeleteArticle(r.Context(), id); errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "article not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
