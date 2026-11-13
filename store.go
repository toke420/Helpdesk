package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var errNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

// migrations are applied in order; PRAGMA user_version records how many have run.
var migrations = [][]string{
	{
		`CREATE TABLE agents (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL UNIQUE COLLATE NOCASE,
			name TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('admin','agent')),
			password_hash TEXT NOT NULL,
			disabled INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE sessions (
			token_hash TEXT PRIMARY KEY,
			agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX sessions_agent ON sessions(agent_id)`,
		`CREATE TABLE contacts (
			id INTEGER PRIMARY KEY,
			visitor_token_hash TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT '',
			last_page TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL
		)`,
		`CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','pending','closed')),
			assignee_id INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			subject TEXT NOT NULL DEFAULT '',
			unread INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			last_message_at INTEGER NOT NULL,
			last_message_preview TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX conversations_status ON conversations(status, last_message_at)`,
		`CREATE INDEX conversations_assignee ON conversations(assignee_id, status)`,
		`CREATE INDEX conversations_contact ON conversations(contact_id, id)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			author_type TEXT NOT NULL CHECK (author_type IN ('contact','agent','system')),
			author_id INTEGER,
			body TEXT NOT NULL,
			is_note INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX messages_conversation ON messages(conversation_id, id)`,
		`CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	},
	// 2: Away mode.
	{
		// Away mode: an agent who's Away doesn't count as available.
		`ALTER TABLE agents ADD COLUMN away INTEGER NOT NULL DEFAULT 0`,
	},
	// 3: case linking.
	{
		// Case linking: a new conversation can follow an earlier case.
		`ALTER TABLE conversations ADD COLUMN related_id INTEGER`,
		`ALTER TABLE conversations ADD COLUMN related_via TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX contacts_email ON contacts(email COLLATE NOCASE)`,
	},
	// 4: case categories and close times, for the Cases page.
	{
		`ALTER TABLE conversations ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN closed_at INTEGER`,
		`UPDATE conversations SET closed_at = last_message_at WHERE status = 'closed'`,
		`CREATE INDEX conversations_created ON conversations(created_at)`,
		`CREATE INDEX conversations_category ON conversations(category, id)`,
	},
	// 5: AI — bot messages, per-case AI state, knowledge base, search indexes.
	{
		// SQLite can't alter a CHECK constraint, so rebuild messages to allow 'bot'.
		`CREATE TABLE messages_new (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
			author_type TEXT NOT NULL CHECK (author_type IN ('contact','agent','system','bot')),
			author_id INTEGER,
			body TEXT NOT NULL,
			is_note INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`INSERT INTO messages_new (id, conversation_id, author_type, author_id, body, is_note, created_at)
			SELECT id, conversation_id, author_type, author_id, body, is_note, created_at FROM messages`,
		`DROP TABLE messages`,
		`ALTER TABLE messages_new RENAME TO messages`,
		`CREATE INDEX messages_conversation ON messages(conversation_id, id)`,
		// '' = AI may answer, 'active' = AI is answering, 'handoff' = AI passed it on, 'off' = a person took over.
		`ALTER TABLE conversations ADD COLUMN ai_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN ai_replies INTEGER NOT NULL DEFAULT 0`,
		`UPDATE conversations SET ai_state = 'off' WHERE EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = conversations.id AND m.author_type = 'agent' AND m.is_note = 0)`,
		`CREATE TABLE knowledge (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			published INTEGER NOT NULL DEFAULT 1,
			source_case INTEGER REFERENCES conversations(id) ON DELETE SET NULL,
			created_by INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE VIRTUAL TABLE knowledge_fts USING fts5(title, body, content='knowledge', content_rowid='id')`,
		`CREATE TRIGGER knowledge_ai AFTER INSERT ON knowledge BEGIN
			INSERT INTO knowledge_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
		END`,
		`CREATE TRIGGER knowledge_ad AFTER DELETE ON knowledge BEGIN
			INSERT INTO knowledge_fts(knowledge_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
		END`,
		`CREATE TRIGGER knowledge_au AFTER UPDATE ON knowledge BEGIN
			INSERT INTO knowledge_fts(knowledge_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
			INSERT INTO knowledge_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
		END`,
		// Resolved cases, indexed for Copilot (rowid = conversation id). Filled on close.
		`CREATE VIRTUAL TABLE case_fts USING fts5(text)`,
	},
	// 6: detailed case states (Awaiting Manufacturer, On Hold, In Progress…).
	// status stays open/pending/closed so every view and count keeps working;
	// sub_status narrows it ('' = plain open / pending awaiting the customer).
	{
		`ALTER TABLE conversations ADD COLUMN sub_status TEXT NOT NULL DEFAULT ''`,
	},
	// 7: Activities — each agent's to-do list, optionally linked to a case.
	{
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'task' CHECK (kind IN ('task','chat','email','call','meeting')),
			priority INTEGER NOT NULL DEFAULT 0 CHECK (priority IN (0,1)),
			due_date TEXT,
			due_time TEXT,
			assignee_id INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			created_by INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			conversation_id INTEGER REFERENCES conversations(id) ON DELETE SET NULL,
			position REAL NOT NULL DEFAULT 0,
			done_at INTEGER,
			done_by INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX tasks_assignee ON tasks(assignee_id, done_at)`,
		`CREATE INDEX tasks_done ON tasks(done_at)`,
		`CREATE INDEX tasks_conversation ON tasks(conversation_id)`,
	},
	// 8: force a password change on first sign-in (set for the bootstrap admin).
	{
		`ALTER TABLE agents ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0`,
	},
	// 9: feedback board — customer ideas, votes and comments.
	{
		`CREATE TABLE ideas (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','under_review','planned','in_progress','complete','closed')),
			approved INTEGER NOT NULL DEFAULT 1,
			contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
			agent_id INTEGER REFERENCES agents(id) ON DELETE SET NULL,
			source_case INTEGER REFERENCES conversations(id) ON DELETE SET NULL,
			merged_into INTEGER REFERENCES ideas(id) ON DELETE SET NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			status_at INTEGER NOT NULL
		)`,
		`CREATE INDEX ideas_status ON ideas(status, id)`,
		`CREATE INDEX ideas_contact ON ideas(contact_id)`,
		`CREATE TABLE idea_votes (
			idea_id INTEGER NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
			contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (idea_id, contact_id)
		) WITHOUT ROWID`,
		`CREATE INDEX idea_votes_contact ON idea_votes(contact_id)`,
		`CREATE TABLE idea_comments (
			id INTEGER PRIMARY KEY,
			idea_id INTEGER NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
			author_type TEXT NOT NULL CHECK (author_type IN ('contact','agent','system')),
			author_id INTEGER,
			body TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX idea_comments_idea ON idea_comments(idea_id, id)`,
	},
}

func (s *Store) Migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range migrations[i] {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------- types ----------

type Agent struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	Away      bool   `json:"away"`
	CreatedAt int64  `json:"created_at"`
	Online    bool   `json:"online"`
	// MustChangePassword: the API is locked until this agent sets a new password.
	MustChangePassword bool `json:"must_change_password"`
	hash               string
}

type Contact struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	UserAgent  string `json:"user_agent"`
	LastPage   string `json:"last_page"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	Online     bool   `json:"online"`
}

type Conversation struct {
	ID            int64    `json:"id"`
	ContactID     int64    `json:"contact_id"`
	Status        string   `json:"status"`
	SubStatus     string   `json:"sub_status"`
	AssigneeID    *int64   `json:"assignee_id"`
	Subject       string   `json:"subject"`
	Unread        bool     `json:"unread"`
	CreatedAt     int64    `json:"created_at"`
	LastMessageAt int64    `json:"last_message_at"`
	Preview       string   `json:"preview"`
	Number        string   `json:"number"`
	RelatedID     *int64   `json:"related_id"`
	RelatedNumber string   `json:"related_number"`
	RelatedVia    string   `json:"related_via"`
	Category      string   `json:"category"`
	ClosedAt      *int64   `json:"closed_at"`
	AIState       string   `json:"ai_state"`
	AIReplies     int      `json:"ai_replies"`
	MessageCount  int      `json:"message_count,omitempty"`
	Contact       *Contact `json:"contact,omitempty"`
}

// Case numbers are what customers and staff see: HD-1001 is conversation 1.
const caseOffset = 1000

func caseNumber(id int64) string { return fmt.Sprintf("HD-%d", id+caseOffset) }

// parseCaseNumber accepts "HD-1035", "hd 1035", "#1035" or "1035".
func parseCaseNumber(s string) (int64, bool) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, "HD")
	s = strings.TrimLeft(s, "-#")
	if s == "" || len(s) > 12 {
		return 0, false
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int64(r-'0')
	}
	if n <= caseOffset {
		return 0, false
	}
	return n - caseOffset, true
}

// caseLink says which earlier case a new conversation follows, and how the
// customer picked it: "number" (typed it in) or "unsure" (their last case).
type caseLink struct {
	ID  int64
	Via string
}

type Message struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"conversation_id"`
	AuthorType     string `json:"author_type"`
	AuthorID       *int64 `json:"author_id"`
	AuthorName     string `json:"author_name"`
	Body           string `json:"body"`
	IsNote         bool   `json:"is_note"`
	CreatedAt      int64  `json:"created_at"`
}

// ---------- agents ----------

const agentCols = `id, email, name, role, password_hash, disabled, away, created_at, must_change_password`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	a := &Agent{}
	err := row.Scan(&a.ID, &a.Email, &a.Name, &a.Role, &a.hash, &a.Disabled, &a.Away, &a.CreatedAt, &a.MustChangePassword)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, err
}

func (s *Store) CountAgents(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&n)
	return n, err
}

func (s *Store) CreateAgent(ctx context.Context, email, name, role, hash string) (*Agent, error) {
	now := nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (email, name, role, password_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		email, name, role, hash, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errDuplicate
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Agent{ID: id, Email: email, Name: name, Role: role, CreatedAt: now}, nil
}

var errDuplicate = errors.New("duplicate")

func (s *Store) GetAgent(ctx context.Context, id int64) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

func (s *Store) GetAgentByEmail(ctx context.Context, email string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE email = ?`, email))
}

