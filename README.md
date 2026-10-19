# zulip-watcher

Blocks until **one** new Zulip message arrives in a channel (optionally a topic),
prints it to stdout and exits. Built for agents waiting on a human: run it once
and sleep in the process wait instead of polling.

```sh
msg=$(zulip-watcher -stream ops -topic deploy -timeout 1h) || echo "no answer"
```

## Install

```sh
go install .        # or: go build -o zulip-watcher .
```

Stdlib only, single static binary.

## Configuration

Credentials come from the environment only (never flags, so the key stays out of `ps`):

| Variable        | Meaning                                                      |
|-----------------|--------------------------------------------------------------|
| `ZULIP_URL`     | Server URL, e.g. `https://zulip.example.com` (trailing `/` ok) |
| `ZULIP_EMAIL`   | Bot email                                                    |
| `ZULIP_API_KEY` | Bot API key                                                  |

Flags:

| Flag             | Meaning                                                  |
|------------------|----------------------------------------------------------|
| `-stream NAME`   | Channel to watch (required)                              |
| `-topic NAME`    | Only messages in this topic (optional)                   |
| `-timeout DUR`   | Give up after this long, Go duration (`30m`, `1h`); `0` = forever (default) |
| `-json`          | Print one JSON object instead of text                    |

"New" means sent after the tool started. Messages from the bot itself are ignored.

## Output

Text (default): a header line, then the raw Markdown content:

```
alice@example.com | ops > deploy | id=101
ship it
```

`-json`: one line:

```json
{"content":"ship it","id":101,"sender_email":"alice@example.com","sender_full_name":"Alice","stream":"ops","timestamp":1700000001,"topic":"deploy"}
```

Diagnostics go to stderr only.

## Exit codes

| Code | Meaning                                                        |
|------|----------------------------------------------------------------|
| 0    | A message was printed                                          |
| 1    | Zulip API / network / auth error                               |
| 2    | Usage error: missing `-stream`, missing env var, bad flag      |
| 3    | `-timeout` elapsed with no matching message (stdout empty)     |

## Caveats

- Public channels work without a subscription; **private channels require the bot to be subscribed**.
- If the server drops the event queue (≈10 min idle, server restart), the tool
  registers a fresh one and keeps waiting; **messages sent during that gap are lost**.
