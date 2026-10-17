# claudex: driving a session

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
