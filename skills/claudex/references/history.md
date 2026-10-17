# claudex: history and targets

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

**Never run it in the foreground of a Codex thread.** It blocks until the pane
changes state — up to the timeout — and for that whole time the thread accepts no
input: you cannot steer, correct, or cancel. The same goes for a foreground
`claudex delegate` without `--no-wait`.

Run it **in the background**. It holds no model, spends no tokens, and does not
poll: it blocks inside Herdr, which already tracks pane state. When the pane
goes idle, done or blocked, the process prints `{"pane", "status",
"final_status"}` and exits — and your harness wakes you on that exit, with the
result already in hand.

Exit `5` means it gave up waiting and nothing is printed; the pane's state is
then unknown, not `idle`. Do not read that as "the pane finished". Exit `7` is
different again: Herdr itself failed mid-wait.
