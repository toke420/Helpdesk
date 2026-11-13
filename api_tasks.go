package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- Activities API ----------
//
// Every agent can see and edit the whole team's tasks (it's a small, trusting
// team, and covering for each other is the point). Deleting is limited to the
// task's creator, its assignee and admins.

func (a *App) listTasks(w http.ResponseWriter, r *http.Request, ag *Agent) {
	q := r.URL.Query()
	f := TaskFilter{Done: q.Get("scope") == "done"}
	if v := q.Get("conversation"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "bad conversation")
			return
		}
		f.ConversationID = id
	}
	switch who := q.Get("assignee"); who {
	case "", "me":
		f.AgentID = ag.ID
	case "all":
		f.All = true
	case "none":
		f.Unassigned = true
	default:
		id, err := strconv.ParseInt(who, 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "bad assignee")
			return
		}
		f.AgentID = id
	}
	if v := q.Get("done_since"); v != "" {
		f.DoneSince, _ = strconv.ParseInt(v, 10, 64)
	} else {
		f.DoneSince = nowMs() - 24*3600*1000
	}
	tasks, err := a.store.ListTasks(r.Context(), f)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// applyTaskFields validates a create or patch body and copies it onto t.
// Only keys present in the body are touched; null clears a nullable field.
func (a *App) applyTaskFields(r *http.Request, t *Task, in map[string]json.RawMessage, me *Agent) string {
	str := func(key string) (string, bool, string) {
		raw, ok := in[key]
		if !ok {
			return "", false, ""
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true, key + " must be text"
		}
		return s, true, ""
	}
	nullable := func(key string) (*string, bool, string) {
		raw, ok := in[key]
		if !ok {
			return nil, false, ""
		}
		if string(raw) == "null" {
			return nil, true, ""
		}
		s, _, p := str(key)
		if p != "" {
			return nil, true, p
		}
		if s = strings.TrimSpace(s); s == "" {
			return nil, true, ""
		}
		return &s, true, ""
	}
	id := func(key string) (*int64, bool, string) {
		raw, ok := in[key]
		if !ok {
			return nil, false, ""
		}
		if string(raw) == "null" {
			return nil, true, ""
		}
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
			return nil, true, "bad " + key
		}
		return &n, true, ""
	}

	if s, ok, p := str("title"); p != "" {
		return p
	} else if ok {
		s = strings.Join(strings.Fields(s), " ")
		switch {
		case s == "":
			return "give the task a title"
		case !utf8.ValidString(s):
			return "title is not valid text"
		case utf8.RuneCountInString(s) > 200:
			return "title is too long (200 characters max)"
		}
		t.Title = s
	}
	if s, ok, p := str("notes"); p != "" {
		return p
	} else if ok {
		s = strings.TrimSpace(s)
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 5000 {
			return "notes are too long (5000 characters max)"
		}
		t.Notes = s
	}
	if s, ok, p := str("kind"); p != "" {
		return p
	} else if ok {
		if !slices.Contains(taskKinds, s) {
			return "unknown task type"
		}
		t.Kind = s
	}
	if raw, ok := in["high"]; ok {
		if err := json.Unmarshal(raw, &t.High); err != nil {
			return "high must be true or false"
		}
	}
	if d, ok, p := nullable("due_date"); p != "" {
		return p
	} else if ok {
		if d != nil {
			if _, err := time.Parse("2006-01-02", *d); err != nil {
				return "due date must look like 2026-11-20"
			}
		}
		t.DueDate = d
	}
	if tm, ok, p := nullable("due_time"); p != "" {
		return p
	} else if ok {
		if tm != nil {
			if _, err := time.Parse("15:04", *tm); err != nil {
				return "time must look like 14:30"
			}
		}
		t.DueTime = tm
	}
	if t.DueDate == nil {
		t.DueTime = nil // a time without a day means nothing
	}
	if n, ok, p := id("assignee_id"); p != "" {
		return p
	} else if ok && !equalID(n, t.AssigneeID) {
		if n != nil {
			who, err := a.store.GetAgent(r.Context(), *n)
			if err != nil || (who.Disabled && *n != me.ID) {
				return "that teammate can't take tasks"
			}
		}
		t.AssigneeID = n
	}
	if n, ok, p := id("conversation_id"); p != "" {
		return p
	} else if ok {
		if n != nil {
			if _, err := a.store.GetConversation(r.Context(), *n); err != nil {
				return "there's no case with that number"
			}
		}
		t.ConversationID = n
	}
	return ""
}

func equalID(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (a *App) createTask(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var in map[string]json.RawMessage
	if !readJSON(w, r, &in) {
		return
	}
	t := &Task{Kind: "task", AssigneeID: &ag.ID, CreatedBy: &ag.ID}
	if _, ok := in["title"]; !ok {
		writeErr(w, http.StatusBadRequest, "give the task a title")
		return
	}
	if p := a.applyTaskFields(r, t, in, ag); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	saved, err := a.store.CreateTask(r.Context(), t)
	if err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "task", Data: saved})
	writeJSON(w, http.StatusCreated, map[string]any{"task": saved})
}

func (a *App) updateTask(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in map[string]json.RawMessage
	if !readJSON(w, r, &in) {
		return
	}
	t, err := a.store.GetTask(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	prevAssignee := t.AssigneeID
	if p := a.applyTaskFields(r, t, in, ag); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	if raw, ok := in["done"]; ok {
		var done bool
		if err := json.Unmarshal(raw, &done); err != nil {
			writeErr(w, http.StatusBadRequest, "done must be true or false")
			return
		}
		if done && t.DoneAt == nil {
			now := nowMs()
			t.DoneAt, t.DoneBy = &now, &ag.ID
		} else if !done {
			t.DoneAt, t.DoneBy = nil, nil
		}
	}
	// Moving to someone else's list, or "move to top": put it first in their list.
	var top bool
	if raw, ok := in["top"]; ok {
		json.Unmarshal(raw, &top)
	}
	if top || !equalID(prevAssignee, t.AssigneeID) {
		if t.Position, err = a.store.TopPosition(r.Context(), t.AssigneeID); err != nil {
			serverError(w, err)
			return
		}
	}
	saved, err := a.store.SaveTask(r.Context(), t)
	if err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "task", Data: saved})
	writeJSON(w, http.StatusOK, map[string]any{"task": saved})
}

func (a *App) reorderTasks(w http.ResponseWriter, r *http.Request, ag *Agent) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 500 {
		writeErr(w, http.StatusBadRequest, "send between 1 and 500 task ids")
		return
	}
	if err := a.store.ReorderTasks(r.Context(), in.IDs); err != nil {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "tasks_reordered", Data: map[string]any{"ids": in.IDs}})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteTask(w http.ResponseWriter, r *http.Request, ag *Agent) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	t, err := a.store.GetTask(r.Context(), id)
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	} else if err != nil {
		serverError(w, err)
		return
	}
	mine := (t.CreatedBy != nil && *t.CreatedBy == ag.ID) || (t.AssigneeID != nil && *t.AssigneeID == ag.ID)
	if !mine && ag.Role != "admin" {
		writeErr(w, http.StatusForbidden, "only the person who made it, its owner or an admin can delete this")
		return
	}
	if err := a.store.DeleteTask(r.Context(), id); err != nil && !errors.Is(err, errNotFound) {
		serverError(w, err)
		return
	}
	a.hub.Publish("agents", Event{Type: "task_deleted", Data: map[string]int64{"id": id}})
	w.WriteHeader(http.StatusNoContent)
}
