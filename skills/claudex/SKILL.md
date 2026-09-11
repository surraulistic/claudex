---
name: claudex
description: Read what the other Claude Code sessions on this machine are doing and what they already decided, and hand work to one of them. Use when asked what another session is working on, what it concluded, where a topic came up before, or to recover context from earlier work — and whenever you are about to give a task to another session and need to know when it finished — `claudex delegate` sends the task and waits for that task, where a bare `herdr agent prompt` leaves the work untracked. Also reads a single entry or the conversation around it in full.
metadata:
  short-description: State and history of the Claude Code sessions on this machine
---

# claudex

`claudex` joins indexed session history with live Herdr state. History comes
from a local index that `claudex index` builds out of the `cass` archive
(Claude Code, Codex, Cursor and Gemini sessions in one place); live state comes
from Herdr. Neither half alone answers "what is that session doing and what did
it already decide".

Requires `claudex` in `PATH`. If it is missing, say so instead of falling back
to hand-assembly; the fallback silently reads the wrong transcript when a pane's
session has rotated.

Search is plain SQLite FTS5 over that index, so it matches any language.
`cass`'s own `cass search` does not index Cyrillic — do not reach for it.

## Start here

```bash
claudex brief             # every live pane at once — status, tail, signals
claudex index             # catch the index up; --full rebuilds from scratch
claudex tasks             # journal of everything delegated
```

The index is incremental: a catch-up costs seconds, a full rebuild about half a
minute. If a digest reports a session missing, run `claudex index` before
concluding the session is empty.

One `brief` answers "what is everyone doing" in a single process. Reaching for
`claudex <target>` per pane costs about five times as much for the same picture.

```bash
claudex sessions          # lighter: which panes exist, which have history
claudex <target>          # full digest of one pane, with history entries
```

## Targets

A target is the **tab label** (the name shown in Herdr — `install`, `slog`), a
Herdr alias, a `pane_id`, a `session_id`, a `transcript_id`, a fragment of the
title, or a cwd. Prefer the label: it is what the pane is called out loud. **An alias is not required** — every pane
in `sessions` and every group in `find` carries a `target` field that is
guaranteed to work:

```bash
claudex install           # by tab label — the usual way
claudex wE:p13            # by pane
claudex 10ad0062-f332-…   # by session id, straight from find
```

A target that is not a live pane but exists in the index returns a digest with
`live: null` and no `tail` — that is how a **closed session** is read. Nothing
else is needed to reach one.

Aliases live in Herdr and nowhere else, so a name is cleared when the agent in
that pane exits or is replaced. That is why `target` falls back to `pane_id`.
Bind a name with `herdr agent rename <pane-id> <alias>` when a stable handle is
wanted; nothing breaks without one. Exit code `2` with several candidates means
the fragment was ambiguous — narrow it, do not guess.

## Reading a digest

Compact JSON (`--pretty` for humans) in four parts:

- `live` — label, agent kind (`claude`, `codex`, …), pane, session id, status,
  context fill, usage limits, title, cwd. From Herdr.
- `history` — conversation id plus recent `entries`, newest first. From the index.
  **Check `history.stale` before you trust it.** The index is built from `cass`,
  which reads transcripts on its own schedule; an hour of lag has been observed
  with its watcher running. When `stale` is present the history is provably
  behind and must not be read as current:

  ```json
  "stale": {
    "observed_at": "2026-09-11T18:02:10+03:00",
    "behind_seconds": 4000,
    "source_newest": "2026-09-11T17:59:16+03:00",
    "cause": "source_behind_file",
    "reason": "в транскрипте есть запись от 17:59:16, а последняя проиндексированная — 16:52:36…"
  }
  ```

  `cause` is `source_behind_file` (the transcript itself holds records the index
  does not) or `journal_ahead` (a delegated task finished after the newest
  indexed record). Absent `stale` means checked and fresh, not unchecked.
  Running `claudex index` does **not** clear it: that only copies what `cass`
  already has. The live `tail` and `claudex tasks` are current regardless —
  **the task journal stays the authoritative record of completion.**
