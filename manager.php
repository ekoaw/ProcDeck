<?php

declare(strict_types=1);

/**
 * The operator menu. It never owns an instance: it shows run/state.json and
 * drops commands for the supervisor, so closing this window changes nothing.
 */

require __DIR__ . '/common.php';

const GREEN = "\033[32m";
const YELLOW = "\033[33m";
const RED = "\033[31m";
const BOLD = "\033[1m";
const RESET = "\033[0m";
const REFRESH_S = 2;
// The main screen is laid out for a default 30-line console; the problem list
// takes what is left, up to MAX_PROBLEM_ROWS.
const WINDOW_LINES = 30;
const MAX_PROBLEM_ROWS = 6;
// Each grid column is filled before the next starts: columns of 5 up to
// GRID_SHORT_MAX instances, then of 10, so the 60-instance cap is 6 x 10.
const GRID_SHORT_MAX = 20;
const SAMPLE_S = 5;

procdeck_ensure_dirs();
if (function_exists('sapi_windows_vt100_support')) {
    sapi_windows_vt100_support(STDOUT, true);
    // The grid's line-drawing characters are written as UTF-8.
    sapi_windows_cp_set(65001);
}

function ask(string $prompt): string
{
    echo $prompt;
    $line = fgets(STDIN);
    if ($line === false) {
        exit(0);
    }

    return trim($line);
}

/** One menu key, or '1' (refresh) when none is pressed within REFRESH_S. */
function read_choice(): string
{
    if (procdeck_is_windows()) {
        // PHP cannot read a key without blocking on Windows; choice.exe ships with it.
        exec('choice /C 1234567890 /N /T ' . REFRESH_S . ' /D 1', $out, $code);
        if ($code >= 1 && $code <= 10) {
            echo "\n";

            return '1234567890'[$code - 1];
        }

        return ask('');
    }
    $key = read_key('1234567890', REFRESH_S) ?? '1';
    echo "\n";

    return $key;
}

/**
 * choice.exe for a Linux terminal: the first key in $keys (any key when $keys
 * is '') pressed within $seconds, or null. Other keys are ignored, as
 * choice.exe ignores them. Without a terminal (input from a pipe or a file) it
 * reads a line instead.
 */
function read_key(string $keys, int $seconds): ?string
{
    $read = [STDIN];
    $write = $except = null;
    if (!stream_isatty(STDIN)) {
        return stream_select($read, $write, $except, $seconds) > 0 ? ask('') : null;
    }
    // Key by key only while waiting: the prompts after it read whole lines.
    shell_exec('stty -icanon -echo');
    $key = null;
    $until = microtime(true) + $seconds;
    while (($left = $until - microtime(true)) > 0) {
        $read = [STDIN];
        if (stream_select($read, $write, $except, (int) $left, (int) (fmod($left, 1) * 1_000_000)) === 0) {
            break;
        }
        $pressed = strtoupper((string) fread(STDIN, 1));
        if ($pressed !== '' && ($keys === '' || str_contains($keys, $pressed))) {
            $key = $pressed;
            break;
        }
    }
    shell_exec('stty icanon echo');

    return $key;
}

function confirm(string $question): bool
{
    return strtolower(ask($question . t('confirm_suffix'))) === strtolower(t('confirm_yes'));
}

function pause(): void
{
    ask(t('press_enter'));
}

function send(array $command): void
{
    $file = PROCDECK_CMD_DIR . '/' . hrtime(true) . '-' . getmypid() . '.json';
    if (!procdeck_write_json($file, $command)) {
        echo RED . t('send_failed', ['file' => $file]) . RESET . "\n";
        pause();

        return;
    }
    // Let the supervisor apply it before the next screen is drawn.
    usleep(1_200_000);
}

/** ` [STAGING]`: red for production, yellow otherwise; '' when no environment is set. */
function environment_tag(): string
{
    $env = procdeck_config()['environment'];
    $color = in_array($env, ['production', 'prod'], true) ? RED . BOLD : YELLOW . BOLD;

    return $env === '' ? '' : $color . procdeck_environment_tag() . RESET;
}

function start_supervisor(): void
{
    $script = __DIR__ . '/supervisor.php';
    if (procdeck_is_windows()) {
        $title = t('supervisor_window', ['env' => procdeck_environment_tag()]);
        pclose(popen('start "' . $title . '" /MIN "' . PHP_BINARY . '" "' . $script . '"', 'r'));
    } else {
        exec('nohup ' . escapeshellarg(PHP_BINARY) . ' ' . escapeshellarg($script) . ' > /dev/null 2>&1 &');
    }
    for ($i = 0; $i < 20 && !procdeck_supervisor_running(); $i++) {
        usleep(250_000);
    }
}

