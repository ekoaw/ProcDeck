package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tick                     = 500 * time.Millisecond
	stopTimeout              = 30 * time.Second
	backoffMaxS              = 60
	healthyAfter             = 60 * time.Second
	pruneEvery               = 60 * time.Second
	failingExit              = 60 * time.Second
	supervisorLogRotateBytes = 5 * 1024 * 1024
)

type instance struct {
	cmd              *exec.Cmd
	done             chan struct{} // closed once the process has exited and exit is set
	exit             int
	pid              int
	started          time.Time
	restarts         int
	streak           int
	lastExit         *int
	nextStart        time.Time
	backoff          int
	stoppingSince    time.Time // zero while not stopping
	restartAfterStop bool
	log              string
}

func (inst *instance) stopping() bool {
	return !inst.stoppingSince.IsZero()
}

func say(message string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + message + "\n"
	fmt.Print(line)
	f, err := os.OpenFile(filepath.Join(logDir, "supervisor.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err == nil {
		f.WriteString(line)
		f.Close()
	}
}

// spawn starts instance id with PROCDECK_INSTANCE=<id> and the [instance_env]
// of procdeck.ini, its {n}, {host} and {environment} filled in, and output to
// a new log file for this run. Output goes to the file directly, never through
// a pipe the supervisor would have to drain.
//
// Each instance gets its own process group (see newGroup), so a stop reaches
// the instance and whatever it started.
func spawn(id int, inst *instance) error {
	log := fmt.Sprintf("%s-instance-%d.log", time.Now().Format("20060102-150405"), id)
	out, err := os.OpenFile(filepath.Join(logDir, log), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer out.Close()
	host, _ := os.Hostname()
	vars := strings.NewReplacer("{n}", strconv.Itoa(id), "{host}", strings.ToLower(host), "{environment}", cfg.environment)
	// Later entries win, so these override the supervisor's own variables.
	env := append(os.Environ(), "PROCDECK_INSTANCE="+strconv.Itoa(id))
	for name, template := range cfg.instanceEnv {
		env = append(env, name+"="+vars.Replace(template))
	}

	cmd := exec.Command(cfg.command[0], cfg.command[1:]...)
	cmd.Dir = cfg.workdir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = newGroup()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	inst.cmd, inst.done, inst.pid, inst.log = cmd, done, cmd.Process.Pid, log
	go func() {
		cmd.Wait()
		inst.exit = cmd.ProcessState.ExitCode()
		close(done)
	}()
	return nil
}

func (inst *instance) exited() bool {
	select {
	case <-inst.done:
		return true
	default:
		return false
	}
}

// stopLeftovers ends the instances of a supervisor that died. They keep running
// unwatched, and starting N new ones would make 2N. Only PIDs from the previous
// state.json whose command line still has every argument of the command are
// ended, so a PID the OS has since given to another process is left alone.
// Forcefully, as a new supervisor in another console cannot send them CTRL_BREAK.
func stopLeftovers() {
	var previous state
	if !readJSON(stateFile, &previous) || previous.Stopped {
		return
	}
	ids := map[int]int{}
	var pids []int
	for _, i := range previous.Instances {
		if i.PID != nil {
			ids[*i.PID] = i.ID
			pids = append(pids, *i.PID)
		}
	}
	if len(pids) == 0 {
		return
	}
	normalize := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, `\`, "/")) }
	for pid, line := range commandLines(pids) {
		ours := true
		for _, arg := range cfg.command {
			if !strings.Contains(normalize(line), normalize(arg)) {
				ours = false
				break
			}
		}
		if ours {
			killTree(pid)
			say(t("sup_leftover_killed", "id", ids[pid], "pid", pid))
		}
	}
}

type logUsage struct {
	bytes int64
	over  bool
}

// pruneLogs deletes the oldest log files until the folder fits in maxBytes.
// Files a running instance writes to are never deleted: Windows would refuse,
// or only hide the name and keep the space until the instance exits. So the
// folder can stay over the limit; over tells the menu to say so.
func pruneLogs(maxBytes int64, active []string) logUsage {
	supervisorLog := filepath.Join(logDir, "supervisor.log")
	if fi, err := os.Stat(supervisorLog); err == nil && fi.Size() > supervisorLogRotateBytes {
		// say() opens and closes it per line, so a rename is safe.
		os.Rename(supervisorLog, filepath.Join(logDir, time.Now().Format("20060102-150405")+"-supervisor.log"))
	}

	type logFile struct {
		path    string
		modTime time.Time
		size    int64
	}
	var files []logFile
	var total int64
	matches, _ := filepath.Glob(filepath.Join(logDir, "*.log"))
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		total += fi.Size()
		name := filepath.Base(path)
		if name != "supervisor.log" && !slices.Contains(active, name) {
			files = append(files, logFile{path, fi.ModTime(), fi.Size()})
		}
	}
	sort.Slice(files, func(a, b int) bool { return files[a].modTime.Before(files[b].modTime) })
	for _, f := range files {
		if total <= maxBytes {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			say(t("sup_log_deleted", "file", filepath.Base(f.path)))
		}
	}
	return logUsage{total, total > maxBytes}
}

type supervisor struct {
	instances map[int]*instance
	desired   int
	running   bool
	startedAt time.Time
	lastCrash *crash
	logUsage  logUsage
	prunedAt  time.Time
	lastState []byte
	signals   chan os.Signal
}

func (s *supervisor) handleCommands() {
	files, _ := filepath.Glob(filepath.Join(cmdDir, "*.json"))
	sort.Strings(files)
	for _, file := range files {
		var cmd command
		readJSON(file, &cmd)
		os.Remove(file)
		switch cmd.Action {
		case "shutdown":
			// One sent to an earlier supervisor must not stop this one.
			if cmd.At >= unixSeconds(s.startedAt) {
				say(t("sup_shutdown_requested"))
				s.running = false
			}
		case "scale":
			count := max(0, min(maxInstances, cmd.Count))
			// Kept across a stop to 0, so the menu can offer to start the same count again.
			var d desiredCount
			readJSON(desiredFile, &d)
			previous := d.Previous
			if count == 0 && s.desired > 0 {
				previous = s.desired
			}
			s.desired = count
			writeJSON(desiredFile, desiredCount{Count: s.desired, Previous: previous})
			say(t("sup_scaled", "count", s.desired))
		case "restart":
			target := 0 // all
			if id, ok := cmd.ID.(float64); ok {
				target = int(id)
				say(t("sup_restart_one", "id", target))
			} else {
				say(t("sup_restart_all"))
			}
			for id, inst := range s.instances {
				if target != 0 && target != id {
					continue
				}
				inst.backoff = 1
				inst.nextStart = time.Time{}
				if inst.cmd != nil && !inst.stopping() {
					inst.restartAfterStop = true
					inst.stoppingSince = time.Now()
					requestStop(inst.pid)
				}
			}
		}
	}
}

func (s *supervisor) writeState(stopped bool) {
	ids := slices.Sorted(maps.Keys(s.instances))
	out := make([]instanceState, 0, len(ids))
	for _, id := range ids {
		inst := s.instances[id]
		i := instanceState{ID: id, Restarts: inst.restarts, CrashStreak: inst.streak, LastExit: inst.lastExit}
		// Still running while its group outlives a wrapper that exited (Linux).
		alive := inst.cmd != nil
		switch {
		case inst.stopping():
			i.Status = "stopping"
			since := unixSeconds(inst.stoppingSince)
			i.StoppingSince = &since
		case alive:
			i.Status = "running"
		default:
			i.Status = "waiting"
			next := unixSeconds(inst.nextStart)
			i.NextStart = &next
		}
		if alive {
			pid, started := inst.pid, unixSeconds(inst.started)
			i.PID, i.Started = &pid, &started
		}
		if inst.log != "" {
			log := inst.log
			i.Log = &log
		}
		out = append(out, i)
	}
	raw, _ := json.Marshal(state{
		SupervisorPID:     os.Getpid(),
		SupervisorStarted: unixSeconds(s.startedAt),
		LastCrash:         s.lastCrash,
		Desired:           s.desired,
		Instances:         out,
		LogBytes:          s.logUsage.bytes,
		LogMaxBytes:       cfg.logMaxBytes,
		LogOver:           s.logUsage.over,
		Stopped:           stopped,
	})
	// Written only when it changed: in steady state nothing does for hours, and
	// on Windows every rewrite is another file for the antivirus to scan.
	if bytes.Equal(raw, s.lastState) {
		return
	}
	if writeJSON(stateFile, json.RawMessage(raw)) == nil {
		s.lastState = raw
	}
}

// step is one pass of the loop. A panic is returned as an error, so one failing
// pass is logged and the next one tried instead of ending the supervisor and
// leaving the instances running unwatched.
func (s *supervisor) step(now time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()

	select {
	case sig := <-s.signals:
		say(t("sup_ctrl_event", "event", sig))
		s.running = false
	default:
	}
	if s.running {
		s.handleCommands()
	} else {
		s.desired = 0
	}

	for id := 1; id <= s.desired; id++ {
		if s.instances[id] == nil {
			s.instances[id] = &instance{backoff: 1}
		}
	}

	for _, id := range slices.Sorted(maps.Keys(s.instances)) {
		inst := s.instances[id]
		if inst.cmd != nil {
			// On Linux a wrapper (sh, go run) can exit on SIGTERM before the program it
			// started: the instance lasts while its process group does.
			if !inst.exited() || groupAlive(inst.pid) {
				switch {
				case id > s.desired && !inst.stopping():
					inst.stoppingSince = now
					requestStop(inst.pid)
				case inst.stopping() && now.Sub(inst.stoppingSince) > stopTimeout:
					say(t("sup_stop_timeout", "id", id, "seconds", int(stopTimeout.Seconds())))
					killTree(inst.pid)
				case inst.streak > 0 && !inst.stopping() && now.Sub(inst.started) >= healthyAfter:
					if inst.streak >= crashLoopAfter {
						say(t("sup_recovered", "id", id))
					}
					inst.streak = 0
				}
				continue
			}

			exit := inst.exit
			inst.lastExit = &exit
			inst.cmd = nil
			if inst.stopping() {
				say(t("sup_stopped_one", "id", id, "exit", exit))
			} else {
				// Unrequested exit: a crash. A long healthy run resets the backoff and the streak.
				if now.Sub(inst.started) >= healthyAfter {
					inst.backoff, inst.streak = 1, 1
				} else {
					inst.backoff, inst.streak = min(backoffMaxS, inst.backoff*2), inst.streak+1
				}
				inst.nextStart = now.Add(time.Duration(inst.backoff) * time.Second)
				inst.restarts++
				s.lastCrash = &crash{ID: id, At: unixSeconds(now), Exit: exit}
				say(t("sup_crashed", "id", id, "exit", exit, "seconds", inst.backoff))
				if inst.streak == crashLoopAfter {
					say(t("sup_crash_loop", "id", id, "count", inst.streak))
				}
			}
			if inst.restartAfterStop {
				inst.nextStart = time.Time{}
			}
			inst.stoppingSince = time.Time{}
			inst.restartAfterStop = false
		}

		if id > s.desired {
			delete(s.instances, id)
			continue
		}
		if !now.Before(inst.nextStart) {
			if err := spawn(id, inst); err != nil {
				inst.backoff = min(backoffMaxS, inst.backoff*2)
				inst.nextStart = now.Add(time.Duration(inst.backoff) * time.Second)
				say(t("sup_spawn_failed", "id", id, "seconds", inst.backoff) + ": " + err.Error())
				continue
			}
			inst.started = now
			say(t("sup_running", "id", id, "pid", inst.pid, "log", inst.log))
		}
	}

	if now.Sub(s.prunedAt) >= pruneEvery {
		var active []string
		for _, inst := range s.instances {
			if inst.cmd != nil {
				active = append(active, inst.log)
			}
		}
		s.logUsage = pruneLogs(cfg.logMaxBytes, active)
		s.prunedAt = now
	}
	s.writeState(false)
	return nil
}

func runSupervisor() int {
	lock, err := lockFile(lockPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, t("sup_already_running"))
		return 1
	}
	defer lock.Close()
	setTitle(t("supervisor_window", "env", environmentTag()))

	s := &supervisor{instances: map[int]*instance{}, running: true, startedAt: time.Now(), signals: make(chan os.Signal, 1)}
	var d desiredCount
	readJSON(desiredFile, &d)
	s.desired = d.Count
	// On Windows, closing the window, logging off and shutting down arrive as SIGTERM.
	signal.Notify(s.signals, os.Interrupt, syscall.SIGTERM)

	environment := cfg.environment
	if environment == "" {
		environment = "-"
	}
	say(t("sup_started", "pid", os.Getpid(), "environment", environment, "command", strings.Join(cfg.command, " "), "count", s.desired))
	stopLeftovers()

	var lastError string
	var lastErrorAt, failingSince time.Time
	for {
		now := time.Now()
		if err := s.step(now); err != nil {
			if err.Error() != lastError || now.Sub(lastErrorAt) >= time.Minute {
				say(t("sup_error", "error", err))
				lastError, lastErrorAt = err.Error(), now
			}
			// A pass that never succeeds supervises nothing and cannot even stop. Exiting
			// without `stopped` turns it into a dead supervisor: the menu says so, and
			// the next one ends the instances left behind.
			if failingSince.IsZero() {
				failingSince = now
			}
			if now.Sub(failingSince) >= failingExit {
				say(t("sup_failing_exit", "seconds", int(failingExit.Seconds()), "error", err))
				return 1
			}
		} else {
			failingSince = time.Time{}
		}
		if !s.running && len(s.instances) == 0 {
			break
		}
		time.Sleep(tick)
	}
	s.writeState(true)
	say(t("sup_stopped"))
	return 0
}
