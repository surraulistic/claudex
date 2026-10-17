---
name: claudex
description: Read what Claude Code sessions on this machine are doing and what they decided, and delegate work with tracked results. Use when asked what another session is working on, what it concluded, where a topic came up before, or to recover earlier context. For new independent work, start a native Claude Code background session with `claudex --headless`; it is tracked and returns an event to the caller without creating a Herdr pane. Use `claudex send` only to message or continue a specific existing conversation. Also reads a single entry or surrounding conversation in full.
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
claudex --headless "<task>"          # default for new independent work: a native
                                    # Claude Code background session, tracked,
                                    # result returns here without a Herdr pane
claudex --headless <project-dir> "<task>" # same, in another checkout
claudex send <target> "<message>"   # message/continue an existing conversation
claudex task list                   # what is still awaited
claudex task <id>                   # state of one assignment
claudex task log <id>               # the detailed record: commands, replies,
                                    # freshness (digest is the old name)
claudex doctor                      # is the plumbing intact
claudex session list                # who you can send to, with their ids
claudex brief --compact             # one line per pane
claudex find "<query>"              # search every transcript, including closed
```

Address a worker by **conversation id**, not by pane name:

```bash
claudex session list                     # ids live here
claudex send 7e403273 "…"                # exact, survives an agent swap
claudex send cc669134 "…"                # a known task id continues that work
claudex send license "…"                 # a pane name: convenient, but it is
                                         # only a lookup — the pane outlives
                                         # the conversation running in it
```

Herdr names and labels are human sugar and a first resolve. The working path is
the id: `claudex send license` asks herdr which conversation that pane runs
*now* and delivers there, which is right until the pane changes hands between
your reading the name and the send. The first argument is classified for you, by decreasing precision: a
conversation id addresses that conversation, a known task id continues that
work, anything else is a herdr pane name. When one string matches both a
conversation and a task, claudex refuses and shows both readings rather than
picking one — the two go to different places. `--session` exists to remove that
ambiguity in scripts; you do not need it by hand.

`session list` and `task list` answer different questions — who can take work,
versus what you already handed out. `peers` is the old name for `session list`.


## Details, read on demand

The rest of this skill lives next to it and is read only when the task needs it:

- `references/working.md` — headless by default, continuing someone's work,
  older names, a worker that forgets `done`, closed Codex threads, the supervisor.
- `references/delegating.md` — `send`/`delegate` in depth, `--notify-thread`,
  what lands in a thread, callbacks that cannot land, late reports.
- `references/history.md` — targets, reading a digest, finding earlier context,
  missing history, waiting for a pane without burning a turn.
- `references/driving.md` — typing into a live session.

Prefer plain text output; `--pretty`/`--json` cost several times more context
and are for when you need a field the text does not show.

## Exit codes

`0` success, including a degraded `history` · `2` target not found or ambiguous
— re-run `claudex sessions` · `3` Herdr unreachable · `4` bad invocation ·
`5` not finished in time · `6` pane busy, nothing sent · `7` Herdr failed while
waiting, so the outcome is unknown. Never read `7` as `5`.
