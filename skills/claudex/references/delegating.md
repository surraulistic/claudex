# claudex: delegating and delivery

## Delegating

`claudex delegate <target> "<task>"` addresses a **conversation**, not a terminal.

The target still names a herdr pane, because that is how you refer to a worker.
What changes is where the task goes: claudex asks herdr which conversation the
pane runs right now, and delivers to that conversation's inbox socket. The pane
outlives the agent inside it, so "the pane is free" proves nothing about the
conversation — that mismatch is what `executor_conversation_changed` was built
to catch, and addressing the conversation removes it at the source.

```bash
claudex delegate license "…"            # pane named, conversation addressed
claudex delegate --session <id|name> "…" # address a conversation directly
claudex delegate --panel license "…"     # legacy: write into the herdr pane
```

It refuses rather than guesses. A wrong guess delivers work into somebody
else's conversation, so every unresolved case is a refusal with a
machine-readable cause:

| cause | when |
|---|---|
| `pane_target_not_claude` | the pane runs something other than Claude Code |
| `pane_identity_unproven` | herdr does not report the pane's conversation |
| `pane_conversation_absent` | that conversation is no longer live |
| `pane_conversation_ambiguous` | the name matches more than one conversation |
| `pane_is_current_conversation` | the pane runs this very conversation |

`--panel` keeps the previous transport for the cases the socket cannot serve:
a pane running a non-Claude agent, or one whose conversation has closed.

Everything else is unchanged and shared by both transports: `--notify-thread`
and `--notify-session`, the journal binding, correlation of a report to its
task, deduplication, `reconcile`, and the pull channel behind `undelivered`.
A task resolved from a pane records **both** the pane and the conversation, so
delivery goes by conversation while `reconcile` keeps the right to read the
pane's screen.


**Never send work with `herdr agent prompt` directly.** It delivers the prompt
and stops there: nothing waits for the pane, nothing wakes you, nothing is
logged. Correlation cannot be added afterwards — the watch has to begin with the
send, and a bare prompt does not do that.

```bash
# default: send and return at once; the report arrives later as its own message
claudex delegate install "<task>" --no-wait --notify-thread "$CODEX_THREAD_ID"

claudex delegate install "<task>"          # blocks this thread until the task ends
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

When nobody could be reached at all — the leader's pane holds a different
conversation *and* Herdr declines to show a notification — the completion is not
dropped. It surfaces as a top-level `undelivered` array in `claudex sessions`
and `claudex brief`, carrying the report text itself:

```json
"undelivered": [{"task": "fd6cc93f", "stage": "reported", "target": "wE:p17",
  "at": "…", "reason": "не доставлено (herdr: busy): … другой разговор …",
  "report": "готово 18 из 21 сервиса с settings.yaml"}]
