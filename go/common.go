// ProcDeck runs N copies of one program, restarts the ones that exit, and
// changes how many run. One executable holds both halves: without arguments it
// is the operator menu, with "supervisor" it is the process that owns the
// instances. They talk only through files in run/: the supervisor is the single
// writer of state.json and desired.json, the menu only drops command files into
// run/cmd/, so no file ever has two writers.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// The main screen's grid is laid out for at most 6 columns of 10.
	maxInstances = 60
	// Crashes in a row, each before running 60 s, that make an instance a crash loop.
	crashLoopAfter = 3
)

// Paths, all relative to the folder the executable is in.
var (
	exePath     string
	baseDir     string
	iniPath     string
	runDir      string
	cmdDir      string
	logDir      string
	stateFile   string
	desiredFile string
	lockPath    string
)

type config struct {
	command     []string
	workdir     string
	name        string
	environment string
	logMaxBytes int64
	instanceEnv map[string]string
}

// ini is procdeck.ini as written: section ("" for the top) -> key -> values.
// A `key[] =` line appends to key; any other line replaces it.
var (
	ini   map[string]map[string][]string
	cfg   config
	texts map[string]string
)

var absolutePath = regexp.MustCompile(`^([a-zA-Z]:)?[\\/]`)

func setPaths(exe string) {
	exePath = exe
	baseDir = filepath.Dir(exe)
	iniPath = filepath.Join(baseDir, "procdeck.ini")
	runDir = filepath.Join(baseDir, "run")
	cmdDir = filepath.Join(runDir, "cmd")
	logDir = filepath.Join(runDir, "logs")
	stateFile = filepath.Join(runDir, "state.json")
	desiredFile = filepath.Join(runDir, "desired.json")
	lockPath = filepath.Join(runDir, "supervisor.lock")
}

// parseINI reads values as written, so {n} templates, Windows paths and "!!" in
// texts stay as they are. A value in double quotes may contain a `;`; outside
// quotes a `;` starts a comment.
func parseINI(text string) map[string]map[string][]string {
	out := map[string]map[string][]string{"": {}}
	section := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if out[section] == nil {
				out[section] = map[string][]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if rest, quoted := strings.CutPrefix(value, `"`); quoted {
			value, _, _ = strings.Cut(rest, `"`)
		} else {
			value, _, _ = strings.Cut(value, ";")
			value = strings.TrimSpace(value)
		}
		if name, list := strings.CutSuffix(key, "[]"); list {
			out[section][name] = append(out[section][name], value)
		} else {
			out[section][key] = []string{value}
		}
	}
	return out
}

// iniValue is a top-level key of procdeck.ini, or "" when it is not set.
func iniValue(key string) string {
	if values := ini[""][key]; len(values) > 0 {
		return strings.TrimSpace(values[len(values)-1])
	}
	return ""
}