func (s *Store) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY disabled, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type AgentUpdate struct {
	Name               *string
	Email              *string
	Role               *string
	Disabled           *bool
	Hash               *string
	MustChangePassword *bool
}

func (s *Store) UpdateAgent(ctx context.Context, id int64, u AgentUpdate) error {
	sets, args := []string{}, []any{}
	if u.Name != nil {
		sets, args = append(sets, "name = ?"), append(args, *u.Name)
	}
	if u.Role != nil {
		sets, args = append(sets, "role = ?"), append(args, *u.Role)
	}
	if u.Disabled != nil {
		sets, args = append(sets, "disabled = ?"), append(args, *u.Disabled)
	}
	if u.Hash != nil {
		sets, args = append(sets, "password_hash = ?"), append(args, *u.Hash)
	}
	if u.Email != nil {
		sets, args = append(sets, "email = ?"), append(args, *u.Email)
	}
	if u.MustChangePassword != nil {
		sets, args = append(sets, "must_change_password = ?"), append(args, *u.MustChangePassword)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return errDuplicate
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// ---------- sessions ----------

func (s *Store) CreateSession(ctx context.Context, tokenHash string, agentID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, agent_id, expires_at) VALUES (?, ?, ?)`,
		tokenHash, agentID, expires.UnixMilli())
	return err
}

func (s *Store) SessionAgent(ctx context.Context, tokenHash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx,
		`SELECT a.id, a.email, a.name, a.role, a.password_hash, a.disabled, a.away, a.created_at, a.must_change_password
		 FROM sessions s JOIN agents a ON a.id = s.agent_id
		 WHERE s.token_hash = ? AND s.expires_at > ? AND a.disabled = 0`, tokenHash, nowMs()))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteAgentSessions(ctx context.Context, agentID int64, exceptHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE agent_id = ? AND token_hash <> ?`, agentID, exceptHash)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, nowMs())
	return err
}