- `tail` — cleaned terminal output, read at **any** status. Which source it
  comes from depends on the pane: a `working` pane returns nothing from
  `--source recent`, so the screen is read instead; an idle one returns several
  times more scrollback from `recent`. `claudex` picks per pane — do not call
  `herdr agent read` yourself with a fixed source.
- `live.context_pct` and `live.limits` — how full the pane's context is and how
  close its usage windows are. `null` means the figure is not on screen: a narrow
  pane truncates its status line, and a non-Claude agent has none. Absent is not
  zero — do not treat a `null` pane as fresh. A pane near 100% context is about to compact:
  let it finish, do not hand it a new task. A `5h` or `7d` limit near 100% means
  it is about to stop entirely.
- `signals` — extractions, not conclusions: `mr`, `tickets`, `repo`,
  `last_user_prompt`, `current_tool_call`.

`signals` are regex hits. Report them as "the session mentions SD-8208", never
as "SD-8208 is done". Conclusions come from reading `entries`.

Signals from a narrow pane are deliberately sparse: a squeezed status line cuts
ids mid-list (`SD-6613` arrives as `SD-66`), so those lines are skipped rather
than guessed at. An empty `mr` is "nothing verifiable on screen", not "no MRs".

Each entry carries an `id`. When `truncated` is true and the content matters,
fetch the full text rather than guessing:

```bash
claudex entry <entry-id>              # the record, whole
claudex context <entry-id>            # the conversation around it
claudex context <entry-id> --before 4 --after 8
```

Both read the local index directly and never truncate.

## Finding earlier context

`find` searches every indexed transcript, including sessions that are long
closed — this is the way to answer "where did we discuss this before".

```bash
claudex find "river миграция"            # across all sessions
claudex find "wallet currency" --days 14
claudex search wE:p9 "MessageWhiz"       # within one session only
```

Hits come back grouped into `sessions`, newest first, each labelled with
`agent` (claude_code, codex, cursor, gemini), `project`, `cwd`, `hits` and the
window `first_hit`/`last_hit` — so
one call answers "which session was this, and when". Every group carries a
`target`; pass it straight to `claudex <target>` to read that session. Terms are
ANDed; a trailing `*` makes a prefix. `--raw` passes FTS5 syntax through
untouched.

Do not grep the session files to find a session. Claude Code transcripts live
under `~/.claude/projects/<slug>/<session-id>.jsonl` and Codex ones under
`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl`. If a file really must be
read directly, go to that day's directory — never sweep `~/Documents`, which
holds tens of thousands of unrelated files.

## When history is missing

`entry_count` is `null`, never `0`, when the history could not be read: `0`
would read as "the session is quiet".

`history` may arrive with a `reason` and no `entries`. Usually that means the
pane's session started after the last index pass — run `claudex index` and look
again. Say the history is unavailable and quote the reason; do not report the
session as quiet, and do not substitute a different transcript. The `tail` is
still trustworthy in that state and is often the only evidence available.

`entry_count` counts conversation only. Roughly seven of every ten records in a
session are `[Tool: …]` stubs; they are kept searchable but excluded from the
timeline. `last_activity`, by contrast, reflects any record — a pane busy
running tools is active, not idle.

## Waiting for a pane without burning a turn

```bash
claudex watch install            # blocks until the pane leaves `working`, then exits
claudex watch install --timeout 600
```

Run it **in the background**. It holds no model, spends no tokens, and does not
poll: it blocks inside Herdr, which already tracks pane state. When the pane
goes idle, done or blocked, the process prints `{"pane", "status",
"final_status"}` and exits — and your harness wakes you on that exit, with the
result already in hand.

Exit `5` means it gave up waiting and nothing is printed; the pane's state is
then unknown, not `idle`. Do not read that as "the pane finished". Exit `7` is
different again: Herdr itself failed mid-wait.

## Delegating

**Never send work with `herdr agent prompt` directly.** It delivers the prompt
and stops there: nothing waits for the pane, nothing wakes you, nothing is
logged. Correlation cannot be added afterwards — the watch has to begin with the
send, and a bare prompt does not do that.

```bash
claudex delegate install "<task>"          # send, wait, return the result
claudex delegate install "<task>" --no-wait
```

