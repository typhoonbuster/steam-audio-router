package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config holds every tunable. See config.toml for the documented defaults.
type Config struct {
	TargetNodePatterns         []string
	SteamAncestorComms         []string
	SelectionMode              string // "steam-log" or "all-steam-children"
	SteamLogPath               string
	FallbackWhenLogUnavailable bool
	IncludeSteamItself         bool
	LogLevel                   string
	DryRun                     bool
	PollInterval               float64 // seconds
	ReconcileInterval          float64 // seconds
	LinkVerifyDelay            float64 // seconds
	MaxRouteAttempts           int
	UnrouteOnDisable           bool
	UnrouteOnExit              bool
	SteamLogTailBytes          int64
	PwRestartBackoff           []float64 // seconds
}

const (
	ModeSteamLog    = "steam-log"
	ModeAllChildren = "all-steam-children"
)

func defaultConfig() Config {
	return Config{
		TargetNodePatterns: []string{"SteamProcessRecording", "SteamRecordProcessing"},
		SteamAncestorComms: []string{
			"steam", "steamwebhelper", "reaper", "steam-runtime-launcher-service",
			"steam-runtime-supervisor", "steam-runtime-launch-client", "srt-client-container",
			"gameoverlayui",
		},
		SelectionMode:              ModeSteamLog,
		SteamLogPath:               "~/.steam/steam/logs/streaming_log.txt",
		FallbackWhenLogUnavailable: true,
		IncludeSteamItself:         false,
		LogLevel:                   "info",
		DryRun:                     false,
		PollInterval:               1.0,
		ReconcileInterval:          10.0,
		LinkVerifyDelay:            2.0,
		MaxRouteAttempts:           3,
		UnrouteOnDisable:           true,
		UnrouteOnExit:              false,
		SteamLogTailBytes:          512 * 1024,
		PwRestartBackoff:           []float64{1, 2, 5, 10},
	}
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// loadConfig reads a TOML file (a simple subset: key = value, strings,
// booleans, numbers, arrays, # comments) over the defaults. A missing file
// is not an error: defaults are used and a warning is logged.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		path = expandHome(path)
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			values, perr := parseTOML(string(data))
			if perr != nil {
				return cfg, fmt.Errorf("%s: %w", path, perr)
			}
			if err := applyValues(&cfg, values); err != nil {
				return cfg, fmt.Errorf("%s: %w", path, err)
			}
		case os.IsNotExist(err):
			Warnf("Config file %s not found: using defaults", path)
		default:
			return cfg, fmt.Errorf("cannot read %s: %w", path, err)
		}
	}
	return cfg, validate(&cfg)
}

func applyValues(cfg *Config, values map[string]any) error {
	var unknown []string
	for key, v := range values {
		var err error
		switch key {
		case "target_node_patterns":
			cfg.TargetNodePatterns, err = asStringList(v)
		case "steam_ancestor_comms":
			cfg.SteamAncestorComms, err = asStringList(v)
		case "selection_mode":
			cfg.SelectionMode, err = asString(v)
		case "steam_log_path":
			cfg.SteamLogPath, err = asString(v)
		case "fallback_when_log_unavailable":
			cfg.FallbackWhenLogUnavailable, err = asBool(v)
		case "include_steam_itself":
			cfg.IncludeSteamItself, err = asBool(v)
		case "log_level":
			cfg.LogLevel, err = asString(v)
		case "dry_run":
			cfg.DryRun, err = asBool(v)
		case "poll_interval":
			cfg.PollInterval, err = asFloat(v)
		case "reconcile_interval":
			cfg.ReconcileInterval, err = asFloat(v)
		case "link_verify_delay":
			cfg.LinkVerifyDelay, err = asFloat(v)
		case "max_route_attempts":
			var f float64
			f, err = asFloat(v)
			cfg.MaxRouteAttempts = int(f)
		case "unroute_on_disable":
			cfg.UnrouteOnDisable, err = asBool(v)
		case "unroute_on_exit":
			cfg.UnrouteOnExit, err = asBool(v)
		case "steam_log_tail_bytes":
			var f float64
			f, err = asFloat(v)
			cfg.SteamLogTailBytes = int64(f)
		case "pw_restart_backoff":
			cfg.PwRestartBackoff, err = asFloatList(v)
		default:
			unknown = append(unknown, key)
			continue
		}
		if err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		Warnf("Unknown config keys ignored: %s", strings.Join(unknown, ", "))
	}
	return nil
}

