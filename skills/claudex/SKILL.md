---
name: claudex
description: Get the state of a parallel Claude Code session by alias (river, config, entitlements) in one call, or drive it. Use when the user asks what another session is doing, what it decided, or wants a prompt sent to it. Not for searching general project history — that is plain contextify.
metadata:
  short-description: State of a parallel Claude Code session by alias
---

# claudex

`claudex` collapses six manual calls into one: it joins Contextify history with
live Herdr state for a named Claude Code session. Prefer it over assembling
`contextify activity` and `herdr agent read` by hand — the join key it uses
(`transcripts.provider_session_id`) is not reachable from either CLI.

Requires `claudex` in `PATH`. If it is missing, say so instead of falling back
to hand-assembly; the fallback silently reads the wrong transcript when a pane's
session has rotated.

## Start here

```bash
claudex sessions          # which aliases exist right now
claudex river             # the digest for one alias
```

Aliases are Herdr agent names, not stored anywhere. Herdr clears a name when the
agent in that pane exits or is replaced, so an alias that worked an hour ago may
be gone. `sessions` lists such panes under `unaliased` with their `pane_id`;
re-bind with `herdr agent rename <pane-id> <alias>`.

## Reading the digest

Output is compact JSON (`--pretty` for humans) with four parts:

- `live` — pane, session id, status, title, cwd. From Herdr.
- `history` — transcript id plus recent `entries`, newest first. From Contextify.
- `tail` — cleaned terminal output, **only when `live.status` is `working`**.
  `null` on an idle pane is normal, not a failure: an idle Claude pane shows its
  input box, not the conversation.
- `signals` — extractions, not conclusions: `mr`, `tickets`, `repo`,
  `last_user_prompt`, `current_tool_call`.

`signals` are regex hits. Report them as "the session mentions SD-8208", never
as "SD-8208 is done". Conclusions come from reading `entries`.

Each entry carries an `id`. When `truncated` is true and the content matters,
fetch the full text rather than guessing:

```bash
contextify entry <entry-id>      # full record
contextify context <entry-id>    # surrounding conversation
```

## When history is missing

`history` may arrive with a `reason` and no `entries`. This is a real condition,
not an empty session — Contextify rejects a transcript whose first 20 lines lack
`uuid`/`timestamp`, and marks it done, so it never recovers on its own. Say the
history is unavailable and quote the reason. Do not report the session as quiet,
and do not substitute a different transcript.

## Digging further

```bash
claudex river --limit 12 --chars 600     # deeper slice
claudex search config "MessageWhiz"      # full-text within that session only
```

## Driving a session

`claudex` is read-only by design. Live control is Herdr, addressed by the same
alias:

```bash
herdr agent prompt river "продолжай Payment, Prometheus не трогай" --wait --until idle
herdr agent wait river --until idle --timeout 600000
herdr agent read river
```

Sending a prompt puts work into someone's live session. Do it when the user
asked for it, and quote back what was sent.

## Exit codes

`0` success, including a degraded `history` · `2` unknown alias — re-run
`claudex sessions` · `3` Herdr unreachable · `4` bad invocation.