// resolvePath makes a path from procdeck.ini absolute, relative to dir.
func resolvePath(dir, path string) string {
	if absolutePath.MatchString(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// loadConfig reads procdeck.ini (see README.md for each key) and the texts. A
// missing file leaves every key at its default; the menu then says what to set.
func loadConfig() {
	raw, _ := os.ReadFile(iniPath)
	ini = parseINI(string(raw))

	workdir := baseDir
	if w := iniValue("workdir"); w != "" {
		workdir = resolvePath(baseDir, w)
	}
	command := append([]string(nil), ini[""]["command"]...)
	if len(command) > 0 && strings.ContainsAny(command[0], `\/`) {
		// Relative to workdir, like every relative path the program itself is given.
		command[0] = resolvePath(workdir, command[0])
	}
	logMaxMB, err := strconv.Atoi(iniValue("log_max_total_mb"))
	if err != nil {
		logMaxMB = 20
	}
	cfg = config{
		command:     command,
		workdir:     workdir,
		name:        iniValue("name"),
		environment: strings.ToLower(iniValue("environment")),
		logMaxBytes: int64(max(1, logMaxMB)) * 1024 * 1024,
		instanceEnv: map[string]string{},
	}
	if cfg.name == "" {
		cfg.name = "ProcDeck"
	}
	for name, values := range ini["instance_env"] {
		cfg.instanceEnv[name] = values[len(values)-1]
	}

	texts = map[string]string{}
	for key, text := range langs["en"] {
		texts[key] = text
	}
	for key, text := range langs[iniValue("lang")] {
		texts[key] = text
	}
	for key, values := range ini["text"] {
		texts[key] = values[len(values)-1]
	}
}

// t is the text for key: from procdeck.ini's [text] section, else the `lang`
// texts, else English. {name} is always filled in; other {placeholders} come
// from pairs of name and value.
func t(key string, pairs ...any) string {
	text, ok := texts[key]
	if !ok {
		text = key
	}
	replace := []string{"{name}", cfg.name}
	for i := 0; i+1 < len(pairs); i += 2 {
		replace = append(replace, "{"+pairs[i].(string)+"}", fmt.Sprint(pairs[i+1]))
	}
	return strings.NewReplacer(replace...).Replace(text)
}

// environmentTag is ` [STAGING]`, or "" when no environment is configured.
func environmentTag() string {
	if cfg.environment == "" {
		return ""
	}
	return " [" + strings.ToUpper(cfg.environment) + "]"
}

func ensureDirs() error {
	for _, dir := range []string{runDir, cmdDir, logDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(file string, v any) bool {
	raw, err := os.ReadFile(file)
	return err == nil && json.Unmarshal(raw, v) == nil
}

// writeJSON writes then renames, so a reader never sees half a file. On
// Windows the rename can fail while a reader has the target open; callers that
// write on every tick just try again on the next one.
func writeJSON(file string, v any) error {
	raw, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o666); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// errLocked is lockFile's error when another process holds the lock.
var errLocked = errors.New("locked by another process")

// supervisorRunning is true while a supervisor holds the lock. The OS releases
// the lock when the supervisor dies, so a crashed supervisor never looks alive.
func supervisorRunning() bool {
	f, err := lockFile(lockPath)
	if err != nil {
		return err == errLocked
	}
	f.Close()
	return false
}

// unixSeconds is how times are kept in state.json.
func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func fromUnixSeconds(s float64) time.Time {
	return time.Unix(0, int64(s*1e9))
}

// state.json, written by the supervisor and read by the menu.
type state struct {
	SupervisorPID     int             `json:"supervisor_pid"`
	SupervisorStarted float64         `json:"supervisor_started"`
	LastCrash         *crash          `json:"last_crash"`
	Desired           int             `json:"desired"`
	Instances         []instanceState `json:"instances"`
	LogBytes          int64           `json:"log_bytes"`
	LogMaxBytes       int64           `json:"log_max_bytes"`
	LogOver           bool            `json:"log_over"`
	// Lets the menu tell a clean stop from a supervisor that died.
	Stopped bool `json:"stopped"`
}

type crash struct {
	ID   int     `json:"id"`
	At   float64 `json:"at"`
	Exit int     `json:"exit"`
}

type instanceState struct {
	ID            int      `json:"id"`
	Status        string   `json:"status"` // running, waiting or stopping
	PID           *int     `json:"pid"`
	Started       *float64 `json:"started"`
	Restarts      int      `json:"restarts"`
	CrashStreak   int      `json:"crash_streak"`
	LastExit      *int     `json:"last_exit"`
	NextStart     *float64 `json:"next_start"`
	StoppingSince *float64 `json:"stopping_since"`
	Log           *string  `json:"log"`
}

// desired.json: the count that should run and, after a stop to 0, the count before it.
type desiredCount struct {
	Count    int `json:"count"`
	Previous int `json:"previous"`
}

// A command file in run/cmd/. ID is an instance number or "all".
type command struct {
	Action string  `json:"action"` // scale, restart or shutdown
	Count  int     `json:"count"`
	ID     any     `json:"id,omitempty"`
	At     float64 `json:"at,omitempty"`
}

func main() {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find the program's own folder:", err)
		os.Exit(1)
	}
	setPaths(exe)
	loadConfig()
	if err := ensureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "supervisor" {
		os.Exit(runSupervisor())
	}
	runManager()
}
