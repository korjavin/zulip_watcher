# zulip_watcher

[![CI](https://github.com/korjavin/zulip_watcher/actions/workflows/ci.yml/badge.svg)](https://github.com/korjavin/zulip_watcher/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/korjavin/zulip_watcher.svg)](https://pkg.go.dev/github.com/korjavin/zulip_watcher)
[![Go Report Card](https://goreportcard.com/badge/github.com/korjavin/zulip_watcher)](https://goreportcard.com/report/github.com/korjavin/zulip_watcher)
[![License: MIT](https://img.shields.io/github/license/korjavin/zulip_watcher)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/korjavin/zulip_watcher)](https://github.com/korjavin/zulip_watcher/releases/latest)

**Let your AI agent wait for a human reply in [Zulip](https://zulip.com) without polling.**
`zulip_watcher` blocks on Zulip's real-time events, prints the next message in a
channel (optionally a topic) to stdout, and exits. Made for LLM agents such as
Claude Code and Codex that need a human-in-the-loop answer, and for plain shell scripts.

```sh
export ZULIP_URL=https://chat.example.com ZULIP_EMAIL=bot@example.com ZULIP_API_KEY=...
msg=$(zulip_watcher -stream ops -topic deploy -timeout 1h) || echo "no answer"
```

## Why

An agent that asks a human a question in Zulip then has to wait for the reply. The usual options are bad:

- **Sleep and poll.** Every wake-up is another agent turn that re-reads the whole
  conversation context, so you pay tokens for "no new message yet", add latency
  between polls, and fill the transcript with noisy tool output.
- **A long-running loop.** The agent burns a process and a turn budget just to sit there.

`zulip_watcher` instead uses Zulip's real-time events long-poll: one process,
idle on the network, zero tokens while waiting. It wakes the agent exactly once,
with only the new message.

| | Poll loop | `zulip_watcher` |
|---|---|---|
| Tokens while waiting | A full agent turn per poll | None |
| Reply latency | Up to one poll interval | Immediate |
| Tool output | One "nothing yet" per poll | Only the reply |
| Agent wake-ups | Many | Exactly one |
| Dependencies | Your own loop and parsing | Single static binary, Go stdlib only |

## How it works

1. Checks the channel exists (accepting a name or an ID) and warns if the topic is new.
2. Registers an event queue with the Zulip API, narrowed to your channel (and topic).
   See [register an event queue](https://zulip.com/api/register-queue).
3. Long-polls [`GET /events`](https://zulip.com/api/get-events) (see the
   [real-time events API](https://zulip.com/api/real-time-events)); the server holds
   the request open until something happens.
4. Ignores messages sent by the bot itself.
5. Prints the first other message and exits (the event queue is deleted on the way out).

## Install

Download a binary for Linux, macOS or Windows (amd64 / arm64) from the
[latest release](https://github.com/korjavin/zulip_watcher/releases/latest), make it
executable and put it on your `PATH`. Or with Go:

```sh
go install github.com/korjavin/zulip_watcher@latest
# or from a checkout: go build .
```

Stdlib only, single static binary named `zulip_watcher`.

You need a Zulip bot: see [Zulip's guide to creating a bot](https://zulip.com/help/add-a-bot-or-integration)
for the bot email and API key.

## Usage

### With Claude Code

Run it in the background so the agent is only woken when the reply lands:

```
Bash(command: "zulip_watcher -stream ops -topic deploy -timeout 1h", run_in_background: true)
```

The Bash tool notifies the agent when the process exits; its output is the human's
reply. A ready-made skill, `zulip-wait`, lives in
[claude-common-skills](https://github.com/korjavin/claude-common-skills/tree/master/skills/zulip-wait).

### Generic agents and shell scripts

```sh
if msg=$(zulip_watcher -stream ops -topic deploy -timeout 1h); then
  sender=${msg%%$'\n'*}   # "<sender_email> | <stream> > <topic> | id=<id>"
  body=${msg#*$'\n'}      # raw Markdown the human typed
else
  echo "no answer (exit $?)"
fi
```

## Configuration

Credentials come from the environment only (never flags, so the key stays out of `ps`):

| Variable        | Meaning                                                      |
|-----------------|--------------------------------------------------------------|
| `ZULIP_URL`     | Server URL, e.g. `https://chat.example.com` (trailing `/` ok) |
| `ZULIP_EMAIL`   | Bot email, e.g. `bot@example.com`                            |
| `ZULIP_API_KEY` | Bot API key                                                  |

Flags:

| Flag             | Meaning                                                  |
|------------------|----------------------------------------------------------|
| `-stream NAME`   | Channel name or numeric ID to watch (required)           |
| `-topic NAME`    | Only messages in this topic (optional)                   |
| `-timeout DUR`   | Give up after this long, Go duration (`30m`, `1h`); `0` = forever (default) |
| `-json`          | Print one JSON object instead of text                    |

"New" means sent after the tool started. Messages from the bot itself are ignored.

Before waiting, the channel is checked: an unknown name or ID exits `1` right away
instead of waiting forever. A numeric `-stream` that is not a channel name is
resolved as a channel ID. A `-topic` that doesn't exist yet only prints a warning
to stderr, since you may start watching before posting to a new topic.

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
| 1    | Unknown channel, or Zulip API / network / auth error           |
| 2    | Usage error: missing `-stream`, missing env var, bad flag      |
| 3    | `-timeout` elapsed with no matching message (stdout empty)     |

## FAQ and limitations

- **Private channels?** Public channels work without a subscription; private
  channels require the bot to be subscribed.
- **Is a reply sent between two runs delivered?** No. "New" means sent after the
  tool started, so a message that arrives in the gap between one run exiting and
  the next starting is not seen. Start the watcher before (or right after) asking
  your question.
- **What if the event queue expires?** If the server drops it (about 10 minutes
  idle, server restart), the tool registers a fresh one and keeps waiting;
  **messages sent during that re-register gap are lost**.
- **Only one message?** Yes, by design: it exits after the first message from someone
  else. Run it again to wait for the next.

## License

[MIT](LICENSE)
