# Help Desk

A small, self-hosted live-chat help desk in the spirit of Intercom: a chat bubble on your website, and a shared inbox where 2–10 agents answer. One Go binary, one SQLite file, one container — typically 20–50 MB of RAM.

**What's in v1**

- **Chat widget** — one `<script>` tag; real-time replies; remembers the visitor; asks for an email so replies can follow them after they leave; shows whether the team is online.
- **Works on phones** — below 860px wide the sidebar becomes a slide-out menu (☰ at the top left, with a red dot when there are unread chats or tasks due). Tap outside it, swipe it left, press ✕ or pick a page to close it.
- **Shared inbox** — Open / Assigned to me / Unassigned / Pending / Closed views, live updates, unread markers, assignment (a first reply auto-assigns), internal notes customers never see, an activity trail of status and assignment changes, customer details and past conversations.
- **Case history** — every chat is a numbered case. **All cases** (agents) searches by case number, subject, customer or any message text; filters by status, agent, category and date; groups by month opened, customer, category or status; and shows totals and average time to close. Filters live in the URL, so Back from a case returns to the same view. Customers get **Your cases** in the widget: open and closed cases listed separately, full transcripts of past cases, more than one case open at once, and a "new reply" marker.
- **Categories** — admins edit the list in Settings (defaults: Account & access, Hardware, Network & VPN, Software, Email, Other); agents set one per case.
- **AI** (optional, needs a model — see below):
  - **Copilot** for agents: *Draft reply* writes the next reply from your Knowledge articles and similar resolved cases, and lists which ones it used; *Improve* tidies the agent's own draft without changing its meaning; *Summarize case* gives a 2–4 line handover; *Turn into article* drafts a Knowledge article from a closed case, with personal details stripped.
  - **AI agent** for customers (off by default): answers in the chat widget *always* or *only when no agent is signed in*, using only published Knowledge articles. It hands off to the team when the articles don't cover it, when the customer asks for a person, or after a set number of replies (default 4); handed-off cases come back unread. An agent's reply, or *Take over*, stops it for that case. It never sees internal notes, unpublished articles or other customers' cases, and treats customer text as conversation, not instructions.
  - **Knowledge**: articles the AI learns from. "AI agent can use this" controls whether the customer-facing agent may use one; Copilot uses them all. Closed cases are indexed automatically so Copilot can learn from past fixes.
- **Activities** — each agent's plan for the day. **Today** (with anything overdue on top), **Upcoming** by day, **No date** and **Done** tabs. Add a task by typing in the "Add a task…" row and pressing Enter; give it a type (to-do, chat, email, call, meeting), a due day and optional time, a high-priority flag, an owner and a linked case. Tick tasks off (they stay struck through for the rest of the day, with a progress bar), drag them — or use the arrow keys on the handle — into the order you'll tackle them, drop an overdue task on Today, or push it to tomorrow in one click. View your own list, a teammate's, Unassigned or the whole team. The sidebar shows how many of your tasks are due today and turns red when something is overdue. On a case, **Follow-up** (in the header) and the **Activities** section of the details panel add and list follow-ups for that case. Everything updates live for the whole team.
- **Email fallback** (optional, SMTP) — a reply to a customer who's left the site is emailed to them; a new message on a conversation assigned to an agent who isn't in the inbox is emailed to that agent. Throttled to one email per conversation per 5 minutes.
- **Team** — admin and agent roles, add/disable agents, password resets, at-least-one-admin safeguard.
- **Ops** — health check, one-click database backup, graceful shutdown.

## Run it

```sh
cp .env.example .env      # set HELPDESK_BASE_URL, HELPDESK_ALLOWED_ORIGINS, admin email
docker compose up -d --build
docker compose logs helpdesk   # shows the generated admin password if you left it blank
```

Open your `HELPDESK_BASE_URL`, sign in, then go to **Settings → Chat widget** to copy the embed snippet. `/demo` on the same host is a test page with the widget already on it.