Outcomes are distinct on purpose, and both the JSON and the exit code carry
them. `outcome` is one of:

- `отчиталась` — the task reported back under its own id. This is the strong
  case: the completion is provably *this* task's, not whatever the pane happened
  to finish. `correlated: true`.
- `освободилась без отчёта` — the pane went idle but never reported. Something
  finished; you cannot tell that it was your task. `correlated: false`.
- `не уложилась в срок` — exit `5`.

How the correlation works: the prompt carries a line telling the agent to run
`claudex done <task-id> "<one line>"` as its last action. That appends to
`~/.claudex/tasks.jsonl` along with the reporting pane, taken from
`HERDR_PANE_ID`. A screen marker would not work — the pane redraws the prompt
itself, so any pattern placed in the task text matches immediately.

`claudex tasks` prints that journal: every send, every report, every outcome. It
is the authority on whether a task finished — `history` can lag behind it, and
says so via `history.stale`.

It refuses with exit `6` when the pane is neither `idle` nor `done`, before
sending anything. `blocked` is refused too, and that one matters: a blocked pane
is waiting on a human decision, and arbitrary text would arrive as the **answer
to that question**. `--force` overrides, and is only for when you were asked to.

Waiting for the pane to start is Herdr's job — the send and the wait leave in a
single `agent.prompt` call, so there is no gap for a fast answer to fall
through. Measured: a 25-second task took 34 seconds end to end, not 0.

Add `--notify <target>` to wake another pane when the task ends.

Waking waits for the target to go free — writing into a busy pane would land in
somebody else's turn. The wait lives in the watcher process and costs polling,
not tokens, so `--notify-timeout` defaults to 1800 s rather than seconds.
If the target is still busy when that runs out, the completion is **not**
dropped: it goes to the human as a Herdr notification instead. Either way the
outcome is appended to the journal as a `notified` record, and `claudex tasks`
lists at the end anything that never reached its leader.

```json
"notified": {"target": "wE:p17", "ok": false, "waited_seconds": 1800,
             "fallback": true, "reason": "ведущий не освободился…"}
```

`ok` means the leader was prompted. `fallback` means the human was told
directly. Both false means nobody was reached — read the `reason`.

**Use `--detach` unless you deliberately want to block.** Without it the call
holds your turn for as long as the pane works, and nobody can talk to you
meanwhile. A trailing `&` does **not** help: the child inherits stdout, the pipe
stays open, and the shell waits for EOF anyway.

```bash
claudex delegate install "<task>" --detach --notify "<your own pane>"
```

It returns in about 0.15 s with the watcher's `pid` and a `log` path. The
watcher runs in its own session, outlives your turn, and wakes you when the pane
settles. `--detach` requires `--notify` — a detached watcher writes to its log,
and without a notification nobody would ever read it (exit `4`).

Blocking form, when you truly want to wait and nothing else is going on:

```bash
claudex delegate install "<task>"
```

## Driving a session

`claudex` is read-only by design. Live control is Herdr, addressed by the same
target.

**Never prompt a pane whose status is not `idle`.** Herdr refuses a submission
only when the pane is `blocked`; a `working` pane silently queues the prompt and
the two tasks interleave. Check `status` first, every time.

For work that is independent of what a pane is already doing, start a **fresh
agent** instead of loading it onto a busy one:

```bash
herdr tab create --cwd <path> --label <имя>
herdr agent start <имя> --kind claude --pane <pane-id>
```

`agent start` takes the name, so the new pane is addressable immediately.


```bash
herdr agent prompt river "продолжай Payment, Prometheus не трогай" --wait --until idle
herdr agent wait river --until idle --timeout 600000
herdr agent read river
```

Sending a prompt puts work into someone's live session. Do it when the user
asked for it, and quote back what was sent.

## Exit codes

`0` success, including a degraded `history` · `2` target not found or ambiguous
— re-run `claudex sessions` · `3` Herdr unreachable · `4` bad invocation ·
`5` not finished in time · `6` pane busy, nothing sent · `7` Herdr failed while
waiting, so the outcome is unknown. Never read `7` as `5`.
