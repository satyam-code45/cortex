# Cortex demo script

Three questions of increasing difficulty against the seeded corpus (fictional company **Vantage Labs**, mid-way through the "Atlas" payments launch). Setup: signed in, personal OpenAI key added, **zero personal connections** (demo mode — all three seeded sources + knowledge base). Total run time: 2–3 minutes.

## Question 1 — warm-up, single source

> **Which issues in Project Atlas are blocked, and why?**

- **Sources touched:** Jira only.
- **Expected behavior:** Jira tool calls only — it lists projects, searches, then reads the comment threads on each candidate (about a dozen calls; the reasons live in comments, not in the issue fields). No Notion/Gmail calls needed.
- **Expected answer:** four blocked issues — the refunds API integration and the chargeback webhook receiver blocked directly on the payment provider (undelivered v3 refunds sandbox; unpublished v3 webhook signing spec), and the reconciliation nightly job plus the support-agent refund API blocked transitively on the refunds integration.
- **What to point at:** citations resolve to the actual Jira issues; the trace panel shows each tool call with arguments and latency.

## Question 2 — the data Jira doesn't have

> **Who at the payment vendor told us about the delay, and when?**

- **Sources touched:** Notion + Gmail. Jira records neither the vendor's name nor the contact — that's the point.
- **Expected behavior:** the agent searches Notion, finds the Atlas Q2 Plan naming **Nordwind Payments** and **Ines Brandt** as the escalation contact, then pivots to Gmail and finds the delay notice.
- **Expected answer:** Ines Brandt, Partner Success Manager at Nordwind Payments, by email on **2026-06-08** (a second revision followed 2026-07-09, and the "sandbox is live" confirmation 2026-08-05).
- **What to point at:** the pivot in the trace panel — a Notion page read followed by a Gmail search built from what it just learned.

## Question 3 — the flagship: cross-source root cause

> **Why was the payment integration delayed and how did it affect the launch date?**

- **Sources touched:** all three. No single source holds the chain: Jira has the blocked dependency graph but never names the vendor; Notion names Nordwind, the original 2026-06-30 launch date, and the 29 May sandbox commitment; only Gmail has the root cause and the decision thread.
- **Expected behavior:** multi-hop investigation across Jira → Notion → Gmail, visible live in the trace panel.
- **Expected answer:** the Atlas billing launch slipped **2026-06-30 → 2026-08-14** because Nordwind Payments' settlement-ledger migration overran, so the v3 refunds sandbox (committed for 29 May) never reached production parity. Ines Brandt gave notice by email on 2026-06-08 with no firm date; the refunds integration couldn't be certified (blocking reconciliation and the support-agent refund API); Marcus Whitfield recommended the new date on 2026-06-11 and Fatima Al-Rashid approved it on 2026-06-12.
- **What to point at:** the answer's citation list spans all three sources; click a citation to land on the underlying evidence. Afterwards, click the finished answer to replay its full trace.

**Encore, if asked "what if it doesn't know?":** ask *"What budget has been allocated to Project Atlas for Q3 2026?"* — the corpus contains no budget figures, and the agent says so instead of inventing a number.

## Recording checklist (2–3 min screen capture)

1. Start on `/login`, sign in with Google. (~10s)
2. Flash `/sources` — the indexed corpus with counts per source. (~10s)
3. Question 1 on `/` — let the trace panel run, read out the answer. (~40s)
4. Question 3 — the main event. Narrate the source pivots as they stream; end on the citations. (~60–80s)
5. Click a citation, show the evidence. Click the earlier answer, show the replayed trace. (~20s)

Save the capture as the primary pitch artifact; take the trace-panel screenshot for the README (`docs/trace-panel.png`) from question 3's live run.

## Fresh-clone checklist

Executed literally on a clean checkout, following only the README. Target: working browser demo in under 15 minutes with credentials in hand.

- [x] Fresh checkout in an empty directory (2026-08-30)
- [x] `cp .env.example .env`, fill keys — every variable's comment is enough to know what goes in it
- [x] `docker compose up -d` brings up Postgres
- [x] `make migrate` applies application + queue schema cleanly on the empty database
- [x] `make gmail-auth` — one-time interactive consent; verified the flow starts and waits for the browser (a previously cached token stood in for completing it headlessly)
- [x] `make seed` loads Jira, Notion, and Gmail fixtures — verified idempotent: second run reported Notion `created=0 skipped=5`, Gmail `inserted=0 skipped=16`, fixtures verified searchable
- [x] `make dev` + `make web` start; `make index` indexed all three sources (jira 99, notion 6, gmail 16 documents)
- [x] Key added (validated live against OpenAI); browser Google sign-in exercised separately since it needs an interactive consent screen
- [x] Demo question 1 answered with 11 citations to real Jira URLs after 12 tool calls; the run's SSE event stream replays in full
- [x] Elapsed time from checkout to answered question: **8 minutes**, including diagnosing the two issues the test caught (target < 15 min)

Two breaks found and fixed during the test: the seeder's deliberately unscoped Gmail client predated the query-scope hardening and was refused by it (fixed by declaring `AllowUnscoped`), and the README originally listed `make index` before `make dev` even though indexing is triggered through the running server's admin endpoint (reordered).
