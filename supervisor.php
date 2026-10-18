<?php

declare(strict_types=1);

/**
 * Keeps N copies of the service running and restarts the ones that exit.
 *
 * Started by manager.php in its own minimized console window. Closing that
 * window ends every instance with it (Windows kills all processes attached to
 * a closed console); Ctrl+C in it stops the instances cleanly first.
 */

require __DIR__ . '/common.php';

const TICK_US = 500_000;
const STOP_TIMEOUT_S = 30;
const BACKOFF_MAX_S = 60;
const HEALTHY_AFTER_S = 60;
const PRUNE_EVERY_S = 60;
const FAILING_EXIT_S = 60;
const SUPERVISOR_LOG_ROTATE_BYTES = 5 * 1024 * 1024;

procdeck_ensure_dirs();

// Close-on-exec on POSIX: an instance must not inherit the lock and keep it held
// after the supervisor dies. Windows releases a lock with the process that took it.
$lock = fopen(PROCDECK_LOCK_FILE, procdeck_is_windows() ? 'c' : 'ce');
if ($lock === false || !flock($lock, LOCK_EX | LOCK_NB)) {
    fwrite(STDERR, t('sup_already_running') . "\n");
    exit(1);
}

$config = procdeck_config();
$desired = (int) (procdeck_read_json(PROCDECK_DESIRED_FILE)['count'] ?? 0);
if (procdeck_is_windows()) {
    @cli_set_process_title(t('supervisor_window', ['env' => procdeck_environment_tag()]));
}
$logUsage = ['bytes' => 0, 'over' => false];
$prunedAt = 0.0;
$startedAt = microtime(true);
$lastCrash = null;
$lastError = '';
$failingSince = null;
$logMaxBytes = $config['log_max_total_mb'] * 1024 * 1024;
$lastErrorAt = 0.0;

/**
 * @var array<int, array{proc: resource|null, pid: int, started: float, restarts: int,
 *     last_exit: int|null, next_start: float, backoff: int, stopping_since: float|null,
 *     restart_after_stop: bool, log: string|null}> $instances
 */
$instances = [];

$running = true;
if (function_exists('sapi_windows_set_ctrl_handler')) {
    sapi_windows_set_ctrl_handler(static function (int $event) use (&$running): void {
        say(t('sup_ctrl_event', ['event' => $event]));
        $running = false;
    });
} elseif (function_exists('pcntl_async_signals')) {
    pcntl_async_signals(true);
    pcntl_signal(SIGINT, static function () use (&$running): void {
        $running = false;
    });
    pcntl_signal(SIGTERM, static function () use (&$running): void {
        $running = false;
    });
}

function say(string $message): void
{
    $line = date('Y-m-d H:i:s') . ' ' . $message . "\n";
    echo $line;
    file_put_contents(PROCDECK_LOG_DIR . '/supervisor.log', $line, FILE_APPEND);
}

/**
 * Starts instance $id with PROCDECK_INSTANCE=<id> and the [instance_env] of
 * procdeck.ini, its {n}, {host} and {environment} filled in, and output to a new
 * log file for this run.
 *
 * @param array{script: string, workdir: string, environment: string, instance_env: array<string, string>} $config
 * @return array{resource, string}|null the process and its log file name
 */
function spawn(int $id, array $config): ?array
{
    $log = date('Ymd-His') . "-instance-{$id}.log";
    $out = fopen(PROCDECK_LOG_DIR . '/' . $log, 'a');
    if ($out === false) {
        return null;
    }
    $vars = ['{n}' => (string) $id, '{host}' => strtolower((string) gethostname()), '{environment}' => $config['environment']];
    $env = ['PROCDECK_INSTANCE' => (string) $id];
    foreach ($config['instance_env'] as $name => $template) {
        $env[$name] = strtr($template, $vars);
    }
    $proc = @proc_open(
        [PHP_BINARY, $config['script']],
        [0 => ['file', procdeck_is_windows() ? 'NUL' : '/dev/null', 'r'], 1 => $out, 2 => $out],
        $pipes,
        $config['workdir'],
        $env + getenv(),
        // Its own process group, so CTRL_BREAK reaches this instance only.
        ['create_process_group' => true],
    );
    fclose($out);

    return is_resource($proc) ? [$proc, $log] : null;
}

/**
 * Command lines of the given PIDs that are still alive.
 *
 * @param list<int> $pids
 * @return array<int, string>
 */