// ---------- contacts ----------

const contactCols = `id, name, email, user_agent, last_page, created_at, last_seen_at`

func scanContact(row interface{ Scan(...any) error }) (*Contact, error) {
	c := &Contact{}
	err := row.Scan(&c.ID, &c.Name, &c.Email, &c.UserAgent, &c.LastPage, &c.CreatedAt, &c.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return c, err
}

func (s *Store) CreateContact(ctx context.Context, tokenHash, userAgent, page string) (*Contact, error) {
	now := nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO contacts (visitor_token_hash, user_agent, last_page, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, userAgent, page, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Contact{ID: id, UserAgent: userAgent, LastPage: page, CreatedAt: now, LastSeenAt: now}, nil
}

func (s *Store) ContactByToken(ctx context.Context, tokenHash string) (*Contact, error) {
	return scanContact(s.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts WHERE visitor_token_hash = ?`, tokenHash))
}

func (s *Store) GetContact(ctx context.Context, id int64) (*Contact, error) {
	return scanContact(s.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts WHERE id = ?`, id))
}

func (s *Store) TouchContact(ctx context.Context, id int64, userAgent, page string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE contacts SET last_seen_at = ?,
			user_agent = CASE WHEN ? <> '' THEN ? ELSE user_agent END,
			last_page = CASE WHEN ? <> '' THEN ? ELSE last_page END
		 WHERE id = ?`, nowMs(), userAgent, userAgent, page, page, id)
	return err
}

func (s *Store) UpdateContactProfile(ctx context.Context, id int64, name, email *string) error {
	sets, args := []string{}, []any{}
	if name != nil {
		sets, args = append(sets, "name = ?"), append(args, *name)
	}
	if email != nil {
		sets, args = append(sets, "email = ?"), append(args, *email)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, `UPDATE contacts SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// ---------- conversations ----------

const convCols = `c.id, c.contact_id, c.status, c.assignee_id, c.subject, c.unread, c.created_at, c.last_message_at, c.last_message_preview, c.related_id, c.related_via, c.category, c.closed_at, c.ai_state, c.ai_replies, c.sub_status`

// scanConv reads convCols, then the contact's columns when withContact, then
// any extra destinations.
func scanConv(row interface{ Scan(...any) error }, withContact bool, extra ...any) (*Conversation, error) {
	c := &Conversation{}
	dest := []any{&c.ID, &c.ContactID, &c.Status, &c.AssigneeID, &c.Subject, &c.Unread, &c.CreatedAt, &c.LastMessageAt, &c.Preview, &c.RelatedID, &c.RelatedVia, &c.Category, &c.ClosedAt, &c.AIState, &c.AIReplies, &c.SubStatus}
	var ct Contact
	if withContact {
		dest = append(dest, &ct.ID, &ct.Name, &ct.Email, &ct.UserAgent, &ct.LastPage, &ct.CreatedAt, &ct.LastSeenAt)
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	if withContact {
		c.Contact = &ct
	}
	c.Number = caseNumber(c.ID)
	if c.RelatedID != nil {
		c.RelatedNumber = caseNumber(*c.RelatedID)
	}
	return c, nil
}

const convWithContact = `SELECT ` + convCols + `, k.id, k.name, k.email, k.user_agent, k.last_page, k.created_at, k.last_seen_at
	FROM conversations c JOIN contacts k ON k.id = c.contact_id`

func (s *Store) GetConversation(ctx context.Context, id int64) (*Conversation, error) {
	return scanConv(s.db.QueryRowContext(ctx, convWithContact+` WHERE c.id = ?`, id), true)
}

// LatestConversation returns the contact's most recent conversation, or errNotFound.
func (s *Store) LatestConversation(ctx context.Context, contactID int64) (*Conversation, error) {
	return scanConv(s.db.QueryRowContext(ctx,
		`SELECT `+convCols+` FROM conversations c WHERE c.contact_id = ? ORDER BY c.id DESC LIMIT 1`, contactID), false)
}

func viewFilter(view string, agentID int64) (string, []any, bool) {
	switch view {
	case "open", "":
		return `c.status = 'open'`, nil, true
	case "pending":
		return `c.status = 'pending'`, nil, true
	case "closed":
		return `c.status = 'closed'`, nil, true
	case "mine":
		return `c.status <> 'closed' AND c.assignee_id = ?`, []any{agentID}, true
	case "unassigned":
		return `c.status <> 'closed' AND c.assignee_id IS NULL`, nil, true
	case "all":
		return `1 = 1`, nil, true
	}
	return "", nil, false
}

func likeEscape(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

func (s *Store) ListConversations(ctx context.Context, view string, agentID int64, before int64, limit int, q string) ([]*Conversation, error) {
	where, args, ok := viewFilter(view, agentID)
	if !ok {
		return nil, fmt.Errorf("unknown view %q", view)
	}
	if q = strings.TrimSpace(q); q != "" {
		// A search looks through every conversation, whatever the view.
		like := likeEscape(q)
		where = `(k.name LIKE ? ESCAPE '\' OR k.email LIKE ? ESCAPE '\' OR c.subject LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.body LIKE ? ESCAPE '\'))`
		args = []any{like, like, like, like}
		if id, ok := parseCaseNumber(q); ok {
			where = `(c.id = ? OR c.related_id = ? OR ` + where + `)`
			args = append([]any{id, id}, args...)
		}
	}
	if before > 0 {
		where += ` AND c.last_message_at < ?`
		args = append(args, before)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, convWithContact+` WHERE `+where+` ORDER BY c.last_message_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		c, err := scanConv(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ViewCounts(ctx context.Context, agentID int64) (map[string]int, error) {
	var open, pending, mine, unassigned, unread int
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status = 'open'), 0),
		COALESCE(SUM(status = 'pending'), 0),
		COALESCE(SUM(status <> 'closed' AND assignee_id = ?), 0),
		COALESCE(SUM(status <> 'closed' AND assignee_id IS NULL), 0),
		COALESCE(SUM(status <> 'closed' AND unread = 1), 0)
		FROM conversations`, agentID).Scan(&open, &pending, &mine, &unassigned, &unread)
	if err != nil {
		return nil, err
	}
	return map[string]int{"open": open, "pending": pending, "mine": mine, "unassigned": unassigned, "unread": unread}, nil
}

func preview(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) > 140 {
		r := []rune(body)
		return string(r[:140]) + "…"
	}
	return body
}

// AddContactMessage appends a customer message to one of their cases.
// targetID picks a case (it must be theirs and not closed); 0 means their
// latest case. A closed or missing case, or forceNew, starts a new one,
// linked to an earlier case when link is set.
func (s *Store) AddContactMessage(ctx context.Context, contact *Contact, body string, targetID int64, forceNew bool, link *caseLink) (*Conversation, *Message, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()
	now := nowMs()
	var convID int64
	created := false
	if !forceNew {
		q := `SELECT id, status FROM conversations WHERE contact_id = ? ORDER BY id DESC LIMIT 1`
		args := []any{contact.ID}
		if targetID > 0 {
			q = `SELECT id, status FROM conversations WHERE contact_id = ? AND id = ?`
			args = append(args, targetID)
		}
		var status string
		err := tx.QueryRowContext(ctx, q, args...).Scan(&convID, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			convID = 0
		case err != nil:
			return nil, nil, false, err
		case status == "closed":
			convID = 0
		}
	}
	if convID == 0 {
		var relID any
		via := ""
		if link != nil && link.ID > 0 {
			relID, via = link.ID, link.Via
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO conversations (contact_id, subject, created_at, last_message_at, last_message_preview, related_id, related_via) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			contact.ID, preview(body), now, now, preview(body), relID, via)
		if err != nil {
			return nil, nil, false, err
		}
		convID, _ = res.LastInsertId()
		created = true
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET last_message_at = ?, last_message_preview = ?, unread = 1,
				status = CASE WHEN status = 'pending' AND sub_status = '' THEN 'open' ELSE status END WHERE id = ?`,
			now, preview(body), convID); err != nil {
			return nil, nil, false, err
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, author_type, author_id, body, created_at) VALUES (?, 'contact', ?, ?, ?)`,
		convID, contact.ID, body, now)
	if err != nil {
		return nil, nil, false, err
	}
	msgID, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, nil, false, err
	}
	conv, err := s.GetConversation(ctx, convID)
	if err != nil {
		return nil, nil, false, err
	}
	msg := &Message{ID: msgID, ConversationID: convID, AuthorType: "contact", AuthorID: &contact.ID,
		AuthorName: contact.Name, Body: body, CreatedAt: now}
	return conv, msg, created, nil
}

// AddAgentMessage stores an agent reply or internal note. A reply to an
// unassigned conversation assigns it to the replying agent.
func (s *Store) AddAgentMessage(ctx context.Context, convID int64, agent *Agent, body string, note bool) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowMs()
	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id = ?`, convID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, author_type, author_id, body, is_note, created_at) VALUES (?, 'agent', ?, ?, ?, ?)`,
		convID, agent.ID, body, note, now)
	if err != nil {
		return nil, err
	}
	msgID, _ := res.LastInsertId()
	if !note {
		if _, err := tx.ExecContext(ctx,
			// A person replying takes the case over from the AI agent.
			`UPDATE conversations SET last_message_at = ?, last_message_preview = ?, unread = 0,
				assignee_id = COALESCE(assignee_id, ?), ai_state = 'off' WHERE id = ?`,
			now, preview(body), agent.ID, convID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Message{ID: msgID, ConversationID: convID, AuthorType: "agent", AuthorID: &agent.ID,
		AuthorName: agent.Name, Body: body, IsNote: note, CreatedAt: now}, nil
}

// AddSystemNote records an agent-only event line (status change, assignment).
func (s *Store) AddSystemNote(ctx context.Context, convID int64, body string) (*Message, error) {
	now := nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, author_type, body, is_note, created_at) VALUES (?, 'system', ?, 1, ?)`,
		convID, body, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Message{ID: id, ConversationID: convID, AuthorType: "system", Body: body, IsNote: true, CreatedAt: now}, nil
}

func (s *Store) ListMessages(ctx context.Context, convID int64, includeNotes bool) ([]*Message, error) {
	q := `SELECT m.id, m.conversation_id, m.author_type, m.author_id,
			COALESCE(CASE m.author_type WHEN 'agent' THEN a.name WHEN 'contact' THEN k.name
				WHEN 'bot' THEN COALESCE((SELECT value FROM settings WHERE key = 'ai_bot_name'), 'AI assistant') END, ''),
			m.body, m.is_note, m.created_at
		FROM messages m
		LEFT JOIN agents a ON m.author_type = 'agent' AND a.id = m.author_id
		LEFT JOIN contacts k ON m.author_type = 'contact' AND k.id = m.author_id
		WHERE m.conversation_id = ?`
	if !includeNotes {
		q += ` AND m.is_note = 0`
	}
	q += ` ORDER BY m.id LIMIT 1000`
	rows, err := s.db.QueryContext(ctx, q, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Message{}
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.AuthorType, &m.AuthorID, &m.AuthorName, &m.Body, &m.IsNote, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CaseStates maps each status an agent can pick to its stored status and
// sub_status. Agents and customers both see the full state name (caseStateLabel).
var CaseStates = map[string][2]string{
	"open":              {"open", ""},
	"in_progress":       {"open", "in_progress"},
	"pending":           {"pending", ""}, // awaiting the customer
	"pending_vendor":    {"pending", "pending_vendor"},
	"awaiting_approval": {"pending", "awaiting_approval"},
	"scheduled":         {"pending", "scheduled"},
	"on_hold":           {"pending", "on_hold"},
	"closed":            {"closed", ""},
}

// SetStatus sets a case to one of the CaseStates keys.
func (s *Store) SetStatus(ctx context.Context, convID int64, state string) error {
	st, ok := CaseStates[state]
	if !ok {
		return fmt.Errorf("unknown case state %q", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET status = ?, sub_status = ?,
			closed_at = CASE WHEN ? = 'closed' THEN ? ELSE NULL END WHERE id = ?`,
		st[0], st[1], st[0], nowMs(), convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) SetAssignee(ctx context.Context, convID int64, agentID *int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET assignee_id = ? WHERE id = ?`, agentID, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) SetCategory(ctx context.Context, convID int64, category string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET category = ? WHERE id = ?`, category, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// CaseFilter narrows the Cases page. Empty fields match everything.
type CaseFilter struct {
	Query     string // case number, subject, customer name/email, or any message text
	Status    string // open | pending | closed | active (open + pending)
	Assignee  string // me | none | <agent id>
	Category  string // none | <category>
	ContactID int64
	Since     int64 // opened at or after (unix ms)
	Before    int64 // cursor: case id
	Limit     int
}

type CaseStats struct {
	Total      int   `json:"total"`
	Open       int   `json:"open"`
	Pending    int   `json:"pending"`
	Closed     int   `json:"closed"`
	AvgCloseMs int64 `json:"avg_close_ms"`
}

func (f CaseFilter) where(me int64) (string, []any, error) {
	conds, args := []string{"1 = 1"}, []any{}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := likeEscape(q)
		search := `(c.subject LIKE ? ESCAPE '\' OR k.name LIKE ? ESCAPE '\' OR k.email LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.body LIKE ? ESCAPE '\')`
		args = append(args, like, like, like, like)
		if n, ok := parseCaseNumber(q); ok {
			search += ` OR c.id = ?`
			args = append(args, n)
		}
		conds = append(conds, search+`)`)
	}
	switch f.Status {
	case "":
	case "open", "pending", "closed":
		conds, args = append(conds, `c.status = ?`), append(args, f.Status)
	case "active":
		conds = append(conds, `c.status <> 'closed'`)
	default:
		return "", nil, fmt.Errorf("unknown status %q", f.Status)
	}
	switch f.Assignee {
	case "":
	case "me":
		conds, args = append(conds, `c.assignee_id = ?`), append(args, me)
	case "none":
		conds = append(conds, `c.assignee_id IS NULL`)
	default:
		id, err := strconv.ParseInt(f.Assignee, 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("unknown assignee %q", f.Assignee)
		}
		conds, args = append(conds, `c.assignee_id = ?`), append(args, id)
	}
	switch f.Category {
	case "":
	case "none":
		conds = append(conds, `c.category = ''`)
	default:
		conds, args = append(conds, `c.category = ?`), append(args, f.Category)
	}
	if f.ContactID > 0 {
		conds, args = append(conds, `c.contact_id = ?`), append(args, f.ContactID)
	}
	if f.Since > 0 {
		conds, args = append(conds, `c.created_at >= ?`), append(args, f.Since)
	}
	return strings.Join(conds, " AND "), args, nil
}

// SearchCases lists cases newest-opened first, with totals for the whole
// filtered set (not just this page).
func (s *Store) SearchCases(ctx context.Context, f CaseFilter, me int64) ([]*Conversation, CaseStats, error) {
	var st CaseStats
	where, args, err := f.where(me)
	if err != nil {
		return nil, st, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),
			COALESCE(SUM(c.status = 'open'), 0), COALESCE(SUM(c.status = 'pending'), 0), COALESCE(SUM(c.status = 'closed'), 0),
			COALESCE(CAST(AVG(CASE WHEN c.status = 'closed' AND c.closed_at IS NOT NULL THEN c.closed_at - c.created_at END) AS INTEGER), 0)
		FROM conversations c JOIN contacts k ON k.id = c.contact_id WHERE `+where, args...).
		Scan(&st.Total, &st.Open, &st.Pending, &st.Closed, &st.AvgCloseMs)
	if err != nil {
		return nil, st, err
	}
	pageWhere, pageArgs := where, append([]any{}, args...)
	if f.Before > 0 {
		pageWhere += ` AND c.id < ?`
		pageArgs = append(pageArgs, f.Before)
	}
	pageArgs = append(pageArgs, f.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+convCols+`, k.id, k.name, k.email, k.user_agent, k.last_page, k.created_at, k.last_seen_at,
			(SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id AND m.is_note = 0)
		FROM conversations c JOIN contacts k ON k.id = c.contact_id
		WHERE `+pageWhere+` ORDER BY c.id DESC LIMIT ?`, pageArgs...)
	if err != nil {
		return nil, st, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		var n int
		c, err := scanConv(rows, true, &n)
		if err != nil {
			return nil, st, err
		}
		c.MessageCount = n
		out = append(out, c)
	}
	return out, st, rows.Err()
}

func (s *Store) MarkRead(ctx context.Context, convID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET unread = 0 WHERE id = ?`, convID)
	return err
}