```

That is a pull channel: you already call `sessions` constantly, so nothing
depends on a toast being rendered. The key is absent when there is nothing to
say. Read it and pass the result on — that is the whole point of it being there.

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

Add `--notify <target>` to wake another pane when the task ends. Without it,
`--detach` wakes **the session that started the delegation** (`HERDR_PANE_ID`) —
waking anyone else has to be asked for by name.

The wake is bound to a conversation, not to a pane, and the binding is **closed
by default**. The target's session id is captured when the task is created and
must match exactly before anything is written. A pane outlives the agent in it,
several leader sessions run side by side, and an hour later that pane may hold
one of the others.

Nothing is written unless the binding is proven. An unknown session id on either
side means *cannot confirm*, not *probably the same* — so a task started by one
session can never land in another. All three refusals go to the human instead,
each naming its reason:

- `в … теперь другой разговор (… вместо …)` — the pane moved on;
- `herdr не сообщает, какой разговор сейчас в …` — no identity to compare with;
- `при заведении поручения разговор в … не был записан` — never bound.

`delegate` reports the binding up front as `wake.session_bound`, so a task that
can never be delivered is visible at once rather than half an hour later.

**A matching session id is necessary but not sufficient.** Measured: Codex runs
several conversations at once behind one pane, and Herdr names only one of them
in `agent_session`. A report bound to the id Herdr reported was delivered — and
landed in a different conversation that was never told about the task. So a pane
the journal has seen host more than one conversation is not an address at all:
delivery is refused with `cause: pane_hosts_several_conversations`, whatever the
ids say. `go run ./cmd/routecheck` prints, per pane, whether a report would be
written there and why not.

Every refusal carries a machine-readable `cause`: `wrong_conversation`,
`unconfirmed_binding`, `pane_hosts_several_conversations`, `leader_busy`. None of
them counts as delivered, and none of them writes into the live conversation —
the fallback is a Herdr notification to the human, never a message in somebody's
thread. Undelivered work is listed by `claudex tasks`, which a person reads; the
pane listings deliberately carry nothing about other conversations' tasks.

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

A watcher lives only until its own deadline. A task that overruns it keeps
working and reports later, and **that late report is delivered too**: `claudex
done` checks whether the task was already closed and, if so, wakes the leader
itself or tells the human. A deadline is not a final state.

Delivery is deduplicated per task **and stage** — `finished` (what the watcher
saw, a timeout included) and `reported` (the real report that arrived later) are
different events, each delivered once. Running `done` twice does not wake anyone
twice; a delivery that reached nobody is retried on the next `done`. `claudex
tasks` lists undelivered stages as `<task>/<stage>`.

**Default: send in the background and let the report come back as a message.**

```bash
claudex delegate install "<task>" --no-wait --notify-thread "$CODEX_THREAD_ID"
```

This returns immediately. Your thread stays open to input the whole time the task
runs — you can keep working, steer, or cancel. When the task finishes, `claudex
done` queues the report **into this same conversation** as an ordinary message,
carrying the digest described below. The address is the thread, so the report
cannot land anywhere else.

Omit `--notify-thread` when `CODEX_THREAD_ID` is set: your own thread is chosen
automatically.

`exec` + `wait` is the exception, not the rule. It holds a cell you must come back
to, and in Codex `wait` is pull-based polling — a long wait is billed as repeated
turns, not as idle time. Reach for it only when the very next thing you do depends
on the result and there is nothing else to get on with:

```
exec("claudex delegate install '<task>'", yield_time_ms: 5000)   → cell_id
wait(cell_id, yield_time_ms: 600000)   → claudex JSON, in this conversation
```

**Do not use `--detach --notify` for this.** It severs that parent-child link and
leaves claudex to route by pane, which cannot work: Codex runs several
conversations behind one pane and Herdr names only one of them. Measured — a
report bound to the id Herdr reported was delivered into a conversation that had
never heard of the task. `--detach` remains only for work whose result a **human**
will read, never as a way to wake a conversation.

### `--notify-thread`: the one address that is not a pane

`--notify-thread <id>` puts the report into a Codex conversation's own queue via
`codex queue --thread`. The address **is** the conversation, so the pane problem
above does not arise: there is nothing to confuse it with.

```bash
claudex delegate install "<task>" --notify-thread "$CODEX_THREAD_ID"
claudex delegate install "<task>" --detach --notify-thread "$CODEX_THREAD_ID"
```

Take the id from `CODEX_THREAD_ID`, which Codex exports into the environment.
When it is set, you may omit the flag entirely: your own thread is chosen
automatically and **outranks** your own pane, because several conversations sit
behind a pane and exactly one behind a thread.

This is the only case where `--detach` is a correct way to wake a conversation: a
thread's queue does not depend on the parent-child link that `--detach` severs.
Prefer plain `exec` + `wait` when you are going to wait anyway — it costs nothing
and needs no thread id. Reach for `--notify-thread` when you must not hold the
handle: detached work, or a task whose result should reach you even if this turn
ends first.

Three refusals, each naming its cause, none of them counted as delivery:

- `thread_not_live` — the thread is known but closed; a queued message there
  would never be read, so this is a refusal, not a success;
- `thread_unknown` — `$CODEX_HOME` knows nothing about that id;
- `unconfirmed_binding` — nothing was bound, or a **session name** was passed
  instead of an id. Names are not addresses: three threads in the session index
  are called `license service`.

Check what would happen right now, without sending anything:

```bash
go run ./cmd/routecheck --threads
```

If the handle is lost — harness restarted, `wait` abandoned — pull the task by
the id `delegate` printed, which only this conversation holds:

```bash
claudex tasks --task 742309b7
```


### What arrives in the thread is a summary, not Claude's answer

Claude is the messenger here, not the author of record. What ClauDex queues into
your thread is a **skeleton**: the outcome, what was actually done, and what got
in the way. It deliberately leaves out what you already have — the prompt you
wrote, the assistant's own phrasing, the pane's screen — and names the command
that fetches the rest. Measured: pushing the full view cost ~3600 tokens per
task; the skeleton costs ~220.

Arriving automatically, bounded to the task window:

- **what was delegated** — the prompt as it was recorded, not as you remember it;
- **the work** — tool calls and their results: commands run, files edited, tests;
- **what was said** — the assistant's own reasoning turns, clipped;
- **a live pane tail** read from herdr, so it is fresh even when the index is not;
- **freshness and coverage** — how far the cass index lags, and whether it reached
  the task window at all.

Two of those deserve care. A digest whose coverage line says the index did not
reach the window has **empty work sections on purpose**: ClauDex would rather show
nothing than pass off older, unrelated work in the same conversation as this
task's. And a `dropped` count means a long session was trimmed to a budget — what
you see is a sample, not the whole run.

Still requiring an explicit read, because no digest can carry it honestly:

- the **full transcript** — `claudex digest <task-id>` for the task-scoped view,
  `claudex <target>` for the pane as a whole;
- the **repository** — `git -C <repo> log`, `git status`, `git show <sha>`. A
  digest reports that a commit was made; only the repo proves what is in it;
- anything **outside the task window**, which the digest deliberately excludes;
- anything the index has not caught up on yet — the freshness line tells you when
  that is the case, and the live tail is then your only fresh evidence.

The rule of thumb: treat the digest as a briefing that tells you where to look,
and verify in the repository anything you are about to build a decision on.

### You are not required to remember the task id

`claudex done` no longer trusts the id you type. It reads `HERDR_PANE_ID`, looks
up which task is actually live for that pane, and files the report there.

```bash
claudex done <task-id> "<one line>"   # normal
claudex done "<one line>"             # id omitted — resolved from the pane
```

This exists because the id you remember goes stale. A session that has been
running for hours and has been compacted remembers the **first** id it ever saw,
not the current one. Measured on the live journal: one pane filed seventeen
reports against a task that had been closed that morning, while the task actually
running got none — and the leader was never woken.

What ClauDex does with what you type:

- **live id for your pane** — filed as given;
- **stale, unknown, foreign, or a whole report pasted where the id goes** — filed
  against your pane's one live task instead, and the journal records both what you
  typed and why it was changed (`claimed_task`, `correction`);
- **your own already-reported id, nothing else live** — kept as is; a repeated
  `done` is how a delivery that reached nobody gets retried;
- **nothing live, or several live at once** — refused with the candidates named.
  Nothing is written. Guessing here would produce a false report, which is worse
  than a missing one because it looks real.

So type the id if you have it, and do not agonise if you don't. What you must not
do is invent one: an id you are unsure about is worse than no id at all, because a
wrong-but-plausible id can match a real closed task.

### When the callback cannot land

A thread that was alive when the task was created can be closed by the time the
report is ready. ClauDex refuses to queue into a closed conversation — a message
there is read by nobody — and it never substitutes a different live thread: the
address *is* the conversation, and a neighbour is not a stand-in.

The report is not lost. It stays in an explicit pull channel:

```bash
claudex undelivered          # every report the leader never got, in full
claudex undelivered --pretty # same as JSON
```

Each entry carries the task, the intended address, the machine-readable cause
(`thread_not_live`, `thread_unknown`, `queue_failed`, `wrong_conversation`, …)
and the **whole report text**, not a summary of it.

Two things follow. A popup shown to the human does **not** count as delivery —
the human reads it when they are at the machine; the conversation still never
learns the task ended. And because it does not count, the next `claudex done`
retries the delivery: a conversation that has since reopened gets the report on
that retry. The human is only shown the popup once per stage, so retrying is
cheap and quiet.

### `find` searches twice: semantically through cass, and lexically

`claudex find` asks cass for a hybrid (semantic + lexical) search and merges the
answer with its own FTS5 index. Two engines disagree usefully: semantic finds the
paraphrase you half-remember, FTS5 finds the exact rare token you typed.

```bash
claudex find "почему отчёт не дошёл"              # auto (default)
claudex find "<query>" --mode lexical             # no cass at all
claudex find "<query>" --mode semantic --limit 20
CLAUDEX_SEARCH=lexical claudex find "<query>"     # same, via environment
```

Every result carries `via`: `cass`, `fts5`, or `both`. `both` means two
independent engines agreed — a stronger signal than either alone. Lexical-only
hits keep reserved slots in the output, so a large `--limit` cannot crowd them
out.

**Read the `engine` block before trusting the ranking.** cass can return a
perfectly successful-looking answer while having quietly fallen back to lexical —
observed live: `"searched":"lexical", "semantic_refinement":false,
"degraded":"семантика отключилась: semantic_backfilling"`. The `engine` block
reports what was actually searched, whether semantic refinement really happened,
and whether the cass index is stale or rebuilding.

If cass is missing, slow, or broken, `find` falls back to FTS5 and says so in
`engine.fallback` — it never passes a lexical answer off as a semantic one.

Two limits worth knowing. A cass query costs 20–22 seconds on this archive
(roughly half that with `cass daemon` running), which is why `--search-timeout`
defaults to 30. And `claudex search <target>` — the per-conversation search — is
still FTS5 only: cass has no session filter, and inside one conversation lexical
is both adequate and instant.

### A closed conversation delays the report; it does not lose it

Codex conversations close and reopen under the **same** thread id — measured: one
thread changed state four times in a day. So a report that could not be delivered
is not dead, it is early.

Undelivered reports are retried on **the next claudex call you make**. There is no
watcher and nothing to start: the retry happens inside a command you were running
anyway, is capped at a handful of reports, and never blocks your input.

```bash
claudex flush         # force it now, and see what moved
claudex undelivered   # what is still waiting, with cause and full text
```

`claudex delegate` flushes first, which is the useful moment: the conversation
creating a task is alive by definition, so anything queued for it goes out then.

Three rules the retry obeys:

- **only the recorded address.** A neighbouring live conversation is never
  substituted — it never asked the question, and an answer there is noise that
  looks like a reply;
- **panes are left alone.** A pane changes conversation within the hour, so its
  binding must be re-checked at delivery time, not replayed from the journal;
- **once.** A report that landed is not sent again; the human popup is shown once
  per stage, no matter how many retries happen.

The binding itself is recorded when the task is created: `target_state` and
`target_seen` in the journal say what the address was and when that was checked.
Without them "the thread was alive" is just a claim.

**The limit worth knowing:** Codex offers no way to ask "which conversation am I".
`CODEX_THREAD_ID` is the only self-identification available, so ClauDex verifies
that it is live and records the evidence — but it cannot detect an id inherited
from a *different* conversation that also happens to be alive.

### A task that finishes without calling `done`

It happens: the work is finished, the final message is written on screen, and
`claudex done` is never called. The journal then holds `started` and `finished`
and nothing else, and the leader learns nothing — observed on task `260031f8`,
which sat silent for two hours with its result visible on the pane the whole time.

ClauDex now collects that report itself, from the **live** pane screen:

```bash
claudex reconcile      # collect what silent tasks left on screen
```

`delegate` and `undelivered` run it for you.

It is deliberately hard to satisfy. Before anything is collected, four things must
be true, and each failure is recorded with its own cause rather than guessed past:

- the executor conversation recorded at delegation is **still the one in the pane**
  (`executor_conversation_changed` otherwise) — a pane outlives the agent in it,
  so "the pane is free" proves nothing;
- the pane is `idle` or `done`, not `working` or `blocked` (`executor_busy`);
- **no later task was given to that pane** (`pane_took_later_task`) — otherwise the
  screen shows somebody else's ending;
- there is actually something on screen (`no_final_output`).

The source is the live herdr screen and only that. The cass index lags by hours,
and substituting yesterday's text for a final report is worse than substituting
nothing.

A collected report is marked `synthetic` in the journal and gets no privileges: it
goes through the same address check, the same inbox, the same deduplication as a
report the task wrote itself. If the conversation is closed, it waits in
`claudex undelivered` like any other.

**A real limit:** tasks delegated before this version have no recorded executor
binding, so they cannot be reconciled after the fact — including `260031f8`
itself. `claudex reconcile` will say so instead of inventing a report.
