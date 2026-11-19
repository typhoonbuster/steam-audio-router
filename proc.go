package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// commMax is TASK_COMM_LEN - 1: the kernel truncates `comm` to 15 bytes.
const commMax = 15

// clockTicksPerSecond is USER_HZ. The starttime field of /proc/<pid>/stat is
// always expressed in USER_HZ ticks (100 on every Linux architecture),
// independently of the kernel's internal HZ.
const clockTicksPerSecond = 100.0

// ProcStat is the subset of /proc/<pid>/stat we need.
type ProcStat struct {
	PID       int
	Comm      string
	PPID      int
	StartTime int64 // clock ticks since boot (field 22)
}

func (p ProcStat) Label() string { return fmt.Sprintf("%s[%d]", p.Comm, p.PID) }

// ProcFS reads process information from a procfs root (overridable in tests).
type ProcFS struct{ Root string }

var procfs = ProcFS{Root: "/proc"}

// Stat parses /proc/<pid>/stat. ok is false when the process is gone or
// unreadable. comm may contain spaces and parentheses, so we anchor on the
// last ')'.
func (fs ProcFS) Stat(pid int) (ProcStat, bool) {
	if pid <= 0 {
		return ProcStat{}, false
	}
	raw, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", fs.Root, pid))
	if err != nil {
		return ProcStat{}, false
	}
	text := string(raw)
	lp := strings.IndexByte(text, '(')
	rp := strings.LastIndexByte(text, ')')
	if lp < 0 || rp < lp || rp+2 > len(text) {
		return ProcStat{}, false
	}
	comm := text[lp+1 : rp]
	rest := strings.Fields(text[rp+2:])
	// rest[0] = field 3 (state), rest[1] = field 4 (ppid), starttime = field 22 -> rest[19]
	if len(rest) < 20 {
		return ProcStat{}, false
	}
	ppid, err1 := strconv.Atoi(rest[1])
	start, err2 := strconv.ParseInt(rest[19], 10, 64)
	if err1 != nil || err2 != nil {
		return ProcStat{}, false
	}
	return ProcStat{PID: pid, Comm: comm, PPID: ppid, StartTime: start}, true
}

// Ancestry returns [process, parent, grandparent, ..., PID 1]. It stops
// cleanly if a link disappeared meanwhile and guards against loops.
func (fs ProcFS) Ancestry(pid int) []ProcStat {
	var chain []ProcStat
	seen := map[int]bool{}
	for cur := pid; cur > 0 && !seen[cur] && len(chain) < 128; {
		seen[cur] = true
		st, ok := fs.Stat(cur)
		if !ok {
			break
		}
		chain = append(chain, st)
		if cur == 1 {
			break
		}
		cur = st.PPID
	}
	return chain
}

// IsDescendantOrSelf reports whether root is pid itself or one of its ancestors.
func (fs ProcFS) IsDescendantOrSelf(pid, root int) bool {
	for _, st := range fs.Ancestry(pid) {
		if st.PID == root {
			return true
		}
	}
	return false
}

// BootTime returns the btime field of /proc/stat (seconds since the epoch).
func (fs ProcFS) BootTime() float64 {
	raw, err := os.ReadFile(fs.Root + "/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "btime ") {
			f, _ := strconv.ParseFloat(strings.TrimSpace(line[6:]), 64)
			return f
		}
	}
	return 0
}

// StartEpoch converts a process start time to seconds since the epoch.
func StartEpoch(bootTime float64, st ProcStat) float64 {
	return bootTime + float64(st.StartTime)/clockTicksPerSecond
}

// commMatches compares a kernel-truncated comm with configured names,
// truncating those the same way (steam-runtime-launcher-service -> steam-runtime-l).
func commMatches(comm string, names []string) bool {
	for _, n := range names {
		if len(n) > commMax {
			n = n[:commMax]
		}
		if comm == n {
			return true
		}
	}
	return false
}

// findSteamAncestor returns the first (strict, unless includeSelf) ancestor
// whose comm is a Steam process, plus the full chain for logging.
func findSteamAncestor(fs ProcFS, pid int, names []string, includeSelf bool) (*ProcStat, []ProcStat) {
	chain := fs.Ancestry(pid)
	candidates := chain
	if !includeSelf && len(chain) > 0 {
		candidates = chain[1:]
	}
	for i := range candidates {
		if commMatches(candidates[i].Comm, names) {
			st := candidates[i]
			return &st, chain
		}
	}
	return nil, chain
}

// steamRT3Marker is the directory the SteamRT3 (64-bit runtime) client runs
// from: ~/.local/share/Steam/steamrt64/steam. The legacy client lives in
// ubuntu12_32/. Only SteamRT3 is supported by this tool.
const steamRT3Marker = "/steamrt64/"

// Exe returns the resolved executable path of a process ("" if unreadable).
func (fs ProcFS) Exe(pid int) string {
	p, err := os.Readlink(fmt.Sprintf("%s/%d/exe", fs.Root, pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(p, " (deleted)")
}

// steamClientInChain returns the executable of the first ancestor whose comm
// is "steam" and whether it is the SteamRT3 client.
func steamClientInChain(fs ProcFS, chain []ProcStat) (exe string, rt3 bool) {
	for _, st := range chain {
		if st.Comm != "steam" {
			continue
		}
		if exe = fs.Exe(st.PID); exe != "" {
			return exe, strings.Contains(exe, steamRT3Marker)
		}
	}
	return "", false
}

// detectSteamClient scans /proc for running "steam" processes and reports
// their executable, used once at startup for a clear message.
func (fs ProcFS) detectSteamClient() (exe string, running bool, rt3 bool) {
	entries, err := os.ReadDir(fs.Root)
	if err != nil {
		return "", false, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, ok := fs.Stat(pid)
		if !ok || st.Comm != "steam" {
			continue
		}
		running = true
		if x := fs.Exe(pid); x != "" {
			exe = x
			if strings.Contains(x, steamRT3Marker) {
				return x, true, true
			}
		}
	}
	return exe, running, false
}

func chainString(chain []ProcStat) string {
	if len(chain) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(chain))
	for i, st := range chain {
		parts[i] = st.Label()
	}
	return strings.Join(parts, " <- ")
}
