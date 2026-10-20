# ProcDeck (PHP)

Runs N copies of one program on a Windows server, restarts the ones that
exit, and changes how many run, without opening a cmd window per copy. The
program is any command: `php.exe service.php`, a compiled Go service,
`node server.js`, a `.bat` or `.sh` script. No service
wrapper and no .exe of its own: plain PHP scripts and a .bat an operator
double-clicks. It also runs on Linux, which is how it is tested without a
Windows machine: `procdeck.sh` is the Linux counterpart of `procdeck.bat`.

Needs PHP 8.2 or later for ProcDeck itself, whatever the instances run.

## Setup

1. Copy this folder into the project, for example to `tools\procdeck\`.
2. Edit `procdeck.ini`: at least `php`, `command[]` and `workdir` (see [Settings](#settings-procdeckini)).
3. Double-click `procdeck.bat` (Linux: run `./procdeck.sh`).

## Use

1. Double-click `procdeck.bat`.
2. Press `[2] Set number of instances`, type the number, then `y`.
3. Done. The screen refreshes every 2 seconds. In the main menu press a number
   key, no Enter needed. When asked for a number (instance count, instance
   number), leave it empty and press Enter to cancel.

The menu:

| Key | Action |
| --- | --- |
| `[1]` | Refresh now. |
| `[2]` | Set the number of instances (0 stops them all). |
| `[3]` | **Stop all instances**; after that it reads **Start N instances** and starts the same number again. |
| `[4]` | Restart one instance by number, or all with `a`. |
| `[5]` | Live log of one instance by number, or of all with `a`. |
| `[6]` | Open the log folder. |
| `[7]` | Table of all instances. |
| `[8]` | Shut down / start the supervisor. |
| `[0]` | Exit the menu; the instances keep running. |

The main screen shows each instance in a box, numbered down then across
(columns of 5 up to 20 instances, then 10; at most 60 instances), as one letter:

| Letter | Status | Meaning |
| --- | --- | --- |
| **H** (green) | HEALTHY | Running normally. |
| **R** (red) | RESTARTING | Exited on its own, will be started again automatically. |
| **S** (yellow) | STOPPING | Being stopped (restart, or the instance count was lowered). |
| **C** (red) | CRASH LOOP | Exited 3 times in a row without running 1 minute. Still retried automatically, but something is almost certainly wrong: see its log (`[5]`). |

When an instance is in a crash loop, a red line `!! N instance(s) in a crash
loop` appears. When **every** instance is in a crash loop, the cause is almost
always outside them: the configuration or a dependency. An instance turns `H`
again on its own after running 1 minute without exiting.

Next to the letter: the instance's CPU and memory, refreshed every 5 seconds.
CPU is counted the way Task Manager does, as a percentage of the whole server:
on an 8-core server one instance using one full core shows 12.5%. `--` means no
CPU figure yet (it needs two samples, 5 seconds apart). If the box lines show as
odd characters, the window is not using UTF-8.

The lines above the box:

- `Supervisor: RUNNING for …`: how long the supervisor has been running
  (`1d 02h 13m 12s`).
- `Instances`: how many are healthy, **currently** waiting to restart, and stopping.
- `Total`: CPU and memory of all instances.
- `Info`: how many times an instance exited on its own since the supervisor
  started (restarts from the menu do not count) and the last one (instance
  number, time, exit code), the server's free RAM, and the log folder size. The
  restart count goes back to 0 when the supervisor is restarted; per instance it
  is in the `Restarts` column of `[7]`.
- `!!` (yellow): the log folder is over its limit.

Under the box, `Needs attention` lists the instances that are not H, one line
each, as many as fit in the window (at most 6). `[7]` shows the full table (PID,
CPU, memory, uptime, restarts, last exit).

### The two windows

When opened, the menu starts a small minimized window titled
**ProcDeck Supervisor - DO NOT CLOSE** (the `name` from `procdeck.ini`). That
window runs every instance:

| Action | Result |
| --- | --- |
| Closing the **ProcDeck Manager** window / `[0]` | Safe, the instances keep running. |
| Menu `[8] Shut down supervisor` | Every instance is stopped cleanly, then the supervisor exits. The instance count is remembered; `[8] Start supervisor` starts it again. **The recommended way to stop.** |
| Closing the **ProcDeck Supervisor** window | **Every instance dies at once** (not cleanly). Open `procdeck.bat` again to start them. |
| The supervisor process dies on its own (rare: ended from Task Manager → Details, or errors without pause) | The instances keep running unwatched until a supervisor runs again. The menu shows `!! The supervisor died unexpectedly`. Press `[8]` (or open `procdeck.bat` again): the new supervisor first stops those old instances, then starts the usual count. |
| RDP disconnect | Safe. |
| Log off / server restart | Every instance dies. Open `procdeck.bat` again; the last instance count is remembered. |

An instance that exits on its own is started again automatically. If it keeps
exiting, the wait before the next start grows (2, 4, 8, … up to 60 seconds);
the **Restarts** and **Last exit** columns show it, and the reason is in its log.

Ctrl+C in the supervisor window is not a supported way to stop: it sometimes
restarts the instances instead. Use `[8]`.

### Staging and production on one server

Use a separate copy of this folder for each; every copy has its own supervisor,
logs and settings. Set `environment` in each `procdeck.ini`: it shows as
`[STAGING]` in the heading and the window titles, `production` (or `prod`) in red.

## Logs

Folder `run\logs\` (menu `[6]`):

- `<date>-<time>-instance-<N>.log`, for example `20261017-091502-instance-7.log`:
  the output of instance N, as it used to show in a cmd window. Every start makes
  a new file. Menu `[5]` shows the last 40 lines of the instance's newest file,
  then new lines as they are written (also after the instance restarts, from its
  new file). Press any letter or digit to go back (on Linux, any key).

  `[5]` → `a` shows all instances at once, like `docker compose logs -f`: new
  lines from every running instance and from `supervisor.log`, each with a
  coloured `[N]` (or `[supervisor]`) prefix. It starts empty and shows what is
  written from then on, read once a second, so lines from different instances
  within the same second may be out of order. For the exact time, use the
  timestamp the service prints on each line; ProcDeck adds none.
- `supervisor.log`: when instances start, exit and are restarted. Above 5 MB it
  is renamed to `<date>-<time>-supervisor.log`.

The folder's total size is kept under `log_max_total_mb`: every minute, while it
is over, the oldest files are deleted. A file a running instance writes to
cannot be deleted; when those files alone are over the limit, the menu shows a
yellow warning. Restarting an instance starts a new file, and the old one can
then be deleted.

## Settings (`procdeck.ini`)

| Key | Default | Meaning |
| --- | --- | --- |
| `php` | `php.exe` | `php.exe` that runs ProcDeck itself (menu and supervisor): its full path, or `php.exe` when it is on the PATH. Read by `procdeck.bat` and `procdeck.sh`; `procdeck.sh` uses `php` from the PATH instead when it ends in `.exe`. Instances run whatever `command[]` says, which can be another PHP. |
| `command[]` | (required) | The program each instance runs, one `command[] =` line per argument; see [The command](#the-command). |
| `workdir` | (required) | The folder the instances run in, relative to this folder or a full path. |
| `name` | `ProcDeck` | Shown in the window titles and the heading. |
| `environment` | empty | Label shown as `[STAGING]`; `production`/`prod` in red, anything else yellow. Empty = no label. |
| `lang` | `en` | Language of the screens and `supervisor.log`: a file in `lang\`. Ships with `en` and `id`. |
| `log_max_total_mb` | `20` | Size the log folder is kept under, in MB. |

### The command

```ini
; a PHP script
command[] = C:\php\php.exe
command[] = service.php
workdir = ..\..