function duration(float $seconds): string
{
    $s = (int) $seconds;
    [$d, $h, $m, $s] = [intdiv($s, 86400), intdiv($s % 86400, 3600), intdiv($s % 3600, 60), $s % 60];

    return match (true) {
        $d > 0 => sprintf('%dd %02dh %02dm %02ds', $d, $h, $m, $s),
        $h > 0 => sprintf('%dh %02dm %02ds', $h, $m, $s),
        $m > 0 => sprintf('%dm %02ds', $m, $s),
        default => "{$s}s",
    };
}

/**
 * CPU time (ms) and memory (bytes) per PID, and the server's free and total RAM
 * (bytes), in one PowerShell call on Windows.
 *
 * @param list<int> $pids
 * @return array{procs: array<int, array{int, int}>, ram: array{int, int}|null}
 */
function read_counters(array $pids): array
{
    $procs = [];
    $ram = null;
    if (procdeck_is_windows()) {
        // Integers only: a decimal would be printed with the server's locale separator.
        exec('powershell -NoProfile -NonInteractive -Command "Get-Process -Id ' . implode(',', $pids)
            . ' -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Id + \' \' + [long]$_.TotalProcessorTime.TotalMilliseconds + \' \' + $_.WorkingSet64 };'
            . ' $os = Get-CimInstance Win32_OperatingSystem; \'RAM \' + $os.FreePhysicalMemory + \' \' + $os.TotalVisibleMemorySize"', $lines);
        foreach ($lines as $line) {
            $f = explode(' ', trim($line));
            if (count($f) === 3 && $f[0] === 'RAM') {
                $ram = [(int) $f[1] * 1024, (int) $f[2] * 1024];
            } elseif (count($f) === 3) {
                $procs[(int) $f[0]] = [(int) $f[1], (int) $f[2]];
            }
        }

        return ['procs' => $procs, 'ram' => $ram];
    }
    foreach ($pids as $pid) {
        $stat = @file_get_contents("/proc/{$pid}/stat");
        $statm = @file_get_contents("/proc/{$pid}/statm");
        if ($stat !== false && $statm !== false) {
            // Fields after "(comm) ": utime and stime are the 12th and 13th, in 1/100 s.
            $f = explode(' ', substr($stat, strrpos($stat, ')') + 2));
            $procs[$pid] = [((int) $f[11] + (int) $f[12]) * 10, (int) explode(' ', $statm)[1] * 4096];
        }
    }
    if (preg_match('/MemTotal:\s+(\d+).*MemAvailable:\s+(\d+)/s', (string) @file_get_contents('/proc/meminfo'), $m)) {
        $ram = [(int) $m[2] * 1024, (int) $m[1] * 1024];
    }

    return ['procs' => $procs, 'ram' => $ram];
}

/**
 * CPU % of the whole machine (as Task Manager shows it) and memory per PID, plus
 * the server's free/total RAM, sampled at most every SAMPLE_S. CPU % needs two
 * samples, so it is null at first.
 *
 * @param list<int> $pids
 * @return array{procs: array<int, array{cpu: float|null, mem: int}>, ram: array{int, int}|null}
 */
function usage(array $pids): array
{
    static $result = ['procs' => [], 'ram' => null], $previous = [], $previousAt = 0.0;
    $now = microtime(true);
    if ($pids === [] || $now - $previousAt < SAMPLE_S) {
        return $result;
    }
    $counters = read_counters($pids);
    $cores = max(1, (int) (getenv('NUMBER_OF_PROCESSORS') ?: shell_exec('nproc')));
    $elapsedMs = ($now - $previousAt) * 1000;
    $result = ['procs' => [], 'ram' => $counters['ram']];
    foreach ($counters['procs'] as $pid => [$cpuMs, $mem]) {
        $cpu = isset($previous[$pid]) ? max(0.0, ($cpuMs - $previous[$pid]) / $elapsedMs / $cores * 100) : null;
        $result['procs'][$pid] = ['cpu' => $cpu, 'mem' => $mem];
    }
    $previous = array_map(static fn (array $c): int => $c[0], $counters['procs']);
    $previousAt = $now;

    return $result;
}

