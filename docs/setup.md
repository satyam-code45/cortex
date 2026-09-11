# Setup: getting the credentials

Cortex talks to four outside services — OpenAI, Jira, Notion, and Google — and every one
of them needs a credential you create yourself. This is the click-by-click guide to
getting them. Budget an evening the first time; the [Quickstart](../README.md#quickstart)
itself takes about 15 minutes once these are in hand.

Everything goes in `.env` (gitignored). `.env.example` documents every variable and is the
authoritative list; this document explains where the values come from and what can go
wrong. Start by copying it:

```bash
cp .env.example .env
```

**Contents**

1. [Local tooling](#1-local-tooling)
2. [Generated secrets](#2-generated-secrets)
3. [OpenAI](#3-openai)
4. [Jira (Atlassian Cloud)](#4-jira-atlassian-cloud)
5. [Notion](#5-notion)
6. [Google Cloud, Gmail, and sign-in](#6-google-cloud-gmail-and-sign-in)
7. [Enabling write actions](#7-enabling-write-actions)
8. [Verifying it all works](#8-verifying-it-all-works)
9. [Troubleshooting](#9-troubleshooting)

---

## 1. Local tooling

| Tool | Why | Check |
|---|---|---|
| Docker + Compose | Postgres 16 with pgvector | `docker --version` |
| Go 1.23+ | the backend | `go version` |
| Node 20+ | the frontend | `node --version` |
| `make` | every command in this repo | `make --version` |

Two Go CLIs are needed for schema work. Install them once:

```bash
go install github.com/pressly/goose/v3/cmd/goose@latest
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
```

Make sure `$(go env GOPATH)/bin` is on your `PATH`, or `make migrate` will not find `goose`.

---

## 2. Generated secrets

Two values you generate rather than fetch. Do these first — they take ten seconds and the
server refuses to start without them.

```bash
openssl rand -hex 32   # -> LLM_KEY_ENCRYPTION_SECRET  (must be exactly 64 hex chars)
openssl rand -hex 24   # -> AUTH_API_TOKEN
```

- **`LLM_KEY_ENCRYPTION_SECRET`** encrypts each user's stored OpenAI key at rest
  (AES-256-GCM). It must be exactly 64 hex characters — 32 bytes. Rotating it invalidates
  every stored key, and users simply re-add theirs.
- **`AUTH_API_TOKEN`** is a static bearer token for callers that are not browsers.
  `make index` uses it, so an empty value makes the last step of the quickstart fail with a
  401.

Also set, in the same section:

```dotenv
DEV_USER_EMAIL=you@example.com      # who the bearer token acts as, and the default admin
AUTH_ALLOWED_EMAILS=you@example.com # who may sign in; see the warning below
ADMIN_EMAILS=you@example.com        # who may call the admin endpoints
```

> **Leaving `AUTH_ALLOWED_EMAILS` empty admits *any* Google account**, gated only by your
> consent screen's test-user list. That is fine while the screen is in Testing mode and
> your own address is the only test user, but put your address in explicitly and you have
> two independent gates instead of one.

---

## 3. OpenAI

1. Go to <https://platform.openai.com/api-keys> and create a secret key.
2. Put it in `.env` as `OPENAI_API_KEY`.
3. Make sure the account has credit — a new key with a $0 balance fails every call with a
   `429 insufficient_quota`, which looks like a rate limit and is not one.

```dotenv
OPENAI_API_KEY=sk-...
LLM_MODEL=gpt-4o
LLM_UTILITY_MODEL=gpt-4o-mini
EMBEDDING_MODEL=text-embedding-3-small
```

**Two keys are in play, and the split is deliberate.** The key in `.env` is the *server's*,
and it pays only for embeddings — the indexing crawl and the knowledge-base query embedder,
both of which read the server's shared index. Chat completions run on the **signed-in
user's own key**, which they add in the UI under **Settings**. So a user without a key gets
a failed run rather than a bill charged to you.

Rough costs: indexing the fixture corpus once is a few cents. A single agent run is a few
cents to about twenty. A full 20-case eval pass is roughly **$2**.

> Changing `EMBEDDING_MODEL` after indexing makes the stored vectors incomparable with new
> ones. If you change it, reindex from scratch.

---

## 4. Jira (Atlassian Cloud)

You need a site, a project to put fixtures in, and an API token.

### Create the site

1. Sign up at <https://www.atlassian.com/software/jira/free> — the free plan is enough.
2. **The site must have the Jira product installed.** A Confluence-only site 404s every
   Jira endpoint, which is a confusing failure to debug.
3. Note the site URL: `https://yourname.atlassian.net` — **no trailing slash**.

### Create a project

Create a project and note its key (e.g. `ATLAS`). The seeder writes roughly 370 issues,
comments, and transitions into it, so use a project you are happy to fill with fixtures.

### Create an API token

1. Go to <https://id.atlassian.com/manage-profile/security/api-tokens>.
2. **Create API token**, name it something like `cortex-local`, and copy it immediately —
   Atlassian shows it once.
3. The token authenticates as *you*: it carries whatever your account can do on that site.

```dotenv
JIRA_BASE_URL=https://yourname.atlassian.net
JIRA_EMAIL=you@example.com
JIRA_API_TOKEN=...
JIRA_PROJECTS=ATLAS
```

> **`JIRA_PROJECTS` is required and the server refuses to start without it.** Because
> sign-in is open to whoever your consent screen admits, an authenticated stranger must be
> structurally unable to reach projects outside the demo set. The pin is what guarantees
> that — it is applied to the agent's searches and to the indexing crawl, not merely
> checked in the UI. Comma-separate several: `JIRA_PROJECTS=ATLAS,BEACON`.

---

## 5. Notion

1. Go to <https://www.notion.so/my-integrations> and **New integration**.
2. Give it a name (`Cortex`), pick your workspace, and create it.
3. Copy the **Internal Integration Secret** — it starts `ntn_` (older ones start `secret_`).
4. **Share a page with the integration.** This is the step everyone misses: a Notion
   integration can see *nothing* by default. Open the page you want as the fixture parent,
   then **⋯ → Connections → Connect to → Cortex**.

```dotenv
NOTION_TOKEN=ntn_...
NOTION_PARENT_PAGE_ID=            # optional; see below
```

`NOTION_PARENT_PAGE_ID` is optional. Leave it empty and the seeder uses the single page
shared with the integration. Set it only when more than one page is shared and the choice
would be ambiguous. To find a page id, copy the page link — the id is the 32-character hex
string at the end, with or without dashes.

If writes are on, the integration also needs **Insert content** and **Update content**
capabilities — see [section 7](#7-enabling-write-actions).

---

## 6. Google Cloud, Gmail, and sign-in

This is the longest part. You need **one Google Cloud project** containing **two OAuth
clients**, because they do different jobs and Google will not let one do both.

| Client type | Used by | Why it must be this type |
|---|---|---|
| **Desktop** | `make gmail-auth`, `make seed` | The one-time local consent flow uses a loopback redirect, which a Web client will not accept |
| **Web application** | user sign-in, per-user Gmail connect | Takes a server redirect URI, which a Desktop client cannot |

### Create the project and enable the API

1. Go to <https://console.cloud.google.com/> and create a project (`cortex-local`).
2. **APIs & Services → Library → Gmail API → Enable.** Nothing works without this and the
   error you get otherwise says nothing about it.

### Configure the OAuth consent screen

1. **APIs & Services → OAuth consent screen.**
2. User type **External**, publishing status **Testing**.
3. Fill in the app name and your support email.
4. **Add your own Google address under *Test users*.** In Testing mode only listed test
   users can complete a flow, and the error for an unlisted user is unhelpful.
5. Add the scopes you intend to use:

   | Scope | Needed for | Tier |
   |---|---|---|
   | `.../auth/userinfo.email`, `.../auth/userinfo.profile`, `openid` | sign-in | basic |
   | `https://www.googleapis.com/auth/gmail.readonly` | the agent reading mail | **restricted** |
   | `https://www.googleapis.com/auth/gmail.insert` | the seeder placing fixture emails | **restricted** |
   | `https://www.googleapis.com/auth/gmail.labels` | the seeder creating the fixture label | sensitive |
   | `https://www.googleapis.com/auth/gmail.send` | write actions — sending mail | **restricted** |

> **What "restricted" means.** In Testing mode restricted scopes work normally for your
> listed test users, which is all a local demo needs. Publishing the app to the public
> requires Google's app verification and possibly a CASA security assessment first. So do
> not promise public Gmail connect — or public write actions — before going through that.

### Create the Desktop client

1. **APIs & Services → Credentials → Create credentials → OAuth client ID.**
2. Application type **Desktop app**.
3. Download the JSON and save it in the repo root as `gmail-credentials.json` (gitignored).

```dotenv
GMAIL_CREDENTIALS_JSON=./gmail-credentials.json
GMAIL_TOKEN_PATH=./.gmail-token.json
```

### Create the Web application client

1. **Create credentials → OAuth client ID → Web application.**
2. Add **both** authorized redirect URIs, exactly:
   - `http://localhost:8080/api/auth/google/callback`
   - `http://localhost:8080/api/connections/gmail/callback`
3. Copy the client ID and secret.

```dotenv
GOOGLE_OAUTH_CLIENT_ID=....apps.googleusercontent.com
GOOGLE_OAUTH_CLIENT_SECRET=...
```

The second redirect URI is the per-user "Connect Gmail" flow, and it is also the one the
write-consent flow returns to. A missing or mistyped URI fails with Google's
`redirect_uri_mismatch`, which at least names the problem clearly.

### Pin the mailbox scope

```dotenv
GMAIL_QUERY_SCOPE=label:vantage-labs
```

> **Required, and the server refuses to start without it.** The demo Gmail account is a
> real mailbox. Because sign-in is open, an authenticated stranger must be structurally
> unable to search outside the fixture set — so this filter is ANDed into every Gmail search
> the agent makes and into the indexing crawl. It is not a UI-level filter that could be
> bypassed.

### Authorize the mailbox once

```bash
make gmail-auth
```

This opens a browser for consent and leaves a refresh token in `.gmail-token.json` (mode
0600, gitignored). It authorizes the **demo workspace** mailbox — the one the seeder writes
fixtures into and the one demo-mode runs read. Individual users connect their own mailbox
through the UI instead.

---

## 7. Enabling write actions

By default Cortex is **read-only**: it investigates and answers, and cannot change anything
anywhere. Write actions are opt-in, per source, per user.

**Nothing is ever written without a person approving it.** The agent *proposes* a write —
recording the exact email, issue, or page it would create — the run pauses, and you see the
payload in full and approve, edit, or decline it. There is no auto-approve setting, and the
reason is concrete: everything the agent reads is text somebody else wrote, so anyone who
can file a ticket could otherwise put instructions in front of it. A human in the loop is
what stops a Jira comment becoming a way to send mail from your address.

### Settings in `.env`

```dotenv
ACTION_TTL=24h                  # how long a proposal waits before it expires
WRITES_PER_USER_PER_HOUR=20     # backstop against a runaway loop
WRITE_ACTION_WORKERS=2          # concurrency on the write queue
GMAIL_SEND_ALLOWED_DOMAINS=     # empty = any domain
```

`GMAIL_SEND_ALLOWED_DOMAINS` is worth setting while you are experimenting. With
`GMAIL_SEND_ALLOWED_DOMAINS=example.com`, a proposal addressed anywhere else is refused
when it is *proposed* — so the agent is told, and can say so in its answer, rather than you
discovering it after approving.

### Turning writes on, per source

In the UI, go to **Connections**, connect the source with your own credentials, then use
**Allow changes** on that card. Each source has a different precondition:

**Jira** — nothing new to grant. An API token already carries whatever its account can do,
so enabling writes only starts offering the write tools. Cortex does check the account
actually holds `CREATE_ISSUES`, `EDIT_ISSUES`, `ADD_COMMENTS` and `TRANSITION_ISSUES` on
the site, and refuses to enable writes without them — ask a site admin to grant them. Be
clear-eyed about the consequence: issues will be created and edited **as that account**, so
your name appears as the author.

**Notion** — the integration needs **Insert content** and **Update content**. Grant them at
<https://www.notion.so/my-integrations> → your integration → **Capabilities**. Cortex
cannot verify this in advance, because Notion exposes no endpoint that reports an
integration's capabilities and the only alternative — attempting a real write to find out —
is exactly the unattended side effect the approval gate exists to prevent. So enabling
succeeds, and a missing capability surfaces on the first real attempt as an error naming
the checkbox to tick. Cortex only ever *appends* to a page or creates a new one; it never
replaces or removes existing content.

**Gmail** — sending needs the `gmail.send` scope, which only a fresh consent screen can
grant. Add the scope to your consent screen (section 6), then click **Allow changes** on
the Gmail card: it sends you to Google, where you will see the permission named, and back.
Until you do, enabling refuses and says so. Recipients see the mail as coming from you.

You can turn writes back off at any time, and turning off never depends on the upstream
service being reachable. A proposal already approved but not yet carried out will fail
cleanly rather than execute, because the executor is gone with the permission.

### Where to see what happened

The **Actions** page is the audit trail: every write ever proposed, what you decided, when,
whether you edited it before approving, and what the upstream system returned. It is
read-only on purpose. An executed write cannot be undone — a sent email is sent — so the
record *is* the recourse.

---

## 8. Verifying it all works

```bash
docker compose up -d       # Postgres
make migrate               # application schema + queue schema
make seed-plan             # dry run: shows what seeding WOULD write, writes nothing
make seed                  # load the fixture corpus into Jira, Notion, Gmail
make dev                   # API + workers on :8080
make web                   # (second terminal) frontend on :3000
make index                 # (once the server is up) crawl + embed into the knowledge base
```

`make seed-plan` first is worth the extra minute: it catches a wrong project key or an
unshared Notion page before anything is written.

Then at <http://localhost:3000>:

1. **/login** → *Continue with Google*.
2. **Settings** → add your own OpenAI key (*Validate & save*). Chat is locked until you do.
3. **Chat** → ask *"Which issues in Project Atlas are blocked, and why?"* and watch the
   trace panel on the right.

Leave **Connections** alone at first. With zero personal connections you are in demo mode,
investigating the seeded corpus across all three sources plus the knowledge base.
Connecting your own Jira, Notion, or Gmail switches your runs to *only* your sources —
never a mix — which is the right behaviour but not the demo.

Confidence check:

```bash
make check      # build + vet + tests, backend and frontend
```

---

## 9. Troubleshooting

**`goose: command not found`** — `$(go env GOPATH)/bin` is not on your `PATH`.

**Server exits at startup naming a variable** — that variable is required. `JIRA_PROJECTS`,
`GMAIL_QUERY_SCOPE`, `OPENAI_API_KEY`, `LLM_KEY_ENCRYPTION_SECRET`, the Jira triple, the
Notion token, and the Google client pair all have to be present. The message names the one
that is missing.

**`LLM_KEY_ENCRYPTION_SECRET` rejected** — it must be exactly 64 hex characters. Regenerate
with `openssl rand -hex 32`.

**Every OpenAI call fails with a 429** — most often an account with no credit rather than a
real rate limit. Check the billing page.

**Every Jira endpoint 404s** — the site has no Jira product installed, or `JIRA_BASE_URL`
has a trailing slash.

**Jira returns 401 with a token you just made** — API tokens authenticate with your
account *email*, not your username. Check `JIRA_EMAIL`.

**Notion says a page does not exist when you can see it** — for an integration token, "not
found" almost always means "not shared with this integration". Open the page → **⋯ →
Connections → Connect to** your integration.

**`redirect_uri_mismatch`** — the Web client is missing one of the two redirect URIs, or has
a typo. They must match exactly, including the scheme and port.

**Google returns no refresh token** — a previous grant is lingering. Remove Cortex's access
at <https://myaccount.google.com/permissions> and connect again.

**Gmail connected but "send" stays off** — the consent screen was completed without the
`gmail.send` scope ticked. Add the scope to the consent screen, then use **Allow changes**
again and allow the send permission when Google asks.

**Sign-in bounces an address you own** — it is not in `AUTH_ALLOWED_EMAILS`, or not a test
user on the consent screen. Both lists have to include it.

**`make index` fails with a 401** — `AUTH_API_TOKEN` is empty. It authenticates with that
token.

**Chat is disabled with no obvious reason** — the signed-in user has no OpenAI key of their
own. Add it under **Settings**; chat deliberately spends the user's key, not the server's.