; a compiled program with arguments
command[] = bin\service.exe
command[] = --port=8080
workdir = ..\..
```

- One argument per line, passed as written: no shell, no quoting. A relative
  path, the program's included, is relative to `workdir`. A bare name
  (`go`, `cmd.exe`, `node`) is looked up on the PATH.
- The program must stop cleanly on **Ctrl+Break** on Windows (Go:
  `os.Interrupt`; Node: `SIGBREAK`; Python: `signal.SIGBREAK`) and on
  **SIGTERM** on Linux. One that does not is killed after 30 seconds, with
  every process it started.
- Each instance runs in its own process group (`setsid` on Linux), so a stop
  or a kill reaches the program and whatever it started.
- A script goes through its interpreter: `command[] = cmd.exe`,
  `command[] = /c`, `command[] = run.bat` on Windows; `command[] = /bin/sh`,
  `command[] = run.sh` on Linux. The same goes for `go run`. These work, with
  two limits: CPU and memory on the screen are the wrapper's (`cmd.exe`,
  `sh`, `go`), not the program's; and on Windows a stop can take until the
  30-second kill when `cmd.exe` holds on to a batch file. For a server, point
  `command[]` at the program itself (`service.exe`, `node.exe server.js`).
- A missing or wrong `command[]` or `workdir` stops the menu at start with the
  line to fix; a `procdeck.ini` that still has the old `script =` says how to
  rewrite it.

Section `[instance_env]`: extra environment variables for each instance.
`{n}` is the instance number, `{host}` the server's name in lowercase,
`{environment}` the label above. Every instance also gets `PROCDECK_INSTANCE=<n>`.
They win over variables of the same name the supervisor itself has.

```ini
[instance_env]
INSTANCE_NAME = {host}-{environment}-{n}
```

Section `[text]`: overrides single texts of the language file, by key (see
`lang/en.php` for the keys and their `{placeholders}`):

```ini
[text]
all_crash_loop = "!! Every instance is in a crash loop: check .env and the database. See the log: [5]"
```

Values are read as written (`INI_SCANNER_RAW`); quote a value that has a `;`.

## Translating

Copy `lang/en.php` to `lang/<code>.php`, translate the values, keep every
`{placeholder}`, and set `lang = <code>`. A key left out falls back to
`lang/en.php`, so a translation can be partial. Keep the menu labels short
enough that the menu fits in 120 columns.

`procdeck.bat` and `procdeck.sh` only print English: it runs before PHP is known to work.

## How it works

Two processes that talk only through files in `run\`:

- `supervisor.php` owns the instances. It starts each with `proc_open` (no
  shell, so the PID is the real program), in its own process group, with
  output appended to one log file per run. Stop, restart and scale-down send
  CTRL_BREAK (Windows) or SIGTERM (Linux) to the instance's group, and kill
  the whole tree after 30 seconds (`taskkill /T`, or the process group). On
  Linux an instance lasts until its whole group has exited, so a wrapper that
  dies first (`sh`, `go run`) does not let a second copy start while its
  program is still stopping. Only one
  supervisor runs, held by a lock on `run\supervisor.lock` that the OS releases
  when the process dies.
- `manager.php` is the menu. It never owns an instance: it reads
  `run\state.json` and drops command files into `run\cmd\`. It also samples
  CPU and memory, in one PowerShell call every 5 seconds.

The supervisor is the only writer of `state.json` and `desired.json`
(write-then-rename); the menu is the only writer of command files. No file has
two writers. `state.json` records whether the supervisor stopped cleanly; a new
supervisor that finds it did not first kills the previous instances, but only
PIDs whose command line still contains every argument of `command[]`, so a
reused PID is left alone.

Windows constraints behind the design:

- Instance output goes to files, never pipes: a non-blocking pipe read
  busy-waits up to ~32 s on Windows.
- A running instance's log cannot be rotated or truncated, so the log cap is
  approximate.
- PHP cannot read a key without blocking on a Windows console, so the menu
  reads keys with `choice.exe`. Esc and Enter cannot be menu keys. On Linux
  the menu does the same with `stty` (`read_key()` in `manager.php`).
- Box characters must exist in CP437: only the sharp corners `┌┐└┘┬┴│─`.
- Closing a console window kills every process attached to it, and killing
  only the supervisor leaves its instances running; hence the leftover cleanup.

## License

MIT, see [LICENSE](../LICENSE).