/** @return array{string, string} CPU and memory: blank without a process, '--' until it is sampled */
function usage_text(array $usage, ?int $pid): array
{
    if ($pid === null) {
        return ['', ''];
    }
    $u = $usage[$pid] ?? null;
    // No memory yet means it was sampled while still starting.
    if ($u === null || $u['mem'] === 0) {
        return ['--', ''];
    }

    return [$u['cpu'] === null ? '--' : sprintf('%.1f%%', $u['cpu']), round($u['mem'] / 1048576) . 'MB'];
}

/**
 * @param list<array<string, mixed>> $instances
 * @param array<int, array{cpu: float|null, mem: int}> $usage
 */
function print_rows(array $instances, array $usage): void
{
    $headings = [t('col_no'), t('col_status'), t('col_pid'), t('col_cpu'), t('col_memory'), t('col_uptime'), t('col_restarts'), t('col_last_exit')];
    // Each column at least as wide as its heading, which differs per language.
    $widths = array_map(static fn (int $w, string $h): int => max($w, visible_length($h)), [3, 22, 7, 6, 7, 15, 8, 0], $headings);
    $row = static fn (array $cells): string => rtrim('  ' . implode(' ', array_map(
        static fn (string $c, int $w): string => pad($c, $w),
        $cells,
        $widths
    ))) . "\n";
    echo $row($headings);
    $now = microtime(true);
    foreach ($instances as $i) {
        [$color, $label] = match (true) {
            in_crash_loop($i) => [RED, t('status_crash_loop_count', ['count' => $i['crash_streak']])],
            $i['status'] === 'running' => [GREEN, t('status_healthy')],
            $i['status'] === 'stopping' => [YELLOW, t('status_stopping')],
            default => [RED, t('status_restarting_in', ['seconds' => max(0, (int) ceil($i['next_start'] - $now))])],
        };
        [$cpu, $mem] = usage_text($usage, $i['pid']);
        echo $row([
            (string) $i['id'],
            $color . $label . RESET,
            (string) ($i['pid'] ?? '-'),
            $cpu === '' ? '-' : $cpu,
            $mem === '' ? '-' : $mem,
            $i['started'] !== null ? duration($now - $i['started']) : '-',
            (string) $i['restarts'],
            (string) ($i['last_exit'] ?? '-'),
        ]);
    }
}

/** @return list<array<string, mixed>> the instances, or none while the supervisor is down */
function current_instances(): array
{
    if (!procdeck_supervisor_running()) {
        return [];
    }

    return procdeck_read_json(PROCDECK_STATE_FILE)['instances'] ?? [];
}

/** Length as it shows on screen: colour codes take no room, a line-drawing character one. */
function visible_length(string $text): int
{
    return (int) preg_match_all('/./su', (string) preg_replace('/\033\[[0-9;]*m/', '', $text));
}

function pad(string $text, int $width): string
{
    return $text . str_repeat(' ', max(0, $width - visible_length($text)));
}

/**
 * `Key       : a  ·  b  ·  c`, with the `·` separators aligned across rows.
 *
 * @param array<string, list<string>> $rows
 * @return list<string>
 */
function status_table(array $rows): array
{
    $widths = [];
    foreach ($rows as $values) {
        foreach (array_slice($values, 0, -1) as $i => $value) {
            $widths[$i] = max($widths[$i] ?? 0, visible_length($value));
        }
    }
    $lines = [];
    foreach ($rows as $key => $values) {
        $last = array_pop($values);
        $cells = array_map(static fn (string $v, int $i): string => pad($v, $widths[$i]), $values, array_keys($values));
        $lines[] = pad((string) $key, 10) . ': ' . implode('  ·  ', [...$cells, $last]);
    }

    return $lines;
}

/** Crashed PROCDECK_CRASH_LOOP_AFTER times in a row, even while it is briefly up again. */
function in_crash_loop(array $i): bool
{
    return $i['status'] !== 'stopping' && ($i['crash_streak'] ?? 0) >= PROCDECK_CRASH_LOOP_AFTER;
}

