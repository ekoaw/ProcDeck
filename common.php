<?php

declare(strict_types=1);

/**
 * Shared by manager.php (the operator menu) and supervisor.php (the process
 * owner). They talk only through files in run/: the supervisor is the single
 * writer of state.json and desired.json, the manager only drops command files
 * into run/cmd/, so no file ever has two writers.
 */

const PROCDECK_INI = __DIR__ . '/procdeck.ini';
const PROCDECK_RUN_DIR = __DIR__ . '/run';
const PROCDECK_CMD_DIR = PROCDECK_RUN_DIR . '/cmd';
const PROCDECK_LOG_DIR = PROCDECK_RUN_DIR . '/logs';
const PROCDECK_STATE_FILE = PROCDECK_RUN_DIR . '/state.json';
const PROCDECK_DESIRED_FILE = PROCDECK_RUN_DIR . '/desired.json';
const PROCDECK_LOCK_FILE = PROCDECK_RUN_DIR . '/supervisor.lock';
// The main screen's grid is laid out for at most 6 columns of 10.
const PROCDECK_MAX_INSTANCES = 60;
// Crashes in a row, each before running 60 s, that make an instance a crash loop.
const PROCDECK_CRASH_LOOP_AFTER = 3;

function procdeck_is_windows(): bool
{
    return PHP_OS_FAMILY === 'Windows';
}

/** @return array<string, mixed> procdeck.ini as written, sections included */
function procdeck_ini(): array
{
    static $ini = null;
    if ($ini === null) {
        // Raw, so {n} templates, Windows paths and "!!" in texts are taken as written.
        $ini = @parse_ini_file(PROCDECK_INI, true, INI_SCANNER_RAW);
        $ini = is_array($ini) ? $ini : [];
    }

    return $ini;
}

/** A path from procdeck.ini: absolute, or relative to the folder procdeck.ini is in. */
function procdeck_path(string $path): string
{
    $path = preg_match('~^([a-zA-Z]:)?[\\\\/]~', $path) ? $path : __DIR__ . '/' . $path;

    return realpath($path) ?: $path;
}

/**
 * procdeck.ini; see README.md for each key. `php` is read by procdeck.bat.
 *
 * @return array{script: string, workdir: string, name: string, environment: string,
 *     log_max_total_mb: int, instance_env: array<string, string>}
 */
function procdeck_config(): array
{
    $ini = procdeck_ini();
    $script = trim((string) ($ini['script'] ?? ''));
    $script = $script === '' ? '' : procdeck_path($script);
    $workdir = trim((string) ($ini['workdir'] ?? ''));
    $instanceEnv = $ini['instance_env'] ?? [];

    return [
        'script' => $script,
        'workdir' => $workdir !== '' ? procdeck_path($workdir) : ($script !== '' ? dirname($script) : __DIR__),
        'name' => trim((string) ($ini['name'] ?? '')) ?: 'ProcDeck',
        'environment' => strtolower(trim((string) ($ini['environment'] ?? ''))),
        'log_max_total_mb' => max(1, (int) ($ini['log_max_total_mb'] ?? 20)),
        'instance_env' => is_array($instanceEnv) ? array_map('strval', $instanceEnv) : [],
    ];
}

/**
 * The text for $key: from procdeck.ini's [text] section, else the `lang` file,
 * else lang/en.php. {name} is always filled in; other {placeholders} from $vars.
 *
 * @param array<string, string|int|float> $vars
 */
function t(string $key, array $vars = []): string
{
    static $texts = null;
    if ($texts === null) {
        $ini = procdeck_ini();
        $lang = (string) ($ini['lang'] ?? 'en');
        $texts = require __DIR__ . '/lang/en.php';
        if ($lang !== 'en' && preg_match('/^[A-Za-z0-9_-]+$/', $lang) && is_file(__DIR__ . "/lang/{$lang}.php")) {
            $texts = (require __DIR__ . "/lang/{$lang}.php") + $texts;
        }
        if (is_array($ini['text'] ?? null)) {
            $texts = array_map('strval', $ini['text']) + $texts;
        }
    }
    $replace = ['{name}' => procdeck_config()['name']];
    foreach ($vars as $var => $value) {
        $replace['{' . $var . '}'] = (string) $value;
    }

    return strtr($texts[$key] ?? $key, $replace);
}

/** `[STAGING]`, or '' when no environment is configured. */
function procdeck_environment_tag(): string
{
    $env = procdeck_config()['environment'];

    return $env === '' ? '' : ' [' . strtoupper($env) . ']';
}

function procdeck_ensure_dirs(): void
{
    foreach ([PROCDECK_RUN_DIR, PROCDECK_CMD_DIR, PROCDECK_LOG_DIR] as $dir) {
        if (!is_dir($dir)) {
            mkdir($dir, 0777, true);
        }
    }
}

/** @return array<string, mixed>|null */
function procdeck_read_json(string $file): ?array
{
    $raw = @file_get_contents($file);
    if ($raw === false) {
        return null;
    }
    $data = json_decode($raw, true);

    return is_array($data) ? $data : null;
}

/**
 * Write-then-rename, so a reader never sees half a file. On Windows the
 * rename can fail while a reader has the target open; callers that write on
 * every tick just try again on the next one.
 *
 * @param array<string, mixed> $data
 */
function procdeck_write_json(string $file, array $data): bool
{
    $tmp = $file . '.' . getmypid() . '.tmp';
    if (file_put_contents($tmp, json_encode($data, JSON_PRETTY_PRINT)) === false) {
        return false;
    }
    if (!@rename($tmp, $file)) {
        @unlink($tmp);

        return false;
    }

    return true;
}

/**
 * True while a supervisor holds the lock. The lock is released by the OS when
 * the supervisor dies, so a crashed supervisor never looks alive.
 */
function procdeck_supervisor_running(): bool
{
    $fh = @fopen(PROCDECK_LOCK_FILE, 'c');
    if ($fh === false) {
        return false;
    }
    $free = flock($fh, LOCK_EX | LOCK_NB);
    if ($free) {
        flock($fh, LOCK_UN);
    }
    fclose($fh);

    return !$free;
}