function command_lines(array $pids): array
{
    $out = [];
    if (procdeck_is_windows()) {
        $filter = implode(' OR ', array_map(static fn (int $p): string => "ProcessId={$p}", $pids));
        exec('powershell -NoProfile -NonInteractive -Command "Get-CimInstance Win32_Process -Filter \'' . $filter
            . '\' | ForEach-Object { [string]$_.ProcessId + \' \' + $_.CommandLine }"', $lines);
        foreach ($lines as $line) {
            [$pid, $command] = explode(' ', trim($line), 2) + [1 => ''];
            $out[(int) $pid] = $command;
        }

        return $out;
    }
    foreach ($pids as $pid) {
        $command = @file_get_contents("/proc/{$pid}/cmdline");
        if ($command !== false && $command !== '') {
            $out[$pid] = str_replace("\0", ' ', $command);
        }
    }

    return $out;
}

/**
 * After a supervisor died, its instances keep running unwatched in its window, and
 * starting N new ones would make 2N. They are ended here, before anything starts:
 * only PIDs from the previous state.json whose command line still runs our
 * service, so a PID Windows has since given to another process is left alone.
 * Forcefully, as a new supervisor in another console cannot send them CTRL_BREAK.
 */
function stop_leftovers(string $script): void
{
    $previous = procdeck_read_json(PROCDECK_STATE_FILE);
    if ($previous === null || ($previous['stopped'] ?? true) !== false) {
        return;
    }
    $pids = [];
    foreach ($previous['instances'] ?? [] as $i) {
        if (($i['pid'] ?? null) !== null) {
            $pids[(int) $i['pid']] = (int) $i['id'];
        }
    }
    if ($pids === []) {
        return;
    }
    $normalize = static fn (string $path): string => strtolower(str_replace('\\', '/', $path));
    $ours = array_filter(
        command_lines(array_keys($pids)),
        static fn (string $command): bool => str_contains($normalize($command), $normalize($script))
    );
    foreach (array_keys($ours) as $pid) {
        if (procdeck_is_windows()) {
            exec("taskkill /F /PID {$pid} 2>NUL");
        } else {
            exec("kill -9 {$pid} 2>/dev/null");
        }
        say(t('sup_leftover_killed', ['id' => $pids[$pid], 'pid' => $pid]));
    }
}

/**
 * Deletes the oldest log files until the folder fits in $maxBytes. Files a
 * running instance writes to are never deleted: Windows would only hide the
 * name and keep the space until the instance exits. So the folder can stay
 * over the limit; 'over' tells the manager to say so.
 *
 * @param list<string> $active log file names of running instances
 * @return array{bytes: int, over: bool}
 */
function prune_logs(int $maxBytes, array $active): array
{
    $supervisorLog = PROCDECK_LOG_DIR . '/supervisor.log';
    clearstatcache();
    if (is_file($supervisorLog) && filesize($supervisorLog) > SUPERVISOR_LOG_ROTATE_BYTES) {
        // say() opens and closes it per line, so a rename is safe.
        @rename($supervisorLog, PROCDECK_LOG_DIR . '/' . date('Ymd-His') . '-supervisor.log');
    }

    $files = [];
    $total = 0;
    foreach (glob(PROCDECK_LOG_DIR . '/*.log') ?: [] as $file) {
        $size = (int) @filesize($file);
        $total += $size;
        $name = basename($file);
        if ($name !== 'supervisor.log' && !in_array($name, $active, true)) {
            $files[$file] = [(int) @filemtime($file), $size];
        }
    }
    uasort($files, static fn (array $a, array $b): int => $a[0] <=> $b[0]);
    foreach ($files as $file => [, $size]) {
        if ($total <= $maxBytes) {
            break;
        }
        if (@unlink($file)) {
            $total -= $size;
            say(t('sup_log_deleted', ['file' => basename($file)]));
        }
    }

    return ['bytes' => $total, 'over' => $total > $maxBytes];
}

/** Asks the instance to stop the way Ctrl+Break in its window would. */
function request_stop(array &$inst): void
{
    $inst['stopping_since'] = microtime(true);
    if (procdeck_is_windows()) {
        @sapi_windows_generate_ctrl_event(PHP_WINDOWS_EVENT_CTRL_BREAK, $inst['pid']);
    } else {
        proc_terminate($inst['proc'], 15);
    }
}