/** One line per instance that is not healthy, for the main screen; [7] has the full table. */
function problem_line(array $i, float $now): string
{
    $id = sprintf('  #%-3d ', $i['id']);
    $wait = max(0, (int) ceil((float) $i['next_start'] - $now));
    if (in_crash_loop($i)) {
        $next = $i['status'] === 'waiting' ? t('problem_retry_in', ['seconds' => $wait]) : t('problem_retrying');

        return $id . RED . BOLD . t('status_crash_loop') . RESET . ' '
            . t('problem_crash_loop', ['count' => $i['crash_streak'], 'exit' => $i['last_exit'] ?? '-', 'next' => $next]);
    }
    if ($i['status'] === 'stopping') {
        return $id . YELLOW . t('status_stopping') . RESET . ' '
            . t('problem_stopping', ['seconds' => max(0, (int) ($now - (float) ($i['stopping_since'] ?? $now))), 'pid' => $i['pid'] ?? '-']);
    }

    return $id . RED . t('status_restarting') . RESET . ' '
        . t('problem_restarting', ['seconds' => $wait, 'count' => $i['restarts'], 'exit' => $i['last_exit'] ?? '-']);
}

/**
 * The menu as three rows, filled down each column like the grid. Column widths
 * fit every label [3] and [8] can switch to, so the menu does not shift.
 *
 * @return list<string>
 */
function menu_lines(bool $supervisor): array
{
    // Each entry: the label shown, then the other labels it can switch to.
    $columns = [
        [
            '1' => [t('menu_refresh')],
            '2' => [t('menu_scale')],
            '3' => [start_stop_label(), t('menu_stop_all'), t('menu_start_count', ['count' => PROCDECK_MAX_INSTANCES]), t('menu_start')],
        ],
        ['4' => [t('menu_restart')], '5' => [t('menu_log')], '6' => [t('menu_log_folder')]],
        [
            '7' => [t('menu_show_all')],
            '8' => [t($supervisor ? 'menu_supervisor_stop' : 'menu_supervisor_start'), t('menu_supervisor_stop'), t('menu_supervisor_start')],
            '0' => [t('menu_exit')],
        ],
    ];
    $lines = ['  ', '  ', '  '];
    foreach ($columns as $column) {
        $width = max(array_map('visible_length', array_merge(...array_values($column))));
        $r = 0;
        foreach ($column as $key => $labels) {
            $lines[$r++] .= pad("[{$key}] {$labels[0]}", $width + 6);
        }
    }

    return array_map('rtrim', $lines);
}

/**
 * The count that should run and, after a stop to 0, the count before it. Read
 * from desired.json, the supervisor's own record, so it holds while it is down.
 *
 * @return array{int, int}
 */
function desired(): array
{
    $desired = procdeck_read_json(PROCDECK_DESIRED_FILE) ?? [];

    return [(int) ($desired['count'] ?? 0), (int) ($desired['previous'] ?? 0)];
}

/** [3]: stop all while instances should run, else start the count from before the stop. */
function start_stop_label(): string
{
    [$count, $previous] = desired();

    return match (true) {
        $count > 0 => t('menu_stop_all'),
        $previous > 0 => t('menu_start_count', ['count' => $previous]),
        default => t('menu_start'),
    };
}

function start_stop(): void
{
    [$count, $previous] = desired();
    if ($count > 0) {
        if (confirm(t('confirm_stop_all'))) {
            send(['action' => 'scale', 'count' => 0]);
        }
    } elseif ($previous === 0) {
        scale();
    } elseif (confirm(t('confirm_start_count', ['count' => $previous]))) {
        send(['action' => 'scale', 'count' => $previous]);
        if (!procdeck_supervisor_running()) {
            start_supervisor();
        }
    }
}

/**
 * The main screen, laid out for a default 30-line console at up to the 60-instance
 * cap: status lines, the instance grid in a box, then the instances that are not
 * healthy in whatever room is left. [7] shows every row in full.
 */
