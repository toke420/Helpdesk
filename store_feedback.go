package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ---------- Feedback board: ideas, votes, comments ----------

// IdeaStatuses in board order, with the names customers and agents see.
var IdeaStatuses = []struct{ Key, Label string }{
	{"open", "Open"},
	{"under_review", "Under review"},
	{"planned", "Planned"},
	{"in_progress", "In progress"},
	{"complete", "Complete"},
	{"closed", "Closed"},
}

func ideaStatusLabel(key string) string {
	for _, st := range IdeaStatuses {
		if st.Key == key {
			return st.Label
		}
	}
	return key
}

func validIdeaStatus(key string) bool {
	for _, st := range IdeaStatuses {
		if st.Key == key {
			return true
		}
	}
	return false
}

type Idea struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Approved    bool   `json:"approved"`
	Votes       int    `json:"votes"`
	Comments    int    `json:"comments"`
	Voted       bool   `json:"voted"`
	Mine        bool   `json:"mine"`
	AuthorName  string `json:"author_name"`
	AuthorType  string `json:"author_type"` // contact | agent
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	StatusAt    int64  `json:"status_at"`
	// Agents only (cleared before sending to customers).
	ContactID    *int64 `json:"contact_id,omitempty"`
	ContactEmail string `json:"contact_email,omitempty"`
	SourceCase   *int64 `json:"source_case,omitempty"`
	MergedInto   *int64 `json:"merged_into,omitempty"`
	AgentID      *int64 `json:"agent_id,omitempty"`
	authorFull   string
}

type IdeaFilter struct {
	Status  string // a status, "active" (not closed), or "" (everything)
	Query   string
	Sort    string // top | new
	Viewer  int64  // contact id: marks Voted/Mine, and shows their own unapproved ideas
	Agent   bool   // agents see unapproved ideas too
	Pending bool   // agents: only ideas waiting for approval
	Limit   int
}

const ideaSelect = `SELECT i.id, i.title, i.body, i.status, i.approved, i.contact_id, i.agent_id, i.source_case, i.merged_into,
		i.created_at, i.updated_at, i.status_at,
		(SELECT COUNT(*) FROM idea_votes v WHERE v.idea_id = i.id),
		(SELECT COUNT(*) FROM idea_comments m WHERE m.idea_id = i.id AND m.author_type <> 'system'),
		EXISTS (SELECT 1 FROM idea_votes v WHERE v.idea_id = i.id AND v.contact_id = ?),
		COALESCE(k.name, ''), COALESCE(k.email, ''), COALESCE(a.name, '')
	FROM ideas i
	LEFT JOIN contacts k ON k.id = i.contact_id
	LEFT JOIN agents a ON a.id = i.agent_id`