func validate(cfg *Config) error {
	if len(cfg.TargetNodePatterns) == 0 {
		return fmt.Errorf("target_node_patterns must not be empty")
	}
	if len(cfg.SteamAncestorComms) == 0 {
		return fmt.Errorf("steam_ancestor_comms must not be empty")
	}
	if cfg.SelectionMode != ModeSteamLog && cfg.SelectionMode != ModeAllChildren {
		return fmt.Errorf("selection_mode must be %q or %q", ModeSteamLog, ModeAllChildren)
	}
	for name, v := range map[string]float64{"poll_interval": cfg.PollInterval,
		"reconcile_interval": cfg.ReconcileInterval, "link_verify_delay": cfg.LinkVerifyDelay} {
		if v <= 0 {
			return fmt.Errorf("%s must be > 0", name)
		}
	}
	if cfg.PollInterval < 0.25 {
		cfg.PollInterval = 0.25 // never poll aggressively
	}
	if cfg.MaxRouteAttempts < 1 {
		cfg.MaxRouteAttempts = 1
	}
	if cfg.SteamLogTailBytes < 4096 {
		cfg.SteamLogTailBytes = 4096
	}
	if len(cfg.PwRestartBackoff) == 0 {
		cfg.PwRestartBackoff = defaultConfig().PwRestartBackoff
	}
	for i, b := range cfg.PwRestartBackoff {
		if b < 0.5 {
			cfg.PwRestartBackoff[i] = 0.5
		}
	}
	cfg.SteamLogPath = expandHome(cfg.SteamLogPath)
	if _, err := parseLevel(cfg.LogLevel); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Minimal TOML subset parser (no third-party dependency).
// Supported: `key = value`, strings in double quotes with \" \\ \n \t escapes,
// true/false, integers and floats, single- or multi-line arrays of those, and
// # comments. Tables ([section]) are rejected on purpose.
// ---------------------------------------------------------------------------

func parseTOML(src string) (map[string]any, error) {
	out := map[string]any{}
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		lineNo := i + 1
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return nil, fmt.Errorf("line %d: tables are not supported", lineNo)
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		key = strings.Trim(key, `"'`)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		raw := strings.TrimSpace(line[eq+1:])
		// Multi-line array: keep appending lines until brackets balance.
		for strings.HasPrefix(raw, "[") && bracketDepth(raw) > 0 && i+1 < len(lines) {
			i++
			raw += " " + strings.TrimSpace(stripComment(lines[i]))
		}
		val, err := parseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		out[key] = val
	}
	return out, nil
}

// stripComment removes a trailing # comment that is not inside a string.
func stripComment(s string) string {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inStr {
				i++
			}
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return s[:i]
			}
		}
	}
	return s
}

func bracketDepth(s string) int {
	depth, inStr := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inStr {
				i++
			}
		case '"':
			inStr = !inStr
		case '[':
			if !inStr {
				depth++
			}
		case ']':
			if !inStr {
				depth--
			}
		}
	}
	return depth
}

func parseValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return nil, fmt.Errorf("missing value")
	case raw == "true":
		return true, nil
	case raw == "false":
		return false, nil
	case strings.HasPrefix(raw, `"`):
		s, rest, err := parseString(raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(rest) != "" {
			return nil, fmt.Errorf("unexpected text after string: %q", rest)
		}
		return s, nil
	case strings.HasPrefix(raw, "["):
		if !strings.HasSuffix(raw, "]") {
			return nil, fmt.Errorf("unterminated array")
		}
		return parseArray(raw[1 : len(raw)-1])
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(raw, "_", ""), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid value %q", raw)
	}
	return f, nil
}

func parseString(raw string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case '\\':
			if i+1 >= len(raw) {
				return "", "", fmt.Errorf("bad escape")
			}
			i++
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"', '\\':
				b.WriteByte(raw[i])
			default:
				return "", "", fmt.Errorf("unsupported escape \\%c", raw[i])
			}
		case '"':
			return b.String(), raw[i+1:], nil
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("unterminated string")
}

func parseArray(body string) ([]any, error) {
	var items []any
	body = strings.TrimSpace(body)
	for body != "" {
		var item any
		var err error
		if strings.HasPrefix(body, `"`) {
			var s, rest string
			s, rest, err = parseString(body)
			if err != nil {
				return nil, err
			}
			item, body = s, strings.TrimSpace(rest)
		} else {
			end := strings.Index(body, ",")
			if end < 0 {
				end = len(body)
			}
			item, err = parseValue(body[:end])
			if err != nil {
				return nil, err
			}
			body = strings.TrimSpace(body[end:])
		}
		items = append(items, item)
		if strings.HasPrefix(body, ",") {
			body = strings.TrimSpace(body[1:])
		} else if body != "" {
			return nil, fmt.Errorf("expected ',' in array near %q", body)
		}
	}
	return items, nil
}

func asString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected a string")
	}
	return s, nil
}

func asBool(v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("expected true or false")
	}
	return b, nil
}

func asFloat(v any) (float64, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("expected a number")
	}
	return f, nil
}

func asStringList(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected an array of strings")
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("expected an array of strings")
		}
		out = append(out, s)
	}
	return out, nil
}

func asFloatList(v any) ([]float64, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected an array of numbers")
	}
	out := make([]float64, 0, len(list))
	for _, it := range list {
		f, ok := it.(float64)
		if !ok {
			return nil, fmt.Errorf("expected an array of numbers")
		}
		out = append(out, f)
	}
	return out, nil
}