function draw(): void
{
    $supervisor = procdeck_supervisor_running();
    $state = procdeck_read_json(PROCDECK_STATE_FILE) ?? [];
    $desired = (int) ($state['desired'] ?? 0);
    $instances = current_instances();
    // A crash-looping instance counts as restarting even while it is briefly up,
    // so the screen does not flicker between healthy and restarting.
    $looping = array_values(array_filter($instances, 'in_crash_loop'));
    $counts = ['running' => 0, 'waiting' => 0, 'stopping' => 0];
    foreach ($instances as $i) {
        $counts[in_crash_loop($i) ? 'waiting' : $i['status']]++;
    }
    $problems = [...$looping, ...array_values(array_filter(
        $instances,
        static fn (array $i): bool => $i['status'] !== 'running' && !in_crash_loop($i)
    ))];
    $usage = usage(array_values(array_filter(array_column($instances, 'pid'))));
    $procs = $usage['procs'];
    $now = microtime(true);

    // Numbered down each column, then across, like the menu below.
    $rows = $instances === [] ? 0 : min(count($instances), count($instances) <= GRID_SHORT_MAX ? 5 : 10);
    $columns = $rows === 0 ? 0 : (int) ceil(count($instances) / $rows);
    $menu = menu_lines($supervisor);
    $right = max(max(array_map('visible_length', $menu)), $columns * 19 + 1);

    $title = BOLD . t('manager_heading') . RESET . environment_tag();
    $lines = [pad($title, $right - 19) . date('Y-m-d H:i:s')];
    $logOver = $supervisor && ($state['log_over'] ?? false);

    if (!$supervisor) {
        $lines[] = t('supervisor_label') . ': ' . RED . t('supervisor_down') . RESET;
        $lines[] = '';
        if (($state['stopped'] ?? true) === false) {
            $lines[] = RED . BOLD . t('supervisor_died') . RESET;
        } else {
            $lines[] = RED . t('supervisor_stopped') . RESET;
        }
    } else {
        $gb = static fn (int $bytes): string => sprintf('%.1f', $bytes / 1073741824);
        // MB below 1 GB, so a few instances do not all read 0.1 GB.
        $memory = static fn (int $bytes): string => $bytes < 1073741824 ? round($bytes / 1048576) . ' MB' : $gb($bytes) . ' GB';
        // Instances not sampled yet (just started) are left out until they are.
        $cpu = array_filter(array_column($procs, 'cpu'), static fn (?float $c): bool => $c !== null);
        $restarts = t('info_restarts', ['count' => array_sum(array_column($instances, 'restarts'))]);
        $last = $state['last_crash'] ?? null;
        if (is_array($last)) {
            $restarts .= ' ' . t('info_last_crash', ['id' => $last['id'], 'time' => date('H:i', (int) $last['at']), 'exit' => $last['exit']]);
        }
        $info = [$restarts];
        if ($usage['ram'] !== null) {
            $info[] = t('info_ram', ['free' => $gb($usage['ram'][0]), 'total' => round($usage['ram'][1] / 1073741824)]);
        }
        if (isset($state['log_bytes'], $state['log_max_bytes'])) {
            $info[] = t('info_log') . ' ' . ($logOver ? YELLOW : '') . round($state['log_bytes'] / 1048576) . '/' . round($state['log_max_bytes'] / 1048576) . ' MB' . RESET;
        }

        $lines[] = t('supervisor_label') . ': ' . GREEN . t('supervisor_running') . RESET . ' '
            . t('elapsed', ['duration' => duration($now - (float) ($state['supervisor_started'] ?? $now))]);
        array_push($lines, ...status_table([
            t('row_instances') => [
                t('count_healthy', ['count' => $counts['running']]),
                t('count_restarting', ['count' => $counts['waiting']]),
                t('count_stopping', ['count' => $counts['stopping'], 'target' => $desired]),
            ],
            t('row_total') => [t('total_cpu') . ' ' . ($cpu === [] ? '--' : sprintf('%.1f%%', array_sum($cpu))), t('total_memory') . ' ' . $memory(array_sum(array_column($procs, 'mem')))],
            t('row_info') => $info,
        ]));
        if ($logOver) {
            $lines[] = YELLOW . t('log_over') . RESET;
        }
        if ($looping !== [] && count($looping) === count($instances)) {
            $lines[] = RED . BOLD . t('all_crash_loop') . RESET;
        } elseif ($looping !== []) {
            $lines[] = RED . BOLD . t('some_crash_loop', ['count' => count($looping)]) . RESET;
        }

        if ($instances === []) {
            // Stopped on purpose ([3], or [2] → 0) reads differently from never set up.
            [$count, $previous] = desired();
            $lines[] = '';
            $lines[] = $count === 0 && $previous > 0
                ? YELLOW . t('all_stopped', ['count' => $previous]) . RESET
                : t('no_instances');
        } else {
            $segment = str_repeat('─', 18);
            $lines[] = '┌' . implode('┬', array_fill(0, $columns, $segment)) . '┐';
            for ($r = 0; $r < $rows; $r++) {
                $cells = [];
                for ($c = 0; $c < $columns; $c++) {
                    $i = $instances[$c * $rows + $r] ?? null;
                    if ($i === null) {
                        $cells[] = str_repeat(' ', 18);
                        continue;
                    }
                    [$color, $letter] = match (true) {
                        in_crash_loop($i) => [RED, 'C'],
                        $i['status'] === 'running' => [GREEN, 'H'],
                        $i['status'] === 'stopping' => [YELLOW, 'S'],
                        default => [RED, 'R'],
                    };
                    [$cpuText, $mem] = usage_text($procs, $i['pid']);
                    $cells[] = sprintf(' %2d %s%s%s %5s %5s ', $i['id'], $color . BOLD, $letter, RESET, $cpuText, $mem);
                }
                $lines[] = '│' . implode('│', $cells) . '│';
            }
            $lines[] = '└' . implode('┴', array_fill(0, $columns, $segment)) . '┘';
            $lines[] = ' ' . GREEN . 'H' . RESET . ' = ' . t('legend_healthy') . '   ' . RED . 'R' . RESET . ' = ' . t('legend_restarting') . '   '
                . YELLOW . 'S' . RESET . ' = ' . t('legend_stopping') . '   ' . RED . 'C' . RESET . ' = ' . t('legend_crash_loop');

            if ($problems === []) {
                $lines[] = '';
                $lines[] = GREEN . t('all_healthy') . RESET . ' ' . t('all_healthy_hint');
            } else {
                // What the window has left after the heading, the blank line, the menu and the prompt.
                $room = WINDOW_LINES - (count($lines) + 1 + 5);
                $show = count($problems) <= min($room, MAX_PROBLEM_ROWS)
                    ? count($problems)
                    : max(1, min(MAX_PROBLEM_ROWS, $room - 1));
                $lines[] = YELLOW . t('needs_attention') . RESET;
                foreach (array_slice($problems, 0, $show) as $i) {
                    $lines[] = problem_line($i, $now);
                }
                if (count($problems) > $show) {
                    $lines[] = '  ' . t('more_problems', ['count' => count($problems) - $show]);
                }
            }
        }
    }

    $lines[] = '';
    array_push($lines, ...$menu);
    // Redrawn in place (home, clear each line's tail, clear below) so a refresh does not flicker.
    echo "\033[H" . implode("\033[K\n", $lines) . "\033[K\n\033[J";
}

