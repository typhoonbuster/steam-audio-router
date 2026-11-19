package main

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// Availability of the Steam log file.
type Availability int

const (
	AvailUnknown Availability = iota
	AvailYes
	AvailNo
)

// SteamLogTailer follows Steam's logs/streaming_log.txt and keeps the set of
// PIDs whose audio Steam asked to capture (i.e. the game of the active Remote
// Play session). Steam writes, at session start and on focus changes:
//
//	Telling process <pid> to change audio capture state: enabled|disabled
//	Recording process audio: <pid>
//
// Robustness: a missing file flips Available to AvailNo (the router may fall
// back); a rotation (new inode or shrinking size, i.e. Steam restarted)
// reopens the file and clears the state; recycled PIDs are rejected by
// comparing the process start time with the log line timestamp, and start
// times are re-checked on every poll.
type SteamLogTailer struct {
	Path      string
	TailBytes int64
	Enabled   map[int]ProcStat
	Available Availability

	fs        ProcFS
	bootTime  float64
	f         *os.File
	ino       uint64
	pos       int64
	partial   []byte
	replaying bool
}

var steamLogLine = regexp.MustCompile(
	`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\]\[[0-9.]+\] ` +
		`(?:Telling process (\d+) to change audio capture state: (enabled|disabled)|Recording process audio: (\d+))`)

func newSteamLogTailer(path string, tailBytes int64, fs ProcFS) *SteamLogTailer {
	return &SteamLogTailer{Path: path, TailBytes: tailBytes, Enabled: map[int]ProcStat{}, fs: fs, bootTime: fs.BootTime()}
}

// EnabledPIDs returns the sorted PIDs Steam currently wants captured.
func (t *SteamLogTailer) EnabledPIDs() []int {
	out := make([]int, 0, len(t.Enabled))
	for pid := range t.Enabled {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func (t *SteamLogTailer) enabledLabels() []string {
	var out []string
	for _, pid := range t.EnabledPIDs() {
		out = append(out, t.Enabled[pid].Label())
	}
	return out
}

// Close releases the file handle; the next Poll reopens from scratch.
func (t *SteamLogTailer) Close() {
	if t.f != nil {
		t.f.Close()
	}
	t.f, t.ino, t.pos, t.partial = nil, 0, 0, nil
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// Poll reads new lines. It returns true when the enabled set changed.
func (t *SteamLogTailer) Poll() bool {
	changed := false
	fi, err := os.Stat(t.Path)
	if err != nil {
		if t.Available != AvailNo {
			Warnf("Steam log not readable (%v): %s", err, t.Path)
			t.Available = AvailNo
			t.Close()
			if len(t.Enabled) > 0 {
				t.Enabled = map[int]ProcStat{}
				changed = true
			}
		}
		return changed
	}
	ino := inodeOf(fi)
	if t.f == nil || ino != t.ino || fi.Size() < t.pos {
		firstOpen := t.f == nil && t.ino == 0
		rotated := t.f != nil
		t.Close()
		f, err := os.Open(t.Path)
		if err != nil {
			Warnf("Cannot open Steam log: %v", err)
			return changed
		}
		t.f, t.ino = f, ino
		if firstOpen {
			t.replaying = true
			if fi.Size() > t.TailBytes {
				// Startup: only replay the tail of the file
				if _, err := f.Seek(fi.Size()-t.TailBytes, io.SeekStart); err == nil {
					t.skipPartialLine()
				}
			}
		}
		t.pos, _ = f.Seek(0, io.SeekCurrent)
		if rotated {
			Infof("Steam log rotated (Steam restarted?): state reset")
			if len(t.Enabled) > 0 {
				t.Enabled = map[int]ProcStat{}
				changed = true
			}
		}
		if t.Available != AvailYes {
			Infof("Following Steam log: %s", t.Path)
			t.Available = AvailYes
		}
	}

	if _, err := t.f.Seek(t.pos, io.SeekStart); err != nil {
		Warnf("Steam log seek failed: %v", err)
		t.Close()
		return changed
	}
	data, err := io.ReadAll(t.f)
	if err != nil {
		Warnf("Steam log read failed: %v", err)
		t.Close()
		return changed
	}
	t.pos += int64(len(data))
	if len(data) > 0 {
		buf := append(t.partial, data...)
		lines := bytes.Split(buf, []byte{'\n'})
		t.partial = append([]byte(nil), lines[len(lines)-1]...)
		for _, raw := range lines[:len(lines)-1] {
			if t.applyLine(string(bytes.TrimRight(raw, "\r"))) {
				changed = true
			}
		}
	}
	if t.prune() {
		changed = true
	}
	if t.replaying {
		t.replaying = false
		if len(t.Enabled) > 0 {
			Infof("Initial state from Steam log: capture requested for %v", t.enabledLabels())
		} else {
			Infof("Initial state from Steam log: no process capture requested")
		}
	}
	return changed
}

func (t *SteamLogTailer) skipPartialLine() {
	one := make([]byte, 1)
	for {
		n, err := t.f.Read(one)
		if n == 0 || err != nil || one[0] == '\n' {
			return
		}
	}
}

func (t *SteamLogTailer) applyLine(line string) bool {
	m := steamLogLine.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
	if err != nil {
		ts = time.Now()
	}
	var pid int
	state := "enabled"
	if m[4] != "" {
		pid, _ = strconv.Atoi(m[4])
	} else {
		pid, _ = strconv.Atoi(m[2])
		state = m[3]
	}
	return t.apply(float64(ts.Unix()), pid, state)
}

func (t *SteamLogTailer) logf(format string, args ...any) {
	if t.replaying {
		Debugf(format, args...)
	} else {
		Infof(format, args...)
	}
}

func (t *SteamLogTailer) apply(ts float64, pid int, state string) bool {
	if state == "enabled" {
		st, ok := t.fs.Stat(pid)
		if !ok {
			Debugf("Steam log: capture requested for PID %d, which no longer exists", pid)
			return false
		}
		// Recycled PID: the current process started after the log line was written
		if StartEpoch(t.bootTime, st) > ts+2.0 {
			Debugf("Steam log: PID %d was recycled (started after the log line), ignored", pid)
			return false
		}
		if cur, exists := t.Enabled[pid]; exists && cur.StartTime == st.StartTime {
			return false
		}
		t.Enabled[pid] = st
		t.logf("Steam requests audio capture of process %s", st.Label())
		return true
	}
	st, exists := t.Enabled[pid]
	if !exists {
		return false
	}
	delete(t.Enabled, pid)
	t.logf("Steam stops audio capture of process %s", st.Label())
	return true
}

// prune drops PIDs that exited or were recycled (different start time).
func (t *SteamLogTailer) prune() bool {
	changed := false
	for pid, st := range t.Enabled {
		cur, ok := t.fs.Stat(pid)
		if !ok || cur.StartTime != st.StartTime {
			delete(t.Enabled, pid)
			Infof("Process %s exited (or PID recycled): removed from Steam capture list", st.Label())
			changed = true
		}
	}
	return changed
}
