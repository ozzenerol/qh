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
- There are four tools: read, write, edit and bash. edit takes one `old`/`new` pair. It still matches when the model copies the line numbers from read or drops trailing spaces. When `old` is not in the file, the error shows the closest lines with their numbers, so the model can copy them without reading the file again. When the model leaves out `path` and only one file holds `old`, qh edits that file and says so. When nothing matches exactly, lines that look nearly the same are used to find the place. write says how many lines it added and removed compared with the old file. When `old` appears more than once, the error lists the lines, and `line=N` picks the match nearest to line N. A read of a missing file suggests project files with the same name. A grep or rg that finds nothing is not counted as a failed call. When grep or rg rejects a pattern (an unescaped `(` and the like), the result says to use `-F` for a literal search. A read-only run (`-ro`) gets read, grep and ls, and no shell.
- If the server leaves a `<tool_call>` block in the text, qh parses it.
- The third identical tool call gets a note that the result will not change.
- When the task has a "Done when" line, qh asks the model once, before the final report, to check each requirement and fix what is missing. In a normal (not read-only) run it then runs the commands in backticks on that line, such as `./mvnw -q test`. If one fails, the model gets the output and one more turn, and the commands run again after it; the report ends with one `qh check passed:` or `qh check FAILED:` line per command. When the task asks for no warnings or no unused code, compiler warnings in the check output count as a failure for that turn (see `warn_ignore` below). qh also reads the Kotlin and Java files that the run wrote or edited, and lists each import whose name is not used again in the file (kotlinc does not warn about these); the model gets one turn to remove them.
- The model server answers one request at a time, so the daemon sends one at a time. A run on another session waits and shows "waiting for the model".
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
qh                # chat in this directory, in a new session
qh -c             # continue the newest session of this directory
qh -r             # pick a session to resume
qh compact -s lexer "keep the test names"
qh wait -s lexer  # block until the run on lexer ends, then print its report
qh sessions
qh reset -s lexer
qh stop
```

`qh wait -s NAME` waits for the run that is going on a session (started from anywhere) and prints its report and stats line the same way, with exit code 4 at the step limit. `qh run` prints the final report on stdout. The first line on stderr names the session, each tool call is one line, and the last line has the steps, failed tool calls, tokens, time and context size (with a hint to compact past 60% of the window). Ctrl+C cancels the run, and so does `-timeout` (exit code 124); the daemon then kills the running command. A run that reaches `max_steps` gets a warning 5 steps before, then a request for the report (a tool call there gets "tools are off" and up to two more tries), and `qh run` exits with 4. When a command fails and left JUnit XML reports (Gradle `test-results`, Maven `surefire-reports`), the tool result ends with the failing tests, their messages and the top of each stack. Test commands (gradle, mvn, `test`, `check`) get this even with exit 0, since `./gradlew test | tail` exits 0. A failed build also gets every compiler error as one `file:line:col message` line (Kotlin `e:`, javac and gcc `error:`, Maven `[ERROR]`), so they are not lost in a cut tail. After three commands in a row that dig in library jars or caches (`unzip`, `javap`, `~/.gradle/caches`, `~/.m2`) with no edit between them, the model is told to stop and gets the last compiler errors again (or, before any build failed, to write the code and compile it). After the second time, such commands are blocked for the rest of the run. Calls to `find` or `glob`, which are not in the tool list but which the model sometimes makes, list matching files.

The chat shows every run on its session, from any client. The session name `@chat` means the chat that is open in a terminal (the newest one, if there are several). When Claude runs `qh run -s @chat -from opus "..."`, the open chat prints the message with an `opus` label and streams the answer. Messages that arrive during a run wait in a queue, and the chat shows them as queued. Esc or Ctrl+C stops the run that is active, whoever started it.

In the chat, Enter sends and Ctrl+J starts a new line (pasted text keeps its lines). Up and Down walk the history, Ctrl+T shows the thinking, and Ctrl+C twice quits. Type `/` for the command menu (`/new`, `/resume`, `/think on|off`, `/sessions`, `/clear`, `/exit`); ↑↓ picks one, Tab completes, Enter runs it. Without a terminal (a pipe, or `TERM=dumb`) qh uses a plain line chat.

Sessions are saved in `~/.qh/sessions/NAME.json` after each step, so they survive a daemon restart. `qh sessions` lists them with the number of turns, the context size, and `RUNNING step N` for a run in progress (`WAITING for the model` while another session's run has the model), and who started it (`from opus`, `from qh run`, `from a terminal chat`). A chat started with plain `qh` gets a new session named after the time (`1108-013105`); `qh run` uses `main` unless you give `-s`.

## Compaction

When a request passes 80% of `context` (262144 tokens by default), qh asks the model for a summary of the conversation and replaces the messages with it. This happens before a new prompt, or between two steps of a run; in the second case the model gets "continue the task from the summary". `/compact [focus]` in the chat, or `qh compact -s NAME [focus]`, does it by hand. The summary request is the normal prompt plus one question, so the server reads the whole conversation from its cache and only writes the summary. The old messages go to `~/.qh/sessions/archive/`. In the chat, Ctrl+O shows or hides the last summary above the input box.

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
  "think_budget": 30000,
  "context": 262144,
  "compact_at": 0,
  "warn_ignore": [],
  "sort_imports": false,
  "format": {".go": "gofmt -w"},
  "repos": {"myrepo": {"sort_imports": true, "format": {".kt": "ktlint -F"}}}
}
```

An empty `model` means the first model the server lists. `compact_at` 0 means 80% of `context`. `warn_ignore` holds regular expressions for compiler warning lines that the "Done when" checks should not count. Only compiler warnings count (Kotlin `w:`, `file:line: warning:`, Maven compiler `[WARNING] File.java:[l,c]`); JVM notices such as `WARNING: sun.misc.Unsafe` do not.

After the model's final answer, qh can tidy the files the run wrote or edited, before the checks run. `sort_imports` sorts the imports of Kotlin files (IntelliJ layout: other packages, then `java`, `javax`, `kotlin`, then aliases) and Java files (static imports last). `format` maps a file extension to a command, and qh appends the changed files of that type. `repos` sets both for one repo, by the folder name of its git root. The report lists what qh changed. `QH_URL`, `QH_MODEL`, `QH_HOME` and `QH_SOCK` override the file.

The system prompt is about 150 words. qh adds `~/.qh/AGENTS.md`, then `~/.qh/notes/REPO.md` (REPO is the folder name of the git repo that holds the working directory; a good place for API facts and repo notes that should not be committed), then `AGENTS.md` (or `CLAUDE.md` when a directory has no `AGENTS.md`) from `/` down to the working directory.

## Tests

```sh
go test ./...
```

The tests use a fake streaming server, so they need no model.