function show_all(): void
{
    echo "\033[2J\033[H" . BOLD . t('all_heading') . RESET . '   ' . date('Y-m-d H:i:s') . "\n";
    $instances = current_instances();
    if ($instances === []) {
        echo t('none_running') . "\n";
    } else {
        print_rows($instances, usage(array_values(array_filter(array_column($instances, 'pid'))))['procs']);
    }
    echo "\n";
    pause();
}

function scale(): void
{
    $answer = ask(t('ask_count', ['max' => PROCDECK_MAX_INSTANCES]));
    if ($answer === '') {
        return;
    }
    if (!ctype_digit($answer) || (int) $answer > PROCDECK_MAX_INSTANCES) {
        echo RED . t('bad_count', ['max' => PROCDECK_MAX_INSTANCES]) . RESET . "\n";
        pause();

        return;
    }
    $count = (int) $answer;
    if (confirm(t('confirm_scale', ['count' => $count]))) {
        send(['action' => 'scale', 'count' => $count]);
        // The supervisor applies the command before starting any instance.
        if ($count > 0 && !procdeck_supervisor_running()) {
            start_supervisor();
        }
    }
}

/** The taskbar and Alt+Tab then tell a staging manager from a production one. */
function set_title(string $suffix = ''): void
{
    if (procdeck_is_windows()) {
        @cli_set_process_title(t('manager_window', ['env' => procdeck_environment_tag()]) . $suffix);
    }
}

/**
 * True when a key is pressed within a second; the live log views poll on this.
 * On Windows any letter or digit (choice.exe takes no other keys), on Linux any key.
 */
function back_pressed(): bool
{
    if (procdeck_is_windows()) {
        $keys = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ';
        $started = microtime(true);
        exec('choice /C ' . $keys . ' /N /T 1 /D Z', $out, $code);
        if ($code >= 1 && $code <= strlen($keys)) {
            // Z is also what a timeout returns; a Z that comes back early was pressed.
            return $code !== strlen($keys) || microtime(true) - $started < 0.9;
        }
        ask('');

        return true;
    }

    return read_key('', 1) !== null;
}

function instance_log(string $id): ?string
{
    foreach (current_instances() as $i) {
        if ((string) $i['id'] === $id && isset($i['log'])) {
            return PROCDECK_LOG_DIR . '/' . $i['log'];
        }
    }

    return null;
}

