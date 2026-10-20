package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The menu never owns an instance: it shows run/state.json and drops commands
// for the supervisor, so closing its window changes nothing.

const (
	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	bold   = "\033[1m"
	reset  = "\033[0m"

	refresh = 2 * time.Second
	// The main screen is laid out for a default 30-line console; the problem list
	// takes what is left, up to maxProblemRows.
	windowLines    = 30
	maxProblemRows = 6
	// Each grid column is filled before the next starts: columns of 5 up to
	// gridShortMax instances, then of 10, so the 60-instance cap is 6 x 10.
	gridShortMax = 20
	sampleEvery  = 5 * time.Second
)

type counters struct {
	cpuMs int64
	mem   int64
}

// Keys come from one goroutine that reads stdin for the life of the menu. The
// console stays in key mode (no line input, no echo) the whole time; ask()
// does the line editing itself, so a key wait and a line prompt never fight
// over a read that is already blocked.
var (
	input        = make(chan byte, 256)
	interactive  bool
	restoreInput = func() {}
)

func quit(code int) {
	restoreInput()
	os.Exit(code)
}

func startInput() {
	interactive = isTerminal()
	if interactive {
		restoreInput = keyMode()
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			for _, b := range buf[:n] {
				input <- b
			}
			if err != nil {
				close(input)
				return
			}
		}
	}()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		fmt.Println()
		quit(130)
	}()
}

func nextByte() byte {
	b, ok := <-input
	if !ok {
		quit(0)
	}
	return b
}

// readLine reads up to Enter, starting with the bytes in first. In a terminal
// it echoes and handles Backspace, as the console does with line input on.
func readLine(first ...byte) string {
	var line []byte
	for i := 0; ; i++ {
		var b byte
		if i < len(first) {
			b = first[i]
		} else {
			b = nextByte()
		}
		switch {
		case b == '\n' || (b == '\r' && interactive):
			if interactive {
				fmt.Println()
			}
			return strings.TrimSpace(string(line))
		case b == 8 || b == 127:
			if interactive && len(line) > 0 {
				_, size := utf8.DecodeLastRune(line)
				line = line[:len(line)-size]
				fmt.Print("\b \b")
			}
		case b >= 0x20:
			line = append(line, b)
			if interactive {
				os.Stdout.Write([]byte{b})
			}
		}
	}
}

func ask(prompt string) string {
	fmt.Print(prompt)
	return readLine()
}

// readKey returns the first key in keys (any key when keys is "") pressed
// within timeout; other keys are ignored. Without a terminal (input from a
// pipe or a file) it returns the next line instead.
func readKey(keys string, timeout time.Duration) (string, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case b, ok := <-input:
			if !ok {
				quit(0)
			}
			if !interactive {
				return readLine(b), true
			}
			key := strings.ToUpper(string(rune(b)))
			if keys == "" || strings.Contains(keys, key) {
				return key, true
			}
		case <-deadline.C:
			return "", false
		}
	}
}

// readChoice is one menu key, or "1" (refresh) when none is pressed in time.
func readChoice() string {
	key, ok := readKey("1234567890", refresh)
	fmt.Println()
	if !ok {
		return "1"
	}
	return key
}

// backPressed is true when any key is pressed within a second; the live log
// views poll on it.
func backPressed() bool {
	_, ok := readKey("", time.Second)
	return ok
}

func confirm(question string) bool {
	return strings.EqualFold(ask(question+t("confirm_suffix")), t("confirm_yes"))
}

func pause() {
	ask(t("press_enter"))
}

func send(c command) {
	file := filepath.Join(cmdDir, fmt.Sprintf("%d-%d.json", time.Now().UnixNano(), os.Getpid()))
	if err := writeJSON(file, c); err != nil {
		fmt.Println(red + t("send_failed", "file", file) + reset)
		pause()
		return
	}
	// Let the supervisor apply it before the next screen is drawn.
	time.Sleep(1200 * time.Millisecond)
}

// coloredEnvironmentTag is ` [STAGING]`: red for production, yellow otherwise.
func coloredEnvironmentTag() string {
	if cfg.environment == "" {
		return ""
	}
	color := yellow + bold
	if cfg.environment == "production" || cfg.environment == "prod" {
		color = red + bold
	}
	return color + environmentTag() + reset
}

