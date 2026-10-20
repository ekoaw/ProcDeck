package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procSetConsoleTitleW     = kernel32.NewProc("SetConsoleTitleW")
)

// lockFile takes an exclusive lock on path, or fails with errLocked at once.
// Windows releases the lock with the process that took it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

// newGroup runs an instance in a process group of its own, so CTRL_BREAK
// reaches it and whatever it started, and not the supervisor.
func newGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// requestStop asks the instance to stop the way Ctrl+Break in its window would.
func requestStop(pid int) {
	windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

// killTree kills pid and every process it started.
func killTree(pid int) {
	exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// groupAlive is false on Windows: taskkill /T already reaches what an instance started.
func groupAlive(int) bool {
	return false
}

// commandLines returns the command lines of the given PIDs that are still
// alive. Only the supervisor's start calls it, so one PowerShell call is fine.
func commandLines(pids []int) map[int]string {
	filter := make([]string, len(pids))
	for i, pid := range pids {
		filter[i] = "ProcessId=" + strconv.Itoa(pid)
	}
	raw, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_Process -Filter '"+strings.Join(filter, " OR ")+
			"' | ForEach-Object { [string]$_.ProcessId + ' ' + $_.CommandLine }").Output()
	out := map[int]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		pid, command, _ := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.Atoi(pid); err == nil {
			out[n] = command
		}
	}
	return out
}

type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// readCounters returns the CPU time and working set of each PID, as Task
// Manager counts them, and the server's free and total RAM.
func readCounters(pids []int) (map[int]counters, *[2]int64) {
	procs := map[int]counters{}
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
		if err != nil {
			continue
		}
		var created, exited, kernel, user windows.Filetime
		mem := processMemoryCounters{cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
		timesErr := windows.GetProcessTimes(h, &created, &exited, &kernel, &user)
		ok, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mem)), uintptr(mem.cb))
		windows.CloseHandle(h)
		if timesErr != nil || ok == 0 {
			continue
		}
		// Filetimes count 100 ns.
		ticks := filetime(kernel) + filetime(user)
		procs[pid] = counters{cpuMs: ticks / 10_000, mem: int64(mem.WorkingSetSize)}
	}
	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); ok == 0 {
		return procs, nil
	}
	return procs, &[2]int64{int64(status.AvailPhys), int64(status.TotalPhys)}
}

func filetime(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// startSupervisorProcess opens the supervisor in a minimized console of its
// own. Closing that console ends every instance, hence its DO NOT CLOSE title.
func startSupervisorProcess() error {
	title := t("supervisor_window", "env", environmentTag())
	cmd := exec.Command("cmd.exe")
	// start's quoting is cmd's own, so the command line is written as is.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /c start "` + title + `" /MIN "` + exePath + `" supervisor`}
	return cmd.Run()
}

// setTitle names the console window, so the taskbar and Alt+Tab tell a
// staging menu from a production one.
func setTitle(title string) {
	p, err := windows.UTF16PtrFromString(title)
	if err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
}

// setupConsole turns on colour codes for the menu's console.
func setupConsole() {
	out := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(out, &mode) == nil {
		windows.SetConsoleMode(out, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}

func isTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}

// keyMode turns off line input and echo, so keys arrive one at a time; Ctrl+C
// still interrupts. It returns the function that turns them back on.
func keyMode() func() {
	in := windows.Handle(os.Stdin.Fd())
	var saved uint32
	if windows.GetConsoleMode(in, &saved) != nil {
		return func() {}
	}
	windows.SetConsoleMode(in, saved&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT))
	return func() { windows.SetConsoleMode(in, saved) }
}

// openFolder opens path in Explorer.
func openFolder(path string) bool {
	exec.Command("explorer", path).Start()
	return true
}