/**
 * What was added to $file since $offset, at most the last $max bytes, the new
 * offset, and whether older bytes were skipped. A file that shrank (rotated) is
 * read from its start.
 *
 * @return array{string, int, bool}
 */
function read_new(string $file, int $offset, int $max): array
{
    clearstatcache(true, $file);
    $size = @filesize($file);
    if ($size === false || $size === $offset) {
        return ['', $offset, false];
    }
    if ($size < $offset) {
        $offset = 0;
    }
    $skipped = $size - $offset > $max;
    if ($skipped) {
        $offset = $size - $max;
    }
    $fh = @fopen($file, 'r');
    if ($fh === false) {
        return ['', $offset, $skipped];
    }
    fseek($fh, $offset);
    $text = (string) stream_get_contents($fh);
    $offset = (int) ftell($fh);
    fclose($fh);

    return [$text, $offset, $skipped];
}

/** Prints what was added to $file since $offset, at most the last 16 KB, and returns the new offset. */
function print_new(string $file, int $offset): int
{
    [$text, $offset, $skipped] = read_new($file, $offset, 16384);
    if ($skipped) {
        echo YELLOW . t('log_skipped') . RESET . "\n";
    }
    echo $text;

    return $offset;
}

/**
 * New output of every running instance and of supervisor.log, line by line with
 * a coloured [N] prefix, until 0 is pressed. Lines are shown in the order they
 * are read, once a second; the exact time is whatever the service prints itself.
 * Nothing older than the moment the view opens: without timestamps of our own,
 * old lines from several files cannot be put in order.
 */
function show_all_logs(): void
{
    // Colours as docker compose uses them; red is left out, it means trouble here.
    $colours = ["\033[36m", "\033[33m", "\033[32m", "\033[35m", "\033[34m", "\033[96m", "\033[93m", "\033[92m", "\033[95m", "\033[94m"];
    set_title(' - ' . t('log_all_window'));
    echo "\033[2J\033[H" . BOLD . t('log_all_heading') . RESET . "\n---\n";
    /** @var array<string, array{file: string, offset: int, partial: string}> $sources */
    $sources = [];
    $first = true;
    do {
        $files = ['supervisor' => PROCDECK_LOG_DIR . '/supervisor.log'];
        foreach (current_instances() as $i) {
            if (isset($i['log'])) {
                $files[(string) $i['id']] = PROCDECK_LOG_DIR . '/' . $i['log'];
            }
        }
        // Shared by all sources, so 60 busy instances cannot flood the console.
        $max = max(1024, intdiv(16384, count($files)));
        foreach ($files as $key => $file) {
            $prefix = $key === 'supervisor'
                ? BOLD . '[' . t('log_prefix_supervisor') . ']' . RESET . ' '
                : $colours[((int) $key - 1) % count($colours)] . sprintf('[%2d]', $key) . RESET . ' ';
            $source = $sources[$key] ?? null;
            if ($source === null || $source['file'] !== $file) {
                // Open: from the end. Started or restarted since: the new file from its start.
                $source = ['file' => $file, 'offset' => $first ? (int) @filesize($file) : 0, 'partial' => ''];
            }
            [$text, $source['offset'], $skipped] = read_new($file, $source['offset'], $max);
            if ($skipped) {
                echo $prefix . YELLOW . t('log_skipped') . RESET . "\n";
                $source['partial'] = '';
            }
            // A line is shown once it is complete, so a prefix never lands mid-line,
            // or after a second without more output, so a last line without "\n" shows too.
            $lines = explode("\n", $source['partial'] . $text);
            $source['partial'] = $text === '' ? '' : (string) array_pop($lines);
            $lines = array_filter($lines, static fn (string $line): bool => $line !== '' || $text !== '');
            foreach ($lines as $line) {
                echo $prefix . rtrim($line, "\r") . "\n";
            }
            $sources[$key] = $source;
        }
        $sources = array_intersect_key($sources, $files);
        $first = false;
    } while (!back_pressed());
    set_title();
}