// ConversationExists reports whether a conversation with this id exists.
func (s *Store) ConversationExists(ctx context.Context, id int64) (bool, error) {
	var x int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id = ?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ContactConversations lists a customer's cases: this browser's, plus any
// from other browsers where they gave the same email address.
func (s *Store) ContactConversations(ctx context.Context, contactID int64, email string) ([]*Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+convCols+` FROM conversations c JOIN contacts k ON k.id = c.contact_id
		 WHERE c.contact_id = ? OR (? <> '' AND k.email = ? COLLATE NOCASE)
		 ORDER BY c.id DESC LIMIT 50`, contactID, email, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		c, err := scanConv(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- settings ----------

var defaultSettings = map[string]string{
	"brand_name":      "Support",
	"accent_color":    "#2f6fed",
	"greeting":        "Hi there! Ask us anything — we usually reply within a few minutes.",
	"offline_message": "We're away right now. Leave a message and your email, and we'll reply by email.",
	"categories":      "Account & access\nHardware\nNetwork & VPN\nSoftware\nEmail\nWeb Hosting\nSales Lead\nInfrastructure\nProcurement\nGeneral Troubleshooting\nOther",
	// AI agent: off | away (only when no agent is signed in) | always
	"ai_agent":           "off",
	"ai_bot_name":        "AI assistant",
	"ai_handoff_message": "I'll pass this to a teammate — they'll reply here as soon as they can. If you need to go, leave your email and we'll reply there.",
	"ai_instructions":    "",
	"ai_max_replies":     "4",
	// Feedback board: on/off, and whether customer ideas wait for an agent's OK.
	"feedback_enabled":  "1",
	"feedback_approval": "1",
	"feedback_title":    "Feedback & ideas",
}

// Categories returns the configured case categories, one per line.
func Categories(settings map[string]string) []string {
	out := []string{}
	for _, line := range strings.Split(settings["categories"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range defaultSettings {
		out[k] = v
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) PutSettings(ctx context.Context, vals map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range vals {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- away mode ----------

func (s *Store) SetAway(ctx context.Context, agentID int64, away bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET away = ? WHERE id = ?`, away, agentID)
	return err
}

// AwayAgents returns the ids of agents currently set to Away.
func (s *Store) AwayAgents(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agents WHERE away = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