func scanIdea(row interface{ Scan(...any) error }, viewer int64) (*Idea, error) {
	it := &Idea{}
	var contactName, agentName string
	err := row.Scan(&it.ID, &it.Title, &it.Body, &it.Status, &it.Approved, &it.ContactID, &it.AgentID, &it.SourceCase, &it.MergedInto,
		&it.CreatedAt, &it.UpdatedAt, &it.StatusAt, &it.Votes, &it.Comments, &it.Voted, &contactName, &it.ContactEmail, &agentName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	it.StatusLabel = ideaStatusLabel(it.Status)
	it.Mine = viewer > 0 && it.ContactID != nil && *it.ContactID == viewer
	switch {
	case it.AgentID != nil || (it.ContactID == nil && agentName != ""):
		it.AuthorType, it.authorFull = "agent", agentName
	default:
		it.AuthorType, it.authorFull = "contact", contactName
	}
	it.AuthorName = it.authorFull
	return it, nil
}

// publicName shortens "Jane Smith" to "Jane S." for the public board.
func publicName(full string) string {
	parts := strings.Fields(full)
	switch len(parts) {
	case 0:
		return "A customer"
	case 1:
		return parts[0]
	}
	last := []rune(parts[len(parts)-1])
	return parts[0] + " " + strings.ToUpper(string(last[0])) + "."
}

// ForCustomer strips what only agents may see and shortens names.
func (it *Idea) ForCustomer(brand string) *Idea {
	out := *it
	out.ContactID, out.ContactEmail, out.SourceCase, out.MergedInto, out.AgentID = nil, "", nil, nil, nil
	if out.AuthorType == "agent" {
		out.AuthorName = brand
	} else {
		out.AuthorName = publicName(it.authorFull)
	}
	return &out
}

func (s *Store) ListIdeas(ctx context.Context, f IdeaFilter) ([]*Idea, error) {
	conds, args := []string{"i.merged_into IS NULL"}, []any{f.Viewer}
	switch {
	case f.Pending:
		conds = append(conds, "i.approved = 0")
	case !f.Agent:
		conds = append(conds, "(i.approved = 1 OR i.contact_id = ?)")
		args = append(args, f.Viewer)
	}
	switch {
	case f.Status == "":
	case f.Status == "roadmap":
		conds = append(conds, "i.status IN ('planned','in_progress','complete')")
	case f.Status == "active":
		conds = append(conds, "i.status <> 'closed'")
	case validIdeaStatus(f.Status):
		conds, args = append(conds, "i.status = ?"), append(args, f.Status)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := likeEscape(q)
		conds, args = append(conds, `(i.title LIKE ? ESCAPE '\' OR i.body LIKE ? ESCAPE '\')`), append(args, like, like)
	}
	order := "votes DESC, i.id DESC"
	if f.Sort == "new" {
		order = "i.id DESC"
	}
	if f.Limit <= 0 || f.Limit > 300 {
		f.Limit = 300
	}
	// The vote count is the 13th column; name it for ORDER BY.
	q := strings.Replace(ideaSelect, "(SELECT COUNT(*) FROM idea_votes v WHERE v.idea_id = i.id),", "(SELECT COUNT(*) FROM idea_votes v WHERE v.idea_id = i.id) AS votes,", 1)
	rows, err := s.db.QueryContext(ctx, q+` WHERE `+strings.Join(conds, " AND ")+` ORDER BY `+order+` LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Idea{}
	for rows.Next() {
		it, err := scanIdea(rows, f.Viewer)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) GetIdea(ctx context.Context, id, viewer int64) (*Idea, error) {
	return scanIdea(s.db.QueryRowContext(ctx, ideaSelect+` WHERE i.id = ?`, viewer, id), viewer)
}

// IdeaCounts: ideas per status (approved, not merged), plus how many wait for approval.
func (s *Store) IdeaCounts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{"pending": 0}
	for _, st := range IdeaStatuses {
		out[st.Key] = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT CASE WHEN approved = 0 THEN 'pending' ELSE status END, COUNT(*)
		FROM ideas WHERE merged_into IS NULL GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

type NewIdea struct {
	Title, Body string
	Status      string
	Approved    bool
	ContactID   *int64
	AgentID     *int64
	SourceCase  *int64
}

func (s *Store) CreateIdea(ctx context.Context, n NewIdea) (int64, error) {
	now := nowMs()
	if n.Status == "" {
		n.Status = "open"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO ideas (title, body, status, approved, contact_id, agent_id, source_case, created_at, updated_at, status_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, n.Title, n.Body, n.Status, n.Approved, n.ContactID, n.AgentID, n.SourceCase, now, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	// The author's own idea starts with their vote, as on Canny.
	if n.ContactID != nil {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO idea_votes (idea_id, contact_id, created_at) VALUES (?, ?, ?)`, id, *n.ContactID, now); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

type IdeaUpdate struct {
	Title, Body, Status *string
	Approved            *bool
}

func (s *Store) UpdateIdea(ctx context.Context, id int64, u IdeaUpdate) error {
	sets, args := []string{"updated_at = ?"}, []any{nowMs()}
	if u.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *u.Title)
	}
	if u.Body != nil {
		sets, args = append(sets, "body = ?"), append(args, *u.Body)
	}
	if u.Status != nil {
		sets, args = append(sets, "status = ?", "status_at = ?"), append(args, *u.Status, nowMs())
	}
	if u.Approved != nil {
		sets, args = append(sets, "approved = ?"), append(args, *u.Approved)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE ideas SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) DeleteIdea(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ideas WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// SetVote adds or removes a customer's vote; returns the new total.
func (s *Store) SetVote(ctx context.Context, ideaID, contactID int64, on bool) (int, error) {
	var err error
	if on {
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO idea_votes (idea_id, contact_id, created_at) VALUES (?, ?, ?)`, ideaID, contactID, nowMs())
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM idea_votes WHERE idea_id = ? AND contact_id = ?`, ideaID, contactID)
	}
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idea_votes WHERE idea_id = ?`, ideaID).Scan(&n)
	return n, err
}

type IdeaComment struct {
	ID         int64  `json:"id"`
	IdeaID     int64  `json:"idea_id"`
	AuthorType string `json:"author_type"` // contact | agent | system
	AuthorID   *int64 `json:"author_id,omitempty"`
	AuthorName string `json:"author_name"`
	Body       string `json:"body"`
	CreatedAt  int64  `json:"created_at"`
	Mine       bool   `json:"mine"`
}

func (s *Store) ListIdeaComments(ctx context.Context, ideaID, viewer int64) ([]*IdeaComment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.idea_id, m.author_type, m.author_id,
			COALESCE(CASE m.author_type WHEN 'agent' THEN a.name WHEN 'contact' THEN k.name END, ''), m.body, m.created_at
		FROM idea_comments m
		LEFT JOIN agents a ON m.author_type = 'agent' AND a.id = m.author_id
		LEFT JOIN contacts k ON m.author_type = 'contact' AND k.id = m.author_id
		WHERE m.idea_id = ? ORDER BY m.id LIMIT 1000`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*IdeaComment{}
	for rows.Next() {
		c := &IdeaComment{}
		if err := rows.Scan(&c.ID, &c.IdeaID, &c.AuthorType, &c.AuthorID, &c.AuthorName, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Mine = viewer > 0 && c.AuthorType == "contact" && c.AuthorID != nil && *c.AuthorID == viewer
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddIdeaComment(ctx context.Context, ideaID int64, authorType string, authorID *int64, body string) (int64, error) {
	now := nowMs()
	res, err := s.db.ExecContext(ctx, `INSERT INTO idea_comments (idea_id, author_type, author_id, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		ideaID, authorType, authorID, body, now)
	if err != nil {
		return 0, err
	}
	s.db.ExecContext(ctx, `UPDATE ideas SET updated_at = ? WHERE id = ?`, now, ideaID)
	return res.LastInsertId()
}

func (s *Store) DeleteIdeaComment(ctx context.Context, ideaID, commentID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idea_comments WHERE id = ? AND idea_id = ?`, commentID, ideaID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

type IdeaVoter struct {
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	VotedAt   int64  `json:"voted_at"`
	Cases     int    `json:"cases"`
}

func (s *Store) IdeaVoters(ctx context.Context, ideaID int64) ([]*IdeaVoter, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k.id, k.name, k.email, v.created_at,
			(SELECT COUNT(*) FROM conversations c WHERE c.contact_id = k.id)
		FROM idea_votes v JOIN contacts k ON k.id = v.contact_id
		WHERE v.idea_id = ? ORDER BY v.created_at DESC LIMIT 1000`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*IdeaVoter{}
	for rows.Next() {
		v := &IdeaVoter{}
		if err := rows.Scan(&v.ContactID, &v.Name, &v.Email, &v.VotedAt, &v.Cases); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------- merging ----------

// mergeError is a merge the agent asked for that can't be done.
type mergeError string

func (e mergeError) Error() string { return string(e) }

// MergeIdea folds src into dst: votes (deduplicated) and comments move,
// src disappears from the board, and links to it lead to dst.
func (s *Store) MergeIdea(ctx context.Context, src, dst int64) (moved int, err error) {
	if src == dst {
		return 0, mergeError("can't merge an idea into itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var srcMerged, dstMerged sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT merged_into FROM ideas WHERE id = ?`, src).Scan(&srcMerged); err != nil {
		return 0, mergeError("idea not found")
	}
	if err := tx.QueryRowContext(ctx, `SELECT merged_into FROM ideas WHERE id = ?`, dst).Scan(&dstMerged); err != nil {
		return 0, mergeError("pick an idea to merge into")
	}
	if srcMerged.Valid || dstMerged.Valid {
		return 0, mergeError("one of those ideas was already merged")
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO idea_votes (idea_id, contact_id, created_at)
		SELECT ?, contact_id, created_at FROM idea_votes WHERE idea_id = ?`, dst, src)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	now := nowMs()
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE idea_comments SET idea_id = ? WHERE idea_id = ? AND author_type <> 'system'`, []any{dst, src}},
		{`UPDATE changelog_ideas SET idea_id = ? WHERE idea_id = ? AND NOT EXISTS (SELECT 1 FROM changelog_ideas x WHERE x.idea_id = ? AND x.entry_id = changelog_ideas.entry_id)`, []any{dst, src, dst}},
		{`UPDATE ideas SET merged_into = ?, updated_at = ? WHERE id = ?`, []any{dst, now, src}},
		{`UPDATE ideas SET merged_into = ? WHERE merged_into = ?`, []any{dst, src}}, // ideas merged into src follow it
		{`UPDATE ideas SET updated_at = ? WHERE id = ?`, []any{now, dst}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}

// AddVoteFor records a vote on a customer's behalf (from a chat, say).
func (s *Store) AddVoteFor(ctx context.Context, ideaID, contactID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO idea_votes (idea_id, contact_id, created_at) VALUES (?, ?, ?)`, ideaID, contactID, nowMs())
	return err
}

// IdeasForContact: ideas a customer has voted for (shown on their cases).
func (s *Store) IdeasForContact(ctx context.Context, contactID int64) ([]*Idea, error) {
	rows, err := s.db.QueryContext(ctx, ideaSelect+` WHERE i.merged_into IS NULL AND EXISTS (SELECT 1 FROM idea_votes v WHERE v.idea_id = i.id AND v.contact_id = ?) ORDER BY i.id DESC LIMIT 50`, contactID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Idea{}
	for rows.Next() {
		it, err := scanIdea(rows, contactID)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------- who hears about an idea ----------

type IdeaFollower struct {
	ContactID int64
	Name      string
	Email     string
	Token     string // unsubscribe token
}

// IdeaFollowers: voters and the author with an email who haven't opted out.
// Each gets an unsubscribe token the first time they're emailed.
func (s *Store) IdeaFollowers(ctx context.Context, ideaID int64) ([]IdeaFollower, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k.id, k.name, k.email, COALESCE(k.notify_token, '') FROM contacts k
		WHERE k.email <> '' AND k.feedback_optout = 0 AND (
			EXISTS (SELECT 1 FROM idea_votes v WHERE v.idea_id = ? AND v.contact_id = k.id)
			OR EXISTS (SELECT 1 FROM ideas i WHERE i.id = ? AND i.contact_id = k.id))
		ORDER BY k.id LIMIT 5000`, ideaID, ideaID)
	if err != nil {
		return nil, err
	}
	out := []IdeaFollower{}
	for rows.Next() {
		var f IdeaFollower
		if err := rows.Scan(&f.ContactID, &f.Name, &f.Email, &f.Token); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Token == "" {
			tok := newToken()
			if _, err := s.db.ExecContext(ctx, `UPDATE contacts SET notify_token = ? WHERE id = ? AND notify_token IS NULL`, tok, out[i].ContactID); err != nil {
				return nil, err
			}
			if err := s.db.QueryRowContext(ctx, `SELECT notify_token FROM contacts WHERE id = ?`, out[i].ContactID).Scan(&out[i].Token); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Unsubscribe turns off feedback emails for whoever holds token.
func (s *Store) FeedbackUnsubscribe(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE contacts SET feedback_optout = 1 WHERE notify_token = ?`, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------- changelog ----------

type ChangelogEntry struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Body        string  `json:"body"`
	PublishedAt *int64  `json:"published_at"`
	CreatedBy   *int64  `json:"created_by,omitempty"`
	CreatedAt   int64   `json:"created_at"`
	UpdatedAt   int64   `json:"updated_at"`
	Ideas       []*Idea `json:"ideas"`
}

func (s *Store) ListChangelog(ctx context.Context, publishedOnly bool) ([]*ChangelogEntry, error) {
	q := `SELECT id, title, body, published_at, created_by, created_at, updated_at FROM changelog`
	if publishedOnly {
		q += ` WHERE published_at IS NOT NULL ORDER BY published_at DESC`
	} else {
		q += ` ORDER BY published_at IS NOT NULL, COALESCE(published_at, updated_at) DESC`
	}
	rows, err := s.db.QueryContext(ctx, q+` LIMIT 200`)
	if err != nil {
		return nil, err
	}
	out := []*ChangelogEntry{}
	for rows.Next() {
		e := &ChangelogEntry{Ideas: []*Idea{}}
		if err := rows.Scan(&e.ID, &e.Title, &e.Body, &e.PublishedAt, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, e := range out {
		if e.Ideas, err = s.changelogIdeas(ctx, e.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) GetChangelog(ctx context.Context, id int64) (*ChangelogEntry, error) {
	e := &ChangelogEntry{}
	err := s.db.QueryRowContext(ctx, `SELECT id, title, body, published_at, created_by, created_at, updated_at FROM changelog WHERE id = ?`, id).
		Scan(&e.ID, &e.Title, &e.Body, &e.PublishedAt, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	e.Ideas, err = s.changelogIdeas(ctx, id)
	return e, err
}

func (s *Store) changelogIdeas(ctx context.Context, entryID int64) ([]*Idea, error) {
	rows, err := s.db.QueryContext(ctx, ideaSelect+` WHERE i.id IN (SELECT idea_id FROM changelog_ideas WHERE entry_id = ?) AND i.merged_into IS NULL ORDER BY i.id`, 0, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Idea{}
	for rows.Next() {
		it, err := scanIdea(rows, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SaveChangelog creates (id 0) or updates an entry and replaces its linked ideas.
func (s *Store) SaveChangelog(ctx context.Context, id int64, title, body string, ideaIDs []int64, agentID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowMs()
	if id == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO changelog (title, body, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, title, body, agentID, now, now)
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE changelog SET title = ?, body = ?, updated_at = ? WHERE id = ?`, title, body, now, id)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, errNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM changelog_ideas WHERE entry_id = ?`, id); err != nil {
		return 0, err
	}
	for _, iid := range ideaIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO changelog_ideas (entry_id, idea_id) SELECT ?, id FROM ideas WHERE id = ?`, id, iid); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// SetChangelogPublished publishes (or unpublishes) an entry. It reports
// whether this is the first time it's ever been published, so the
// announcement (ideas marked done, voters emailed) happens only once.
func (s *Store) SetChangelogPublished(ctx context.Context, id int64, published bool) (bool, error) {
	var at sql.NullInt64
	var announced bool
	if err := s.db.QueryRowContext(ctx, `SELECT published_at, announced FROM changelog WHERE id = ?`, id).Scan(&at, &announced); errors.Is(err, sql.ErrNoRows) {
		return false, errNotFound
	} else if err != nil {
		return false, err
	}
	if published == at.Valid {
		return false, nil
	}
	var v any
	if published {
		v = nowMs()
	}
	first := published && !announced
	_, err := s.db.ExecContext(ctx, `UPDATE changelog SET published_at = ?, announced = announced OR ?, updated_at = ? WHERE id = ?`, v, first, nowMs(), id)
	return first, err
}

func (s *Store) DeleteChangelog(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM changelog WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
