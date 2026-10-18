# ProcDeck

Runs N copies of one PHP script (`php.exe service.php`) on a Windows server,
restarts the ones that exit, and changes how many run, without opening a cmd
window per copy. No service wrapper and no .exe: plain PHP scripts and a .bat
an operator double-clicks. It also runs on Linux, which is how it is tested
without a Windows machine: `procdeck.sh` is the Linux counterpart of
`procdeck.bat`.

Needs PHP 8.2 or later.

## Setup

1. Copy this folder into the project, for example to `tools\procdeck\`.
2. Edit `procdeck.ini`: at least `php` and `script` (see [Settings](#settings-procdeckini)).
3. Double-click `procdeck.bat` (Linux: run `./procdeck.sh`).

## Use

1. Double-click `procdeck.bat`.
2. Press `[2] Set number of instances`, type the number, then `y`.
3. Done. The screen refreshes every 2 seconds. In the main menu press a number
   key, no Enter needed (on Linux, press Enter after the key). When asked for a number (instance count, instance
   number), leave it empty and press Enter to cancel.

The main screen shows each instance in a box, numbered down then across
(columns of 5 up to 20 instances, then 10; at most 60 instances), as one letter:

| Letter | Status | Meaning |
| --- | --- | --- |
| **H** (green) | HEALTHY | Running normally. |
| **R** (red) | RESTARTING | Exited on its own, will be started again automatically. |
| **S** (yellow) | STOPPING | Being stopped (restart, or the instance count was lowered). |
| **C** (red) | CRASH LOOP | Exited 3 times in a row without running 1 minute. Still retried automatically, but something is almost certainly wrong: see its log (`[6]`). |

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
  is in the `Restarts` column of `[9]`.
- `!!` (yellow): the log folder is over its limit.

Under the box, `Needs attention` lists the instances that are not H, one line
each, as many as fit in the window (at most 6). `[9]` shows the full table (PID,
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

Folder `run\logs\` (menu `[7]`):

- `<date>-<time>-instance-<N>.log`, for example `20261017-091502-instance-7.log`:
  the output of instance N, as it used to show in a cmd window. Every start makes
  a new file. Menu `[6]` shows the last 40 lines of the instance's newest file,
  then new lines as they are written (also after the instance restarts, from its
  new file). Press `0` to go back.
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
| `php` | `php.exe` | `php.exe` for the menu and every instance: its full path, or `php.exe` when it is on the PATH. Read by `procdeck.bat` and `procdeck.sh`; `procdeck.sh` uses `php` from the PATH instead when it ends in `.exe`. |
| `script` | (required) | The script to run, relative to this folder or a full path. |
| `workdir` | the script's folder | Working directory of the instances, relative to this folder or a full path. |
| `name` | `ProcDeck` | Shown in the window titles and the heading. |
| `environment` | empty | Label shown as `[STAGING]`; `production`/`prod` in red, anything else yellow. Empty = no label. |
| `lang` | `en` | Language of the screens and `supervisor.log`: a file in `lang\`. Ships with `en` and `id`. |
| `log_max_total_mb` | `20` | Size the log folder is kept under, in MB. |

Section `[instance_env]`: extra environment variables for each instance.
`{n}` is the instance number, `{host}` the server's name in lowercase,
`{environment}` the label above. Every instance also gets `PROCDECK_INSTANCE=<n>`.
They win over variables of the same name the supervisor itself has.

```ini
[instance_env]
METRICS_INSTANCE_LABEL = {host}-{environment}-{n}
```

Section `[text]`: overrides single texts of the language file, by key (see
`lang/en.php` for the keys and their `{placeholders}`):

```ini
[text]
all_crash_loop = "!! Every instance is in a crash loop: check .env and the broker. See the log: [6]"
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
  shell, so the PID is the real `php.exe`), in its own process group, with
  output appended to one log file per run. Stop, restart and scale-down send
  CTRL_BREAK to the instance's group, and kill it after 30 seconds. Only one
  supervisor runs, held by a lock on `run\supervisor.lock` that the OS releases
  when the process dies.
- `manager.php` is the menu. It never owns an instance: it reads
  `run\state.json` and drops command files into `run\cmd\`. It also samples
  CPU and memory, in one PowerShell call every 5 seconds.

The supervisor is the only writer of `state.json` and `desired.json`
(write-then-rename); the menu is the only writer of command files. No file has
two writers. `state.json` records whether the supervisor stopped cleanly; a new
supervisor that finds it did not first kills the previous instances, but only
PIDs whose command line still contains the script, so a reused PID is left alone.

Windows constraints behind the design:

- Instance output goes to files, never pipes: a non-blocking pipe read
  busy-waits up to ~32 s on Windows.
- A running instance's log cannot be rotated or truncated, so the log cap is
  approximate.
- PHP cannot read a key without blocking on a Windows console, so the menu
  reads keys with `choice.exe`. Esc and Enter cannot be menu keys.
- Box characters must exist in CP437: only the sharp corners `┌┐└┘┬┴│─`.
- Closing a console window kills every process attached to it, and killing
  only the supervisor leaves its instances running; hence the leftover cleanup.

## License

MIT, see [LICENSE](LICENSE).