The first build needs internet access to fetch Go and the SQLite driver. For reproducible builds afterwards, run `go mod tidy` once and commit the generated `go.sum`.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `HELPDESK_BASE_URL` | `http://localhost:8080` | Public URL of the help desk. `https://` turns on secure cookies. |
| `HELPDESK_ALLOWED_ORIGINS` | *(none)* | Sites that may embed the widget, comma-separated (e.g. `https://example.com`). `*` allows any. The help desk's own origin is always allowed. |
| `HELPDESK_TRUST_PROXY` | `false` | Read client IPs from `X-Forwarded-For` (set `true` behind a proxy). |
| `HELPDESK_ADMIN_EMAIL` / `HELPDESK_ADMIN_PASSWORD` | `admin@example.com` / generated | First admin, created only when there are no agents. |
| `HELPDESK_ADDR` | `:8080` | Listen address. |
| `HELPDESK_DATA_DIR` | `/data` in the image | Where `helpdesk.db` lives. |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM` | *(off)* | Outbound email. Port 587 = STARTTLS, 465 = implicit TLS. |
| `AI_PROVIDER` | `anthropic` | `anthropic` (Claude API) or `openai` (any OpenAI-compatible API: OpenAI, Ollama, OpenRouter, LM Studio). |
| `AI_API_KEY` | *(off)* | Provider key. With Claude, AI is off until this is set. A local Ollama needs none. |
| `AI_MODEL` | `claude-haiku-5-5` | Model name. Haiku is fast and cheap for chat; `claude-sonnet-5-5` writes better drafts at higher cost. For Ollama, e.g. `llama3.2:3b`. |
| `AI_BASE_URL` | provider default | For Ollama in Docker: `http://host.docker.internal:11434/v1` (add `extra_hosts: ["host.docker.internal:host-gateway"]` to the service). |

**AI and privacy:** with a hosted provider, conversation text (and, for Copilot, internal notes and similar past cases) is sent to that provider for each AI request. Use a local Ollama model if conversations must stay on your server. The AI agent's answers are only as good as your Knowledge articles; start with the ten questions you answer most.

Widget name, colour, greeting and away message are set in the inbox under **Settings**.

## Reverse proxy

Live updates use Server-Sent Events on `/api/stream` and `/api/widget/stream`, so the proxy must not buffer them.

**Caddy** works as is:

```
help.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

**Nginx:**

```nginx
server {
    server_name help.example.com;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;          # required for live updates
        proxy_read_timeout 1h;
    }
}
```

**Traefik:** add the usual router labels; no extra settings needed.

## Backups

All state is in one file. Either:

- **Settings → Download database backup** (admins), or
- from the host: `docker compose stop helpdesk && docker cp helpdesk:/data/helpdesk.db ./backup.db && docker compose start helpdesk` (a clean stop folds the write-ahead log into the main file).

To restore, stop the container and put the backup file in the volume as `helpdesk.db`.

If you bind-mount a host folder instead of the named volume, make it writable by UID 65532 (`chown -R 65532:65532 ./data`).

## Security notes

- Agent passwords: PBKDF2-SHA256, 600k iterations. Sessions: random tokens, stored hashed, HTTP-only `SameSite=Lax` cookies, 30-day expiry; changing a password signs out other sessions.
- Agent write requests require an `X-Helpdesk` header, which blocks cross-site request forgery.
- Visitors get a random token kept in the browser's localStorage and sent as a bearer header (never in URLs). Visitors only ever see their own conversation and never see internal notes.
- Rate limits on login and on widget endpoints; request bodies capped at 64 KB, messages at 5,000 characters.

## Code map

| File | What |
| --- | --- |
| `main.go` | Config, startup, first admin, graceful shutdown, `healthcheck` subcommand |
| `store.go` | SQLite schema, migrations (`PRAGMA user_version`) and queries |
| `app.go` | Routes, middleware (CORS, auth, CSRF guard, security headers), helpers |
| `api_widget.go` / `api_agent.go` | Customer and agent endpoints |
| `hub.go` | In-memory pub/sub + Server-Sent Events |
| `ai.go` / `api_ai.go` / `store_ai.go` | Model client (Claude + OpenAI-compatible), prompts, Copilot, AI agent, Knowledge, keyword search (SQLite FTS5) |
| `api_tasks.go` / `store_tasks.go` | Activities: tasks, ordering, permissions |
| `mail.go` | SMTP notifications |
| `auth.go` | Password hashing, tokens, rate limiter |
| `web/` | Inbox app (`index.html`, `assets/`) and widget (`widget/`), embedded into the binary |

Local development: `go run .` (needs Go 1.24+), then open http://localhost:8080.

## Upgrading

Database changes apply automatically on start (tracked with SQLite's `user_version`). Back up `helpdesk.db` first, then `docker compose up -d --build`. Upgrading from v1 adds case categories and close times; cases that were already closed get their last message time as their close time. Upgrading to the AI release adds the Knowledge base, indexes your closed cases for Copilot on first start, and marks cases an agent already answered so the AI agent never jumps into them. Upgrading to the Activities release adds an empty task list; nothing else changes.

## Roadmap

v2 email-to-conversation (IMAP), canned replies, attachments, search · v3 knowledge base, CSAT, tags · v4 reports, round-robin assignment, office hours, Slack/Discord webhooks.