function new_instance(): array
{
    return [
        'proc' => null, 'pid' => 0, 'started' => 0.0, 'restarts' => 0, 'streak' => 0, 'last_exit' => null,
        'next_start' => 0.0, 'backoff' => 1, 'stopping_since' => null, 'restart_after_stop' => false,
        'log' => null,
    ];
}

function handle_commands(array &$instances, int &$desired, bool &$running, float $startedAt): void
{
    $files = glob(PROCDECK_CMD_DIR . '/*.json') ?: [];
    sort($files);
    foreach ($files as $file) {
        $cmd = procdeck_read_json($file) ?? [];
        @unlink($file);
        $action = $cmd['action'] ?? '';
        if ($action === 'shutdown') {
            // One sent to an earlier supervisor must not stop this one.
            if ((float) ($cmd['at'] ?? 0) >= $startedAt) {
                say(t('sup_shutdown_requested'));
                $running = false;
            }
        } elseif ($action === 'scale') {
            $desired = max(0, min(PROCDECK_MAX_INSTANCES, (int) ($cmd['count'] ?? 0)));
            procdeck_write_json(PROCDECK_DESIRED_FILE, ['count' => $desired]);
            say(t('sup_scaled', ['count' => $desired]));
        } elseif ($action === 'restart') {
            $target = $cmd['id'] ?? 'all';
            say($target === 'all' ? t('sup_restart_all') : t('sup_restart_one', ['id' => $target]));
            foreach ($instances as $id => &$inst) {
                if ($target !== 'all' && (int) $target !== $id) {
                    continue;
                }
                $inst['backoff'] = 1;
                $inst['next_start'] = 0.0;
                if ($inst['proc'] !== null && $inst['stopping_since'] === null) {
                    $inst['restart_after_stop'] = true;
                    request_stop($inst);
                }
            }
            unset($inst);
        }
    }
}

/**
 * @param array{bytes: int, over: bool} $logUsage
 * @param array{id: int, at: float, exit: int}|null $lastCrash
 */
function write_state(array $instances, int $desired, array $logUsage, int $logMaxBytes, float $startedAt, ?array $lastCrash, bool $stopped = false): void
{
    $out = [];
    foreach ($instances as $id => $inst) {
        $status = match (true) {
            $inst['stopping_since'] !== null => 'stopping',
            $inst['proc'] !== null => 'running',
            default => 'waiting',
        };
        $out[] = [
            'id' => $id,
            'status' => $status,
            'pid' => $inst['proc'] !== null ? $inst['pid'] : null,
            'started' => $inst['proc'] !== null ? $inst['started'] : null,
            'restarts' => $inst['restarts'],
            'crash_streak' => $inst['streak'],
            'last_exit' => $inst['last_exit'],
            'next_start' => $status === 'waiting' ? $inst['next_start'] : null,
            'stopping_since' => $inst['stopping_since'],
            'log' => $inst['log'],
        ];
    }
    $state = [
        'supervisor_pid' => getmypid(),
        'supervisor_started' => $startedAt,
        'last_crash' => $lastCrash,
        'desired' => $desired,
        'instances' => $out,
        'log_bytes' => $logUsage['bytes'],
        'log_max_bytes' => $logMaxBytes,
        'log_over' => $logUsage['over'],
        // Lets the manager tell a clean stop from a supervisor that died.
        'stopped' => $stopped,
    ];
    // Written only when it changed: in steady state nothing does for hours, and
    // on Windows every rewrite is another file for the antivirus to scan.
    static $written = null;
    if ($state !== $written && procdeck_write_json(PROCDECK_STATE_FILE, $state)) {
        $written = $state;
    }
}

say(t('sup_started', [
    'pid' => getmypid(), 'environment' => $config['environment'] !== '' ? $config['environment'] : '-',
    'script' => $config['script'], 'count' => $desired,
]));
stop_leftovers($config['script']);

