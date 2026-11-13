package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
)

// ---------- knowledge base ----------

type Article struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Published  bool   `json:"published"`
	SourceCase *int64 `json:"source_case"`
	CreatedBy  *int64 `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

const articleCols = `id, title, body, published, source_case, created_by, created_at, updated_at`

func scanArticle(row interface{ Scan(...any) error }) (*Article, error) {
	a := &Article{}
	err := row.Scan(&a.ID, &a.Title, &a.Body, &a.Published, &a.SourceCase, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, err
}

func (s *Store) ListArticles(ctx context.Context, q string) ([]*Article, error) {
	var rows *sql.Rows
	var err error
	if match := ftsQuery(q, 8); match != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+prefixCols("k.", articleCols)+`
			FROM knowledge_fts f JOIN knowledge k ON k.id = f.rowid
			WHERE knowledge_fts MATCH ? ORDER BY bm25(knowledge_fts, 4.0, 1.0) LIMIT 200`, match)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+articleCols+` FROM knowledge ORDER BY updated_at DESC LIMIT 200`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Article{}
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		a.Snippet = preview(a.Body)
		a.Body = ""
		out = append(out, a)
	}
	return out, rows.Err()
}

func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *Store) GetArticle(ctx context.Context, id int64) (*Article, error) {
	return scanArticle(s.db.QueryRowContext(ctx, `SELECT `+articleCols+` FROM knowledge WHERE id = ?`, id))
}

func (s *Store) CreateArticle(ctx context.Context, a *Article) error {
	now := nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge (title, body, published, source_case, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Title, a.Body, a.Published, a.SourceCase, a.CreatedBy, now, now)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	a.CreatedAt, a.UpdatedAt = now, now
	return nil
}

func (s *Store) UpdateArticle(ctx context.Context, a *Article) error {
	a.UpdatedAt = nowMs()
	res, err := s.db.ExecContext(ctx, `UPDATE knowledge SET title = ?, body = ?, published = ?, updated_at = ? WHERE id = ?`,
		a.Title, a.Body, a.Published, a.UpdatedAt, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) DeleteArticle(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) CountPublishedArticles(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge WHERE published = 1`).Scan(&n)
	return n, err
}

// RelevantArticles returns the best keyword matches for text. Only
// published articles are ever given to the customer-facing AI agent.
func (s *Store) RelevantArticles(ctx context.Context, text string, limit int, publishedOnly bool) ([]*Article, error) {
	match := ftsQuery(text, 16)
	if match == "" {
		return nil, nil
	}
	q := `SELECT ` + prefixCols("k.", articleCols) + ` FROM knowledge_fts f JOIN knowledge k ON k.id = f.rowid
		WHERE knowledge_fts MATCH ?`
	if publishedOnly {
		q += ` AND k.published = 1`
	}
	q += ` ORDER BY bm25(knowledge_fts, 4.0, 1.0) LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- resolved-case index (Copilot only) ----------

type CaseDoc struct {
	ID      int64
	Subject string
	Text    string
}

// IndexCase (re)indexes a case's transcript, notes included: only agents
// ever see results from this index.
func (s *Store) IndexCase(ctx context.Context, convID int64) error {
	var subject string
	if err := s.db.QueryRowContext(ctx, `SELECT subject FROM conversations WHERE id = ?`, convID).Scan(&subject); err != nil {
		return err
	}
	msgs, err := s.ListMessages(ctx, convID, true)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(subject + "\n")
	for _, m := range msgs {
		switch {
		case m.AuthorType == "system":
			continue
		case m.IsNote:
			b.WriteString("Internal note: ")
		case m.AuthorType == "contact":
			b.WriteString("Customer: ")
		default:
			b.WriteString("Support: ")
		}
		b.WriteString(m.Body + "\n")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM case_fts WHERE rowid = ?`, convID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO case_fts (rowid, text) VALUES (?, ?)`, convID, b.String()); err != nil {
		return err
	}
	return tx.Commit()
}

// BackfillCaseIndex indexes closed cases after the upgrade that adds the index.
func (s *Store) BackfillCaseIndex(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM case_fts`).Scan(&n); err != nil || n > 0 {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM conversations WHERE status = 'closed' ORDER BY id DESC LIMIT 5000`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.IndexCase(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SimilarCases(ctx context.Context, text string, exclude int64, limit int) ([]CaseDoc, error) {
	match := ftsQuery(text, 16)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.subject, f.text FROM case_fts f
		JOIN conversations c ON c.id = f.rowid
		WHERE case_fts MATCH ? AND c.id <> ? AND c.status = 'closed'
		ORDER BY bm25(case_fts) LIMIT ?`, match, exclude, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseDoc
	for rows.Next() {
		var d CaseDoc
		if err := rows.Scan(&d.ID, &d.Subject, &d.Text); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- AI agent state ----------

func (s *Store) SetAIState(ctx context.Context, convID int64, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET ai_state = ? WHERE id = ?`, state, convID)
	return err
}

// AddBotMessage stores an AI agent reply. handoff marks the case for a
// person (and unread, so it surfaces in the inbox).
func (s *Store) AddBotMessage(ctx context.Context, convID int64, body string, handoff bool) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Re-check inside the transaction: a person may have taken over meanwhile.
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT ai_state FROM conversations WHERE id = ?`, convID).Scan(&state); err != nil {
		return nil, err
	}
	if state == "off" || state == "handoff" {
		return nil, errAISuperseded
	}
	now := nowMs()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, author_type, body, created_at) VALUES (?, 'bot', ?, ?)`, convID, body, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	newState := "active"
	if handoff {
		newState = "handoff"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET last_message_at = ?, last_message_preview = ?, ai_state = ?, ai_replies = ai_replies + 1,
			unread = CASE WHEN ? THEN 1 ELSE unread END WHERE id = ?`,
		now, preview(body), newState, handoff, convID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Message{ID: id, ConversationID: convID, AuthorType: "bot", Body: body, CreatedAt: now}, nil
}

var errAISuperseded = errors.New("a person took over")

// ---------- keyword query building ----------

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are but not you your yours all any can had her was one our out has have
		him his how its may new now old see two who did get got just like that this with from they them then than there these
		those what when where which while will would could should about into over also been being more most some such only own
		same very each other because after before again does doing done hello thanks thank please hey okay yes yeah sure
		want need help know think really still since today morning afternoon evening hii hiya cheers regards dear sir madam
		i'm i've it's don't can't won't isn't`) {
		stopwords[w] = true
	}
}

// ftsQuery turns free text into an FTS5 OR-query of up to max distinct
// keywords. Each term is quoted, so user text can't inject FTS syntax.
func ftsQuery(text string, max int) string {
	seen := map[string]bool{}
	var terms []string
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		if len([]rune(w)) < 3 || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
		if len(terms) == max {
			break
		}
	}
	return strings.Join(terms, " OR ")
}