func startSupervisor() {
	if err := startSupervisorProcess(); err != nil {
		fmt.Println(red + err.Error() + reset)
		pause()
		return
	}
	for i := 0; i < 20 && !supervisorRunning(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
}

func duration(d time.Duration) string {
	s := int(d.Seconds())
	days, h, m, s := s/86400, s%86400/3600, s%3600/60, s%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %02dh %02dm %02ds", days, h, m, s)
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

type procUsage struct {
	cpu *float64 // nil until there are two samples
	mem int64
}

type usageSample struct {
	procs map[int]procUsage
	ram   *[2]int64 // free, total
}

var (
	lastSample  usageSample
	previousCPU map[int]int64
	previousAt  time.Time
)

// usage is the CPU % of the whole machine (as Task Manager shows it) and the
// memory per PID, plus the server's free/total RAM, sampled at most every
// sampleEvery. CPU % needs two samples, so it is nil at first.
func usage(pids []int) usageSample {
	now := time.Now()
	if len(pids) == 0 || now.Sub(previousAt) < sampleEvery {
		return lastSample
	}
	procs, ram := readCounters(pids)
	elapsedMs := float64(now.Sub(previousAt).Milliseconds())
	lastSample = usageSample{procs: map[int]procUsage{}, ram: ram}
	current := map[int]int64{}
	for pid, c := range procs {
		u := procUsage{mem: c.mem}
		if previous, ok := previousCPU[pid]; ok {
			cpu := max(0, float64(c.cpuMs-previous)/elapsedMs/float64(runtime.NumCPU())*100)
			u.cpu = &cpu
		}
		lastSample.procs[pid] = u
		current[pid] = c.cpuMs
	}
	previousCPU, previousAt = current, now
	return lastSample
}

func pidsOf(instances []instanceState) []int {
	var pids []int
	for _, i := range instances {
		if i.PID != nil {
			pids = append(pids, *i.PID)
		}
	}
	return pids
}

// usageText is the CPU and memory: blank without a process, "--" until it is sampled.
func usageText(procs map[int]procUsage, pid *int) (string, string) {
	if pid == nil {
		return "", ""
	}
	u, ok := procs[*pid]
	// No memory yet means it was sampled while still starting.
	if !ok || u.mem == 0 {
		return "--", ""
	}
	cpu := "--"
	if u.cpu != nil {
		cpu = fmt.Sprintf("%.1f%%", *u.cpu)
	}
	return cpu, fmt.Sprintf("%dMB", int(math.Round(float64(u.mem)/1048576)))
}

func orDash(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func secondsUntil(at *float64, now time.Time) int {
	if at == nil {
		return 0
	}
	return max(0, int(math.Ceil(*at-unixSeconds(now))))
}

func printRows(instances []instanceState, procs map[int]procUsage) {
	headings := []string{t("col_no"), t("col_status"), t("col_pid"), t("col_cpu"), t("col_memory"), t("col_uptime"), t("col_restarts"), t("col_last_exit")}
	// Each column at least as wide as its heading, which differs per language.
	widths := []int{3, 22, 7, 6, 7, 15, 8, 0}
	for c, h := range headings {
		widths[c] = max(widths[c], visibleLength(h))
	}
	row := func(cells []string) {
		for c := range cells {
			cells[c] = pad(cells[c], widths[c])
		}
		fmt.Println(strings.TrimRight("  "+strings.Join(cells, " "), " "))
	}
	row(headings)
	now := time.Now()
	for _, i := range instances {
		var color, label string
		switch {
		case inCrashLoop(i):
			color, label = red, t("status_crash_loop_count", "count", i.CrashStreak)
		case i.Status == "running":
			color, label = green, t("status_healthy")
		case i.Status == "stopping":
			color, label = yellow, t("status_stopping")
		default:
			color, label = red, t("status_restarting_in", "seconds", secondsUntil(i.NextStart, now))
		}
		cpu, mem := usageText(procs, i.PID)
		if cpu == "" {
			cpu = "-"
		}
		if mem == "" {
			mem = "-"
		}
		uptime := "-"
		if i.Started != nil {
			uptime = duration(now.Sub(fromUnixSeconds(*i.Started)))
		}
		row([]string{strconv.Itoa(i.ID), color + label + reset, orDash(i.PID), cpu, mem, uptime, strconv.Itoa(i.Restarts), orDash(i.LastExit)})
	}
}

// currentInstances is the instances, or none while the supervisor is down.
func currentInstances() []instanceState {
	if !supervisorRunning() {
		return nil
	}
	var s state
	readJSON(stateFile, &s)
	return s.Instances
}

var colorCode = regexp.MustCompile(`\033\[[0-9;]*m`)

// visibleLength is the length as it shows on screen: colour codes take no
// room, a line-drawing character one.
func visibleLength(text string) int {
	return utf8.RuneCountInString(colorCode.ReplaceAllString(text, ""))
}

func pad(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-visibleLength(text)))
}

type statusRow struct {
	key    string
	values []string
}

// statusTable is `Key       : a  ·  b  ·  c`, with the `·` separators aligned across rows.
func statusTable(rows []statusRow) []string {
	var widths []int
	for _, r := range rows {
		for c, v := range r.values[:len(r.values)-1] {
			if c == len(widths) {
				widths = append(widths, 0)
			}
			widths[c] = max(widths[c], visibleLength(v))
		}
	}
	var lines []string
	for _, r := range rows {
		cells := slices.Clone(r.values)
		for c := range cells[:len(cells)-1] {
			cells[c] = pad(cells[c], widths[c])
		}
		lines = append(lines, pad(r.key, 10)+": "+strings.Join(cells, "  ·  "))
	}
	return lines
}

// inCrashLoop is true after crashLoopAfter crashes in a row, even while the
// instance is briefly up again.
func inCrashLoop(i instanceState) bool {
	return i.Status != "stopping" && i.CrashStreak >= crashLoopAfter
}

// problemLine is one line per instance that is not healthy, for the main
// screen; [7] has the full table.
func problemLine(i instanceState, now time.Time) string {
	id := fmt.Sprintf("  #%-3d ", i.ID)
	wait := secondsUntil(i.NextStart, now)
	switch {
	case inCrashLoop(i):
		next := t("problem_retrying")
		if i.Status == "waiting" {
			next = t("problem_retry_in", "seconds", wait)
		}
		return id + red + bold + t("status_crash_loop") + reset + " " +
			t("problem_crash_loop", "count", i.CrashStreak, "exit", orDash(i.LastExit), "next", next)
	case i.Status == "stopping":
		since := 0
		if i.StoppingSince != nil {
			since = max(0, int(unixSeconds(now)-*i.StoppingSince))
		}
		return id + yellow + t("status_stopping") + reset + " " + t("problem_stopping", "seconds", since, "pid", orDash(i.PID))
	default:
		return id + red + t("status_restarting") + reset + " " +
			t("problem_restarting", "seconds", wait, "count", i.Restarts, "exit", orDash(i.LastExit))
	}
}

type menuEntry struct {
	key    string
	labels []string // the label shown, then the other labels it can switch to
}

// menuLines is the menu as three rows, filled down each column like the grid.
// Column widths fit every label [3] and [8] can switch to, so the menu does
// not shift.
func menuLines(supervisor bool) []string {
	supervisorLabel := t("menu_supervisor_start")
	if supervisor {
		supervisorLabel = t("menu_supervisor_stop")
	}
	columns := [][]menuEntry{
		{
			{"1", []string{t("menu_refresh")}},
			{"2", []string{t("menu_scale")}},
			{"3", []string{startStopLabel(), t("menu_stop_all"), t("menu_start_count", "count", maxInstances), t("menu_start")}},
		},
		{{"4", []string{t("menu_restart")}}, {"5", []string{t("menu_log")}}, {"6", []string{t("menu_log_folder")}}},
		{
			{"7", []string{t("menu_show_all")}},
			{"8", []string{supervisorLabel, t("menu_supervisor_stop"), t("menu_supervisor_start")}},
			{"0", []string{t("menu_exit")}},
		},
	}
	lines := []string{"  ", "  ", "  "}
	for _, column := range columns {
		width := 0
		for _, entry := range column {
			for _, label := range entry.labels {
				width = max(width, visibleLength(label))
			}
		}
		for r, entry := range column {
			lines[r] += pad("["+entry.key+"] "+entry.labels[0], width+6)
		}
	}
	for r := range lines {
		lines[r] = strings.TrimRight(lines[r], " ")
	}
	return lines
}

// desired is the count that should run and, after a stop to 0, the count
// before it. Read from desired.json, the supervisor's own record, so it holds
// while it is down.
func desired() (int, int) {
	var d desiredCount
	readJSON(desiredFile, &d)
	return d.Count, d.Previous
}

// startStopLabel is [3]: stop all while instances should run, else start the
// count from before the stop.
func startStopLabel() string {
	count, previous := desired()
	switch {
	case count > 0:
		return t("menu_stop_all")
	case previous > 0:
		return t("menu_start_count", "count", previous)
	default:
		return t("menu_start")
	}
}

func startStop() {
	count, previous := desired()
	switch {
	case count > 0:
		if confirm(t("confirm_stop_all")) {
			send(command{Action: "scale", Count: 0})
		}
	case previous == 0:
		scale()
	case confirm(t("confirm_start_count", "count", previous)):
		send(command{Action: "scale", Count: previous})
		if !supervisorRunning() {
			startSupervisor()
		}
	}
}

// draw is the main screen, laid out for a default 30-line console at up to the
// 60-instance cap: status lines, the instance grid in a box, then the instances
// that are not healthy in whatever room is left. [7] shows every row in full.
func draw() {
	supervisor := supervisorRunning()
	var st state
	st.Stopped = true
	readJSON(stateFile, &st)
	instances := currentInstances()
	// A crash-looping instance counts as restarting even while it is briefly up,
	// so the screen does not flicker between healthy and restarting.
	var looping, others []instanceState
	healthy, restarting, stopping := 0, 0, 0
	for _, i := range instances {
		switch {
		case inCrashLoop(i):
			looping = append(looping, i)
			restarting++
		case i.Status == "running":
			healthy++
		case i.Status == "stopping":
			others = append(others, i)
			stopping++
		default:
			others = append(others, i)
			restarting++
		}
	}
	problems := append(looping, others...)
	procs := usage(pidsOf(instances)).procs
	ram := lastSample.ram
	now := time.Now()

	// Numbered down each column, then across, like the menu below.
	rows, columns := 0, 0
	if len(instances) > 0 {
		rows = min(len(instances), 5)
		if len(instances) > gridShortMax {
			rows = min(len(instances), 10)
		}
		columns = (len(instances) + rows - 1) / rows
	}
	menu := menuLines(supervisor)
	right := columns*19 + 1
	for _, line := range menu {
		right = max(right, visibleLength(line))
	}

	title := bold + t("manager_heading") + reset + coloredEnvironmentTag()
	lines := []string{pad(title, right-19) + now.Format("2006-01-02 15:04:05")}
	logOver := supervisor && st.LogOver

	if !supervisor {
		lines = append(lines, t("supervisor_label")+": "+red+t("supervisor_down")+reset, "")
		if !st.Stopped {
			lines = append(lines, red+bold+t("supervisor_died")+reset)
		} else {
			lines = append(lines, red+t("supervisor_stopped")+reset)
		}
	} else {
		gb := func(bytes int64) string { return fmt.Sprintf("%.1f", float64(bytes)/1073741824) }
		// MB below 1 GB, so a few instances do not all read 0.1 GB.
		memory := func(bytes int64) string {
			if bytes < 1073741824 {
				return fmt.Sprintf("%d MB", int(math.Round(float64(bytes)/1048576)))
			}
			return gb(bytes) + " GB"
		}
		// Instances not sampled yet (just started) are left out until they are.
		var cpu float64
		var memTotal int64
		sampled := false
		for _, u := range procs {
			memTotal += u.mem
			if u.cpu != nil {
				cpu += *u.cpu
				sampled = true
			}
		}
		cpuText := "--"
		if sampled {
			cpuText = fmt.Sprintf("%.1f%%", cpu)
		}
		totalRestarts := 0
		for _, i := range instances {
			totalRestarts += i.Restarts
		}
		restarts := t("info_restarts", "count", totalRestarts)
		if last := st.LastCrash; last != nil {
			restarts += " " + t("info_last_crash", "id", last.ID, "time", fromUnixSeconds(last.At).Format("15:04"), "exit", last.Exit)
		}
		info := []string{restarts}
		if ram != nil {
			info = append(info, t("info_ram", "free", gb(ram[0]), "total", int(math.Round(float64(ram[1])/1073741824))))
		}
		if st.LogMaxBytes > 0 {
			color := ""
			if logOver {
				color = yellow
			}
			info = append(info, t("info_log")+" "+color+fmt.Sprintf("%d/%d MB", int(math.Round(float64(st.LogBytes)/1048576)), int(math.Round(float64(st.LogMaxBytes)/1048576)))+reset)
		}

		lines = append(lines, t("supervisor_label")+": "+green+t("supervisor_running")+reset+" "+
			t("elapsed", "duration", duration(now.Sub(fromUnixSeconds(st.SupervisorStarted)))))
		lines = append(lines, statusTable([]statusRow{
			{t("row_instances"), []string{
				t("count_healthy", "count", healthy),
				t("count_restarting", "count", restarting),
				t("count_stopping", "count", stopping, "target", st.Desired),
			}},
			{t("row_total"), []string{t("total_cpu") + " " + cpuText, t("total_memory") + " " + memory(memTotal)}},
			{t("row_info"), info},
		})...)
		if logOver {
			lines = append(lines, yellow+t("log_over")+reset)
		}
		if len(looping) > 0 && len(looping) == len(instances) {
			lines = append(lines, red+bold+t("all_crash_loop")+reset)
		} else if len(looping) > 0 {
			lines = append(lines, red+bold+t("some_crash_loop", "count", len(looping))+reset)
		}

		if len(instances) == 0 {
			// Stopped on purpose ([3], or [2] → 0) reads differently from never set up.
			count, previous := desired()
			lines = append(lines, "")
			if count == 0 && previous > 0 {
				lines = append(lines, yellow+t("all_stopped", "count", previous)+reset)
			} else {
				lines = append(lines, t("no_instances"))
			}
		} else {
			segments := slices.Repeat([]string{strings.Repeat("─", 18)}, columns)
			lines = append(lines, "┌"+strings.Join(segments, "┬")+"┐")
			for r := 0; r < rows; r++ {
				var cells []string
				for c := 0; c < columns; c++ {
					n := c*rows + r
					if n >= len(instances) {
						cells = append(cells, strings.Repeat(" ", 18))
						continue
					}
					i := instances[n]
					color, letter := red, "R"
					switch {
					case inCrashLoop(i):
						color, letter = red, "C"
					case i.Status == "running":
						color, letter = green, "H"
					case i.Status == "stopping":
						color, letter = yellow, "S"
					}
					cpu, mem := usageText(procs, i.PID)
					cells = append(cells, fmt.Sprintf(" %2d %s%s%s %5s %5s ", i.ID, color+bold, letter, reset, cpu, mem))
				}
				lines = append(lines, "│"+strings.Join(cells, "│")+"│")
			}
			lines = append(lines, "└"+strings.Join(segments, "┴")+"┘")
			lines = append(lines, " "+green+"H"+reset+" = "+t("legend_healthy")+"   "+red+"R"+reset+" = "+t("legend_restarting")+"   "+
				yellow+"S"+reset+" = "+t("legend_stopping")+"   "+red+"C"+reset+" = "+t("legend_crash_loop"))

			if len(problems) == 0 {
				lines = append(lines, "", green+t("all_healthy")+reset+" "+t("all_healthy_hint"))
			} else {
				// What the window has left after the heading, the blank line, the menu and the prompt.
				room := windowLines - (len(lines) + 1 + 5)
				show := len(problems)
				if show > min(room, maxProblemRows) {
					show = max(1, min(maxProblemRows, room-1))
				}
				lines = append(lines, yellow+t("needs_attention")+reset)
				for _, i := range problems[:show] {
					lines = append(lines, problemLine(i, now))
				}
				if len(problems) > show {
					lines = append(lines, "  "+t("more_problems", "count", len(problems)-show))
				}
			}
		}
	}

	lines = append(lines, "")
	lines = append(lines, menu...)
	// Redrawn in place (home, clear each line's tail, clear below) so a refresh does not flicker.
	fmt.Print("\033[H" + strings.Join(lines, "\033[K\n") + "\033[K\n\033[J")
}

func showAll() {
	fmt.Print("\033[2J\033[H" + bold + t("all_heading") + reset + "   " + time.Now().Format("2006-01-02 15:04:05") + "\n")
	instances := currentInstances()
	if len(instances) == 0 {
		fmt.Println(t("none_running"))
	} else {
		printRows(instances, usage(pidsOf(instances)).procs)
	}
	fmt.Println()
	pause()
}

func scale() {
	answer := ask(t("ask_count", "max", maxInstances))
	if answer == "" {
		return
	}
	count, err := strconv.Atoi(answer)
	if err != nil || count < 0 || count > maxInstances {
		fmt.Println(red + t("bad_count", "max", maxInstances) + reset)
		pause()
		return
	}
	if confirm(t("confirm_scale", "count", count)) {
		send(command{Action: "scale", Count: count})
		// The supervisor applies the command before starting any instance.
		if count > 0 && !supervisorRunning() {
			startSupervisor()
		}
	}
}

func setManagerTitle(suffix string) {
	setTitle(t("manager_window", "env", environmentTag()) + suffix)
}

func instanceLog(id string) string {
	for _, i := range currentInstances() {
		if strconv.Itoa(i.ID) == id && i.Log != nil {
			return filepath.Join(logDir, *i.Log)
		}
	}
	return ""
}

// readNew returns what was added to file since offset, at most the last max
// bytes, the new offset, and whether older bytes were skipped. A file that
// shrank (rotated) is read from its start.
func readNew(file string, offset, maxBytes int64) (string, int64, bool) {
	fi, err := os.Stat(file)
	if err != nil || fi.Size() == offset {
		return "", offset, false
	}
	size := fi.Size()
	if size < offset {
		offset = 0
	}
	skipped := size-offset > maxBytes
	if skipped {
		offset = size - maxBytes
	}
	f, err := os.Open(file)
	if err != nil {
		return "", offset, skipped
	}
	defer f.Close()
	f.Seek(offset, io.SeekStart)
	raw, _ := io.ReadAll(f)
	return string(raw), offset + int64(len(raw)), skipped
}

// printNew prints what was added to file since offset, at most the last 16 KB,
// and returns the new offset.
func printNew(file string, offset int64) int64 {
	text, offset, skipped := readNew(file, offset, 16384)
	if skipped {
		fmt.Println(yellow + t("log_skipped") + reset)
	}
	fmt.Print(text)
	return offset
}

type logSource struct {
	file    string
	offset  int64
	partial string
}

// showAllLogs prints new output of every running instance and of
// supervisor.log, line by line with a coloured [N] prefix, until a key is
// pressed. Lines are shown in the order they are read, once a second; the
// exact time is whatever the service prints itself. Nothing older than the
// moment the view opens: without timestamps of our own, old lines from several
// files cannot be put in order.
func showAllLogs() {
	// Colours as docker compose uses them; red is left out, it means trouble here.
	colours := []string{"\033[36m", "\033[33m", "\033[32m", "\033[35m", "\033[34m", "\033[96m", "\033[93m", "\033[92m", "\033[95m", "\033[94m"}
	setManagerTitle(" - " + t("log_all_window"))
	fmt.Print("\033[2J\033[H" + bold + t("log_all_heading") + reset + "\n---\n")
	sources := map[string]logSource{}
	first := true
	for {
		keys := []string{"supervisor"}
		files := map[string]string{"supervisor": filepath.Join(logDir, "supervisor.log")}
		for _, i := range currentInstances() {
			if i.Log != nil {
				key := strconv.Itoa(i.ID)
				keys = append(keys, key)
				files[key] = filepath.Join(logDir, *i.Log)
			}
		}
		// Shared by all sources, so 60 busy instances cannot flood the console.
		maxBytes := max(1024, 16384/int64(len(files)))
		for _, key := range keys {
			file := files[key]
			prefix := bold + "[" + t("log_prefix_supervisor") + "]" + reset + " "
			if key != "supervisor" {
				n, _ := strconv.Atoi(key)
				prefix = colours[(n-1)%len(colours)] + fmt.Sprintf("[%2d]", n) + reset + " "
			}
			source, ok := sources[key]
			if !ok || source.file != file {
				// Open: from the end. Started or restarted since: the new file from its start.
				source = logSource{file: file}
				if fi, err := os.Stat(file); first && err == nil {
					source.offset = fi.Size()
				}
			}
			text, offset, skipped := readNew(file, source.offset, maxBytes)
			source.offset = offset
			if skipped {
				fmt.Println(prefix + yellow + t("log_skipped") + reset)
				source.partial = ""
			}
			// A line is shown once it is complete, so a prefix never lands mid-line,
			// or after a second without more output, so a last line without "\n" shows too.
			lines := strings.Split(source.partial+text, "\n")
			source.partial = ""
			if text != "" {
				source.partial = lines[len(lines)-1]
				lines = lines[:len(lines)-1]
			}
			for _, line := range lines {
				if line != "" || text != "" {
					fmt.Println(prefix + strings.TrimRight(line, "\r"))
				}
			}
			sources[key] = source
		}
		for key := range sources {
			if _, ok := files[key]; !ok {
				delete(sources, key)
			}
		}
		first = false
		if backPressed() {
			break
		}
	}
	setManagerTitle("")
}

// showLog prints the last 40 lines of an instance's log, then new output as it
// is written, until a key is pressed.
func showLog() {
	id := ask(t("ask_log_id"))
	if id == "" {
		return
	}
	if strings.EqualFold(id, "a") {
		showAllLogs()
		return
	}
	file := instanceLog(id)
	fi, err := os.Stat(file)
	if file == "" || err != nil {
		fmt.Println(red + t("log_not_found", "id", id) + reset)
		pause()
		return
	}
	setManagerTitle(" - " + t("log_window", "id", id))
	// A screen of its own, so the menu above it does not look still active.
	fmt.Print("\033[2J\033[H" + bold + t("log_heading", "id", id) + reset + "\n" + file + "\n---\n")
	text, offset, _ := readNew(file, 0, min(fi.Size(), 16384))
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	fmt.Println(strings.Join(lines[max(0, len(lines)-40):], "\n"))
	for !backPressed() {
		offset = printNew(file, offset)
		if current := instanceLog(id); current != "" && current != file {
			// The instance restarted into a new file.
			file = current
			fmt.Println(yellow + "--- " + t("log_new_file", "id", id, "file", filepath.Base(file)) + " ---" + reset)
			offset = printNew(file, 0)
		}
	}
	setManagerTitle("")
}

// configError is what is wrong with procdeck.ini, or "" when the menu can start.
func configError() string {
	program := ""
	if len(cfg.command) > 0 {
		program = cfg.command[0]
	}
	vars := []any{"ini", iniPath, "workdir", cfg.workdir, "program", program}
	switch {
	case program == "":
		return t("config_command_lines", vars...)
	case iniValue("workdir") == "":
		return t("config_workdir_required", vars...)
	}
	if fi, err := os.Stat(cfg.workdir); err != nil || !fi.IsDir() {
		return t("workdir_not_found", vars...)
	}
	if strings.ContainsAny(program, `\/`) {
		if fi, err := os.Stat(program); err != nil || fi.IsDir() {
			return t("command_not_found", vars...)
		}
	} else if _, err := exec.LookPath(program); err != nil {
		// A bare name (php, go, cmd.exe) is looked up on the PATH, as starting it will.
		return t("command_not_found", vars...)
	}
	return ""
}

func runManager() {
	setupConsole()
	startInput()
	setManagerTitle("")
	if err := configError(); err != "" {
		fmt.Println(red + err + reset)
		pause()
		quit(1)
	}
	if !supervisorRunning() {
		startSupervisor()
	}

	for {
		draw()
		fmt.Print(t("prompt_choice", "seconds", int(refresh.Seconds())))
		switch readChoice() {
		case "", "1":
		case "2":
			scale()
		case "3":
			startStop()
		case "4":
			id := ask(t("ask_restart_id"))
			if strings.EqualFold(id, "a") {
				if confirm(t("confirm_restart_all")) {
					send(command{Action: "restart", ID: "all"})
				}
			} else if n, err := strconv.Atoi(id); err == nil && n > 0 && confirm(t("confirm_restart_one", "id", n)) {
				send(command{Action: "restart", ID: n})
			}
		case "5":
			showLog()
		case "6":
			if !openFolder(logDir) {
				fmt.Println(t("log_folder", "path", logDir))
				pause()
			}
		case "7":
			showAll()
		case "8":
			if !supervisorRunning() {
				startSupervisor()
			} else if confirm(t("confirm_shutdown")) {
				send(command{Action: "shutdown", At: unixSeconds(time.Now())})
			}
		case "0":
			// A supervisor that died leaves its instances running unwatched.
			var st state
			st.Stopped = true
			readJSON(stateFile, &st)
			switch {
			case supervisorRunning() && runtime.GOOS != "windows":
				fmt.Println(t("exit_message_background"))
			case supervisorRunning():
				fmt.Println(t("exit_message", "window", t("supervisor_window", "env", environmentTag())))
			case !st.Stopped:
				fmt.Println(t("exit_message_died"))
			default:
				fmt.Println(t("exit_message_down"))
			}
			quit(0)
		default:
			fmt.Println(red + t("unknown_choice") + reset)
			pause()
		}
	}
}