while (true) {
    // One failing pass is logged and the next one tried: an exception must not end
    // the supervisor and leave the instances running unwatched.
    $now = microtime(true);
    try {
        if ($running) {
            handle_commands($instances, $desired, $running, $startedAt);
        } else {
            $desired = 0;
        }

        for ($id = 1; $id <= $desired; $id++) {
            $instances[$id] ??= new_instance();
        }

        foreach ($instances as $id => &$inst) {
            if ($inst['proc'] !== null) {
                $status = proc_get_status($inst['proc']);
                if ($status['running']) {
                    if ($id > $desired && $inst['stopping_since'] === null) {
                        request_stop($inst);
                    } elseif ($inst['stopping_since'] !== null && $now - $inst['stopping_since'] > STOP_TIMEOUT_S) {
                        say(t('sup_stop_timeout', ['id' => $id, 'seconds' => STOP_TIMEOUT_S]));
                        proc_terminate($inst['proc'], 9);
                    } elseif ($inst['streak'] > 0 && $inst['stopping_since'] === null && $now - $inst['started'] >= HEALTHY_AFTER_S) {
                        if ($inst['streak'] >= PROCDECK_CRASH_LOOP_AFTER) {
                            say(t('sup_recovered', ['id' => $id]));
                        }
                        $inst['streak'] = 0;
                    }
                    continue;
                }

                $inst['last_exit'] = $status['exitcode'];
                proc_close($inst['proc']);
                $inst['proc'] = null;
                if ($inst['stopping_since'] !== null) {
                    say(t('sup_stopped_one', ['id' => $id, 'exit' => $inst['last_exit']]));
                } else {
                    // Unrequested exit: a crash. A long healthy run resets the backoff and the streak.
                    $healthyRun = $now - $inst['started'] >= HEALTHY_AFTER_S;
                    $inst['backoff'] = $healthyRun ? 1 : min(BACKOFF_MAX_S, $inst['backoff'] * 2);
                    $inst['streak'] = $healthyRun ? 1 : $inst['streak'] + 1;
                    $inst['next_start'] = $now + $inst['backoff'];
                    $inst['restarts']++;
                    $lastCrash = ['id' => $id, 'at' => $now, 'exit' => $inst['last_exit']];
                    say(t('sup_crashed', ['id' => $id, 'exit' => $inst['last_exit'], 'seconds' => $inst['backoff']]));
                    if ($inst['streak'] === PROCDECK_CRASH_LOOP_AFTER) {
                        say(t('sup_crash_loop', ['id' => $id, 'count' => $inst['streak']]));
                    }
                }
                if ($inst['restart_after_stop']) {
                    $inst['next_start'] = 0.0;
                }
                $inst['stopping_since'] = null;
                $inst['restart_after_stop'] = false;
            }

            if ($id > $desired) {
                unset($instances[$id]);
                continue;
            }
            if ($now >= $inst['next_start']) {
                $started = spawn($id, $config);
                if ($started === null) {
                    $inst['backoff'] = min(BACKOFF_MAX_S, $inst['backoff'] * 2);
                    $inst['next_start'] = $now + $inst['backoff'];
                    say(t('sup_spawn_failed', ['id' => $id, 'seconds' => $inst['backoff']]));
                    continue;
                }
                [$inst['proc'], $inst['log']] = $started;
                $inst['pid'] = proc_get_status($inst['proc'])['pid'];
                $inst['started'] = $now;
                say(t('sup_running', ['id' => $id, 'pid' => $inst['pid'], 'log' => $inst['log']]));
            }
        }
        unset($inst);

        ksort($instances);
        if ($now - $prunedAt >= PRUNE_EVERY_S) {
            $active = array_values(array_filter(array_map(
                static fn (array $i): ?string => $i['proc'] !== null ? $i['log'] : null,
                $instances
            )));
            $logUsage = prune_logs($logMaxBytes, $active);
            $prunedAt = $now;
        }
        write_state($instances, $desired, $logUsage, $logMaxBytes, $startedAt, $lastCrash);
        $failingSince = null;
    } catch (\Throwable $e) {
        unset($inst);
        $error = $e::class . ': ' . $e->getMessage();
        if ($error !== $lastError || $now - $lastErrorAt >= 60) {
            say(t('sup_error', ['error' => $error]));
            [$lastError, $lastErrorAt] = [$error, $now];
        }
        // A pass that never succeeds supervises nothing and cannot even stop. Exiting
        // without `stopped` turns it into a dead supervisor: the manager says so, and
        // the next one ends the instances left behind.
        $failingSince ??= $now;
        if ($now - $failingSince >= FAILING_EXIT_S) {
            say(t('sup_failing_exit', ['seconds' => FAILING_EXIT_S, 'error' => $error]));
            exit(1);
        }
    }

    if (!$running && $instances === []) {
        break;
    }
    usleep(TICK_US);
}

write_state($instances, $desired, $logUsage, $logMaxBytes, $startedAt, $lastCrash, true);
say(t('sup_stopped'));
