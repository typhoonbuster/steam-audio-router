package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Level is a log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = map[Level]string{LevelDebug: "DEBUG", LevelInfo: "INFO", LevelWarn: "WARN", LevelError: "ERROR"}

// journald priorities (syslog levels) used with the <N> prefix convention.
var journalPrio = map[Level]int{LevelDebug: 7, LevelInfo: 6, LevelWarn: 4, LevelError: 3}

func parseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("unknown log level %q (debug, info, warning, error)", s)
}

// Logger is a tiny leveled logger. When stdout is the journald stream it
// prefixes each line with <N> so journalctl -p filters by priority; otherwise
// it prints a timestamp for human readers.
type Logger struct {
	mu      sync.Mutex
	w       io.Writer
	level   Level
	journal bool
}

var logger = &Logger{w: os.Stdout, level: LevelInfo, journal: stdoutIsJournal()}

func (l *Logger) SetLevel(lv Level) { l.mu.Lock(); l.level = lv; l.mu.Unlock() }

func (l *Logger) logf(lv Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lv < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if l.journal {
		fmt.Fprintf(l.w, "<%d>%s\n", journalPrio[lv], msg)
	} else {
		fmt.Fprintf(l.w, "%s %-5s %s\n", time.Now().Format("15:04:05"), levelNames[lv], msg)
	}
}

func Debugf(format string, args ...any) { logger.logf(LevelDebug, format, args...) }
func Infof(format string, args ...any)  { logger.logf(LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { logger.logf(LevelWarn, format, args...) }
func Errorf(format string, args ...any) { logger.logf(LevelError, format, args...) }

// stdoutIsJournal reports whether stdout is the journald stream: systemd sets
// JOURNAL_STREAM to "<dev>:<inode>" of the socket it connected to stdout.
func stdoutIsJournal() bool {
	parts := strings.SplitN(os.Getenv("JOURNAL_STREAM"), ":", 2)
	if len(parts) != 2 {
		return false
	}
	dev, err1 := strconv.ParseUint(parts[0], 10, 64)
	ino, err2 := strconv.ParseUint(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == dev && st.Ino == ino
}
