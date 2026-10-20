# ProcDeck (Go)

The Go variant of ProcDeck. It has the same menu, supervisor, `procdeck.ini`
and `run/` folder as the [PHP variant](../php/README.md), and is meant mainly
for Linux VMs. It runs from source: `procdeck.sh` builds it and starts the
menu. It also builds for Windows as one `procdeck.exe`.

**Status: draft.** Tested on Linux. It has not run on Windows yet.

## Setup (Linux)

Needs Go 1.26 or later on the VM. The Linux build has no dependencies outside
the standard library, so it builds without internet.

1. Copy this folder into the project, for example to `tools/procdeck/`. Keep
   it on the VM's own disk, not on a folder shared from the host: the
   supervisor lock (`flock`) is not reliable on shared folders.
2. Edit `procdeck.ini`: at least `command[]` and `workdir`.
3. Run `./procdeck.sh`.

`procdeck.sh` builds `procdeck` next to `procdeck.ini`, then starts it. The
build is cached, so it takes about a second when nothing changed. After a
source change, the menu runs the new code at once. A supervisor that is already
running keeps the old code until it is shut down and started again with `[8]`.

`go run .` does not work: it builds in a temporary folder that it deletes on
exit, while the supervisor it started keeps running, and ProcDeck looks for
`procdeck.ini` next to the executable.

Closing the terminal or losing the SSH session ends only the menu. The
supervisor runs in a session of its own, so it and the instances keep running.
A VM reboot ends them all: run `./procdeck.sh` again, and the last instance
count is restarted.

## Windows

```sh
GOOS=windows GOARCH=amd64 go build -o procdeck.exe .
```

Copy `procdeck.exe` and `procdeck.ini` into one folder and double-click
`procdeck.exe`. The supervisor opens in its own minimized window, as in the
PHP variant.

## Use and settings

The same as the PHP variant: see [Use](../php/README.md#use),
[Logs](../php/README.md#logs) and [Settings](../php/README.md#settings-procdeckini).
The differences:

- No `php` key. The menu and the supervisor are the same executable:
  `procdeck` is the menu, and `procdeck supervisor` is the supervisor, which
  the menu starts by itself.
- The live log views (`[5]`) close on any key, on Windows too: the menu reads
  keys itself and does not need `choice.exe`.
- The texts are compiled in (`lang.go`: `en` and `id`). `[text]` in
  `procdeck.ini` still overrides single keys.
- On Windows, CPU and memory come from the Windows API, not from a PowerShell
  call every 5 seconds. PowerShell is still used once when a supervisor starts,
  to read the command lines of a dead supervisor's leftover instances.

## How it works

The same two processes and the same files in `run/` as the PHP variant (see
[How it works](../php/README.md#how-it-works)):

- `supervisor.go` owns the instances. Each one is started with `os/exec`, with
  stdout and stderr going straight to its log file, in its own process group
  (`Setsid` on Linux, `CREATE_NEW_PROCESS_GROUP` on Windows). One goroutine
  per instance waits for it to exit. Everything else runs in one loop that
  ticks every 0.5 s. Stop is SIGTERM to the group, or CTRL_BREAK on Windows.
  Kill is SIGKILL to the group, or `taskkill /T /F`.
- `manager.go` is the menu. The terminal stays in key mode (no line input, no
  echo) for as long as the menu runs, and one goroutine reads stdin. Prompts
  do their own echo and Backspace. With this, a key wait and a line prompt
  never compete for a read that is already blocked.
- `os_linux.go` and `os_windows.go` hold what differs per OS: the lock, stop
  and kill, CPU and memory counters, terminal modes, and starting the
  supervisor (a detached session on Linux, `cmd /c start "<title>" /MIN` on
  Windows).