/** The last 40 lines of an instance's log, then new output as it is written, until 0 is pressed. */
function show_log(): void
{
    $id = ask(t('ask_log_id'));
    if ($id === '') {
        return;
    }
    if (strtolower($id) === 'a') {
        show_all_logs();

        return;
    }
    $file = instance_log($id);
    if ($file === null || !is_file($file)) {
        echo RED . t('log_not_found', ['id' => $id]) . RESET . "\n";
        pause();

        return;
    }
    set_title(' - ' . t('log_window', ['id' => $id]));
    // A screen of its own, so the menu above it does not look still active.
    echo "\033[2J\033[H" . BOLD . t('log_heading', ['id' => $id]) . RESET . "\n{$file}\n---\n";
    $fh = fopen($file, 'r');
    fseek($fh, max(0, filesize($file) - 16384));
    $lines = explode("\n", rtrim((string) stream_get_contents($fh)));
    $offset = (int) ftell($fh);
    fclose($fh);
    echo implode("\n", array_slice($lines, -40)) . "\n";
    while (!back_pressed()) {
        $offset = print_new($file, $offset);
        $current = instance_log($id);
        if ($current !== null && $current !== $file) {
            // The instance restarted into a new file.
            $file = $current;
            echo YELLOW . '--- ' . t('log_new_file', ['id' => $id, 'file' => basename($file)]) . ' ---' . RESET . "\n";
            $offset = print_new($file, 0);
        }
    }
    set_title();
}

function on_path(string $program): bool
{
    exec(procdeck_is_windows()
        ? 'where /Q ' . escapeshellarg($program)
        : 'command -v ' . escapeshellarg($program) . ' > /dev/null 2>&1', $out, $code);

    return $code === 0;
}

$config = procdeck_config();
set_title();
$ini = procdeck_ini();
$vars = ['ini' => realpath(PROCDECK_INI) ?: PROCDECK_INI, 'script' => $config['script'], 'workdir' => $config['workdir'], 'program' => $config['command'][0] ?? ''];
$hasCommand = isset($ini['command']);
$error = match (true) {
    $hasCommand && (!is_array($ini['command']) || trim((string) reset($ini['command'])) === '') => t('config_command_lines', $vars),
    $hasCommand && trim((string) ($ini['script'] ?? '')) !== '' => t('config_both', $vars),
    $hasCommand && trim((string) ($ini['workdir'] ?? '')) === '' => t('config_workdir_required', $vars),
    !$hasCommand && !is_file($config['script']) => t('script_not_found', $vars),
    !is_dir($config['workdir']) => t('workdir_not_found', $vars),
    $hasCommand && preg_match('~[\\\\/]~', $vars['program']) && !is_file($vars['program']) => t('command_not_found', $vars),
    // A bare name (go, cmd.exe) is looked up on the PATH, as starting it will.
    $hasCommand && !preg_match('~[\\\\/]~', $vars['program']) && !on_path($vars['program']) => t('command_not_found', $vars),
    default => null,
};
if ($error !== null) {
    echo RED . $error . RESET . "\n";
    pause();
    exit(1);
}
if (!procdeck_supervisor_running()) {
    start_supervisor();
}

while (true) {
    draw();
    echo t('prompt_choice', ['seconds' => REFRESH_S]);
    $choice = read_choice();
    switch ($choice) {
        case '':
        case '1':
            break;
        case '2':
            scale();
            break;
        case '3':
            start_stop();
            break;
        case '4':
            $id = ask(t('ask_restart_id'));
            if (strtolower($id) === 'a') {
                if (confirm(t('confirm_restart_all'))) {
                    send(['action' => 'restart', 'id' => 'all']);
                }
            } elseif (ctype_digit($id) && confirm(t('confirm_restart_one', ['id' => $id]))) {
                send(['action' => 'restart', 'id' => (int) $id]);
            }
            break;
        case '5':
            show_log();
            break;
        case '6':
            if (procdeck_is_windows()) {
                pclose(popen('explorer "' . realpath(PROCDECK_LOG_DIR) . '"', 'r'));
            } else {
                echo t('log_folder', ['path' => realpath(PROCDECK_LOG_DIR)]) . "\n";
                pause();
            }
            break;
        case '7':
            show_all();
            break;
        case '8':
            if (!procdeck_supervisor_running()) {
                start_supervisor();
            } elseif (confirm(t('confirm_shutdown'))) {
                send(['action' => 'shutdown', 'at' => microtime(true)]);
            }
            break;
        case '0':
            // A supervisor that died leaves its instances running unwatched.
            echo match (true) {
                procdeck_supervisor_running() => t('exit_message', ['window' => t('supervisor_window', ['env' => procdeck_environment_tag()])]),
                (procdeck_read_json(PROCDECK_STATE_FILE)['stopped'] ?? true) === false => t('exit_message_died'),
                default => t('exit_message_down'),
            } . "\n";
            exit(0);
        default:
            echo RED . t('unknown_choice') . RESET . "\n";
            pause();
    }
}
