# claudex: assignments in progress

### New work runs headless by default

When Codex is asked to delegate a new, independent task to Claude, start it
with `claudex --headless "<task>"`. This uses Claude Code's native `--bg`
session, not a Herdr pane and not a separate visible panel. The current working
directory is the default project; pass a project directory when the task
belongs elsewhere. Keep the ordinary user/project permission configuration—do
not add permission-bypass flags. The task is journaled before launch, the
supervisor is ensured running, and the worker is instructed to finish with
`claudex done`; if it forgets, the existing Stop-hook/supervisor path reports
that it ended silently rather than inventing an outcome. The caller receives a
small event and reads `claudex task log <id>` for the source report.

After launching headless work from Codex Desktop, open one Codex terminal tab
running `claude attach <short-session-id>` from the launch result. This gives
the user a clickable live view of the native background session; it is not
another Herdr pane and sends nothing to the conversation. Use the app's
terminal session and show it in Codex. `claude logs <short-session-id>` prints
recent output without attaching. Do not open duplicate attach tabs for the
same task; if the user asks for quiet/no terminal, leave the worker headless
and return only the normal task event.

Use `claudex send <conversation-id> "…"` only when the user explicitly wants
to message or continue that particular existing Claude conversation. A pane
name is not a reliable identity. Headless is for new delegated work, not a
reason to fork every substep into another session.

### Sending to someone already working continues their work

If the worker has exactly one open assignment **that you started**, your
message **continues it** rather than opening a second one. It used to open a
second one every time, and one worker accumulated 62 open assignments, none of
them ever closed.

A continuation reports back to whoever started the original assignment, so
someone else's open assignment is never continued: your message becomes a new
tracked assignment that returns to you, and `send` says why. Abandoned
assignments (no movement for 72 h) are not continued either. If claudex cannot
tell who you are (no Codex thread, Claude Code conversation or herdr pane in
the environment), it refuses instead of guessing.

```bash
claudex send license "also check the stage config"   # continues the open work
claudex send license --new "unrelated: audit deps"   # deliberately separate
```

Several open assignments of yours is a refusal listing them: which one you
meant is something only you know.

`send` is tracked, asynchronous, and returns to its caller by default. The
caller is whoever ran it — a Codex thread, a Claude Code conversation, or a
herdr pane — resolved from the environment in that order. There is no
per-caller flag: returning the result is a property of the assignment, not a
Codex feature.

Waiting is explicit (`--wait`) because it costs a turn, and you rarely need it:
the report arrives as a message either way.

An assignment moves through named states, and `claudex task <id>` reports the
one it is in:

| state | meaning |
|---|---|
| `created` | text kept, the executor refused the send |
| `sent` | delivered to the executor, no report yet |
| `needs_input` | the task says it is blocked on a question |
| `done` / `failed` | the task reported, and the report reached its caller |
| `undelivered` | the report exists but never reached the caller — `claudex flush` |
| `lost` | the executor finished and said nothing — `claudex reconcile` |
| `working` / `progress` | written by the supervisor; not inferred without one |

`undelivered` is the state worth knowing about: to the caller it is
indistinguishable from silence, which is why it is named rather than folded
into `done`.

### Older names, and what not to reach for

`delegate` is `send`, `tasks` is `task list`, `digest` is `task digest`. They
keep working; new call sites should use the new names.

`tell` posts raw text into a conversation and tracks nothing. If you expect a
result, or you are continuing work you handed over, that is `send` — a `tell`
leaves no assignment, so nothing can report on it and nothing can be recovered
when it goes missing.

`flush`, `reconcile` and `undelivered` are manual repair. The supervisor does
the flushing part on its own, and `claudex doctor` says whether it is running.

### When the worker forgets to report

`claudex done` is how a task states its own outcome — done, failed, partial,
blocked. That is a judgement, and only whoever did the work can make it.

It is also something a model has to remember, and measurement says it remembers
six times out of ten: of 424 assignments, 168 ended in silence.

So completion no longer depends on remembering. Claude Code fires a `Stop` hook
at the end of every turn and hands it `last_assistant_message` — its own schema
says this "avoids the need to read and parse the transcript file". `claudex hook
stop` records that; the supervisor waits out the silence and wakes the caller
with what it observed:

```
claudex: поручение 43bc6e11 — закончило молча (исполнитель wE:p13)
закончил молча; последняя реплика: ветка запушена, тесты флакают
```

Note what it does not say: not "done", not "failed". We saw that the work
ended; what it ended in, we do not know. Calling it done would be inventing the
one thing the report exists to carry. A task that calls `done` gives a
judgement; one that does not still gives facts.

Install the hook with `claudex hook install`.

### A closed Codex thread still receives

Closing the Codex app, or losing Remote Control from a phone, does not strand
the report. The app-server queue outlives the thread and is drained when it next
wakes — which is what you see when you open the app, type anything, and
everything that piled up arrives at once.

claudex used to refuse that case, on the reasoning that "a closed thread accepts
the queue silently and never reads it". That was an assumption written down as
fact, and it cost a hundred reports. A thread that `$CODEX_HOME` knows is a
valid address whether or not anyone is holding it; only an address that cannot
be confirmed at all is refused.

### The supervisor

A report can be written and never arrive: the caller's conversation closed
while the work was in flight. The text survives in the pull channel, but it
only moves when someone runs claudex for an unrelated reason — so a report can
wait for days, and to the caller that is indistinguishable from silence. On the
live journal there were 132 of them.

```bash
claudex supervisor                  # run it; background it yourself
claudex supervisor status           # is it running, what has it done
claudex supervisor stop
claudex supervisor --once           # single pass, for cron
nohup claudex supervisor >>~/.claudex/supervisor.log 2>&1 &
```

What arrives in your conversation is an **event, not a report**:

```
claudex: поручение 43bc6e11 — готово (исполнитель wE:p13)
миграции применены на dev и stage

Это событие, а не отчёт. Подробности читай сам:
  claudex task 43bc6e11
  claudex task log 43bc6e11
```

That is deliberate. A full report pasted into the conversation is the tool
talking where a colleague should be, and it hands the coordinator a ready
retelling to repeat in its own words. An event says what changed and where the
source is; the coordinator reads `claudex task log` and answers from the
original.

Only a caller that can run commands gets an event — a Codex thread or a Claude
Code conversation. A herdr pane notification is read by a person, and telling a
person to go read it themselves just moves the tool's work onto them, so panes
still receive the whole report. `--full-report` forces the old behaviour
anywhere.

`claudex task <id>` shows which of the two applies, and `claudex doctor` shows
it for your own session.

It is a plain process, not a language model, and **it never writes a report of
its own**. Retelling work you did not do is invention delivered in a confident
voice. It notices a state change and wakes the caller; the caller then reads
`claudex task <id>` and `claudex task log <id>` itself. That is the whole
contract, and it is what keeps a supervisor from becoming a bot that talks in
your conversation instead of the coordinator.

It stays quiet by design: at most 3 reports per pass, a bounded budget per
pass, and an address that just refused is deferred — doubling each time up to
half an hour — instead of being retried every tick. Delivery is deduplicated
through the journal, so a report that landed is never sent twice.

Collecting reports for silent workers (`reconcile`) is behind `--reconcile` and
off by default: it reads live screens and touches far more assignments than
flushing does, so it should run under supervision before it runs unattended.
