package main

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// lockFile takes an exclusive lock on path, or fails with errLocked at once.
// Go opens files close-on-exec, so an instance never inherits the lock and
// keeps it held after the supervisor dies.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

// newGroup runs an instance in a session of its own, so a stop reaches it and
// whatever it started (a .sh and its program, `go run` and its binary). The
// PID is still the command's own.
func newGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// requestStop sends SIGTERM to the instance's process group.
func requestStop(pid int) {
	if syscall.Kill(-pid, syscall.SIGTERM) != nil {
		syscall.Kill(pid, syscall.SIGTERM)
	}
}

// killTree kills the instance's process group, or the PID alone when it has none.
func killTree(pid int) {
	if syscall.Kill(-pid, syscall.SIGKILL) != nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// groupAlive is true while a process of the group led by pid is still running.
func groupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}

// commandLines returns the command lines of the given PIDs that are still alive.
func commandLines(pids []int) map[int]string {
	out := map[int]string{}
	for _, pid := range pids {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err == nil && len(raw) > 0 {
			out[pid] = strings.ReplaceAll(string(raw), "\x00", " ")
		}
	}
	return out
}

var meminfo = regexp.MustCompile(`(?s)MemTotal:\s+(\d+).*MemAvailable:\s+(\d+)`)

// readCounters returns the CPU time and resident memory of each PID, and the
// server's free and total RAM, or nil when they cannot be read.
func readCounters(pids []int) (map[int]counters, *[2]int64) {
	procs := map[int]counters{}
	for _, pid := range pids {
		stat, err1 := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		statm, err2 := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
		if err1 != nil || err2 != nil {
			continue
		}
		// Fields after "(comm) ": utime and stime are the 12th and 13th, in 1/100 s.
		f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
		m := strings.Fields(string(statm))
		if len(f) < 13 || len(m) < 2 {
			continue
		}
		utime, _ := strconv.ParseInt(f[11], 10, 64)
		stime, _ := strconv.ParseInt(f[12], 10, 64)
		resident, _ := strconv.ParseInt(m[1], 10, 64)
		procs[pid] = counters{cpuMs: (utime + stime) * 10, mem: resident * int64(os.Getpagesize())}
	}
	var ram *[2]int64
	raw, _ := os.ReadFile("/proc/meminfo")
	if m := meminfo.FindStringSubmatch(string(raw)); m != nil {
		total, _ := strconv.ParseInt(m[1], 10, 64)
		free, _ := strconv.ParseInt(m[2], 10, 64)
		ram = &[2]int64{free * 1024, total * 1024}
	}
	return procs, ram
}

// startSupervisorProcess starts the supervisor detached from this terminal, so
// closing the menu does not end it.
func startSupervisorProcess() error {
	cmd := exec.Command(exePath, "supervisor")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped when it exits while the menu is still open.
	go cmd.Wait()
	return nil
}

func setTitle(string) {}

func setupConsole() {}

// termios gets (TCGETS) or sets (TCSETS) the terminal settings of stdin.
func termios(request uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), request, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func isTerminal() bool {
	var t syscall.Termios
	return termios(syscall.TCGETS, &t) == nil
}

// keyMode turns off line input and echo, so keys arrive one at a time; Ctrl+C
// still interrupts. It returns the function that turns them back on.
func keyMode() func() {
	var saved syscall.Termios
	if termios(syscall.TCGETS, &saved) != nil {
		return func() {}
	}
	keys := saved
	keys.Lflag &^= syscall.ICANON | syscall.ECHO
	keys.Cc[syscall.VMIN] = 1
	keys.Cc[syscall.VTIME] = 0
	termios(syscall.TCSETS, &keys)
	return func() { termios(syscall.TCSETS, &saved) }
}

// openFolder is false on Linux: the menu prints the path instead.
func openFolder(string) bool {
	return false
}
