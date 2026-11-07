# qh

qh is a small coding agent for Qwen models on an OpenAI-compatible server. I wrote it for Qwen3.8-27B and Qwen3.6-35B-A3B on `mlx_lm.server`, on a MacBook. It is one Go binary and uses only the standard library.

The daemon listens on a Unix socket (`~/.qh/qh.sock`). It keeps the sessions and runs the agent loop. `qh run` and the chat client connect to it, and the first call starts it. It builds on Linux and macOS.

The socket takes one JSON line per connection. `run` streams the events of a run back; `attach` streams every event of a session (first its history) until the client leaves; `cancel` stops the active run. Runs on one session go one at a time, in order.

## Differences from pi

I used pi with these models first. Some things cost time:

- The Qwen chat template reads earlier thinking from `reasoning_content`. pi sends it back as `reasoning`, so the template drops it. In a tool loop the model loses its plan after each call and works it out again.
- Thinking sometimes loops ("wait, let me check the parser again...") until it reaches `max_tokens`.
- The system prompt and the edit tool are written for many models. The edit tool takes an array of objects, and Qwen gets the JSON wrong more often than with flat arguments.

What qh does:

- It sends the thinking back as `reasoning_content` and sets `preserve_thinking`. Each request is then the previous prompt plus new messages, which is what the server's prompt cache can reuse. (The Qwen 3.5+ models have linear-attention layers; their cache can't be trimmed, so a prompt that differs in the middle is processed again from that point.)
- When the thinking repeats itself or passes `think_budget` characters, qh stops the step and asks again with thinking off.
- There are four tools: read, write, edit and bash. edit takes one `old`/`new` pair. It still matches when the model copies the line numbers from read or drops trailing spaces. A read-only run (`-ro`) gets read, grep and ls, and no shell.
- If the server leaves a `<tool_call>` block in the text, qh parses it.
- The third identical tool call gets a note that the result will not change.
- Tool output longer than 12000 characters is cut in the middle. The end is kept, since errors are usually there.

## Install

```sh
go install github.com/ozzenerol/qh@latest
```

or clone the repo and run `go build`.

## Use

```sh
qh run "the parser tests fail since the last commit; find out why and fix it"
qh run -s lexer -C ~/src/iris -new "add a test for the new duration literals"
echo "long task text" | qh run -
qh run -ro -timeout 10m "where is the config file parsed?"
qh                # chat in this directory, session "main"
qh -c             # continue the newest session of this directory
qh -r             # pick a session to resume
qh sessions
qh reset -s lexer
qh stop
```

`qh run` prints the final report on stdout. Each tool call is one line on stderr, and the last line has the step count, tokens and time. Ctrl+C cancels the run, and so does `-timeout` (exit code 124); the daemon then kills the running command.

The chat shows every run on its session, from any client. When Claude runs `qh run -s main -from opus "..."`, the open chat prints the message with an `opus` label and streams the answer. Messages that arrive during a run wait in a queue, and the chat shows them as queued. Esc or Ctrl+C stops the run that is active, whoever started it.

In the chat, Enter sends and Ctrl+J starts a new line (pasted text keeps its lines). Up and Down walk the history, Ctrl+T shows the thinking, and Ctrl+C twice quits. Commands: `/new`, `/resume`, `/think on|off`, `/sessions`, `/clear`, `/exit`. Without a terminal (a pipe, or `TERM=dumb`) qh uses a plain line chat.

Sessions are saved in `~/.qh/sessions/NAME.json`, so they survive a daemon restart.

I use it with Claude Code as the coordinator. Claude writes the task, runs `qh run -s NAME "..."` and reads the report, then checks the diff itself. A second `qh run -s NAME` continues the same session, so a correction does not need the whole brief again.

On a small test (one failing Python test, Qwen3.6-35B-A3B on an M3 Max), qh fixed it in 9.6 s and pi in 11.7 s. That is one run each, not a benchmark.

## Config

`~/.qh/config.json` (every field is optional):

```json
{
  "url": "http://127.0.0.1:8080/v1",
  "model": "",
  "max_steps": 80,
  "max_tokens": 16384,
  "think_budget": 30000
}
```

An empty `model` means the first model the server lists. `QH_URL`, `QH_MODEL`, `QH_HOME` and `QH_SOCK` override the file.

The system prompt is about 150 words. qh adds `~/.qh/AGENTS.md`, then `AGENTS.md` (or `CLAUDE.md` when a directory has no `AGENTS.md`) from `/` down to the working directory.

## Tests

```sh
go test ./...
```

The tests use a fake streaming server, so they need no model.
