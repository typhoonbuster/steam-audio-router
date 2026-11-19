// steam-audio-router routes the audio of the Steam game being streamed with
// Remote Play Together to Steam's capture node ("SteamProcessRecording") under
// PipeWire/WirePlumber. Single static binary; the only runtime requirements
// are pw-dump and pw-link, which ship with PipeWire itself.
//
// How it works:
//  1. `pw-dump -m` delivers PipeWire events as JSON (no polling of the graph).
//  2. For every Stream/Output/Audio node the owner PID is taken from the
//     Client object's pipewire.sec.pid (kernel-verified via SO_PEERCRED),
//     falling back to application.process.id for pipewire-pulse clients.
//  3. The process tree is walked through /proc/<pid>/stat (PPID) up to PID 1;
//     the stream "comes from Steam" if an ancestor's comm is in the
//     configurable steam_ancestor_comms list.
//  4. To route only the game of the active Remote Play Together session, Steam's own
//     logs/streaming_log.txt is followed: "Telling process <pid> to change
//     audio capture state: enabled|disabled". A stream is routed when its
//     PID is that process or one of its descendants (and point 3 holds).
//  5. Routing ADDS links (pw-link) from the game's output ports to the
//     target's playback ports, channel by channel. The game's existing links
//     to the user's sink are never touched, nothing is moved: the host keeps
//     hearing the game exactly as before, Steam's capture node simply gets a
//     copy. Links die with either node, so cleanup is automatic.
//
// Every PID is paired with its start time (field 22 of /proc/<pid>/stat) to
// detect recycled PIDs.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// version is set at build time: -ldflags "-X main.version=1.2.3"
var version = "dev"

const usageText = `steam-audio-router %s

Usage:
  steam-audio-router [run]  [--config FILE] [--dry-run] [--log-level L] [--log-file F]
  steam-audio-router once   [--config FILE] [--dry-run] [--log-level L]
  steam-audio-router install   [--enable] [--systemd | --autostart | --no-service]
  steam-audio-router uninstall
  steam-audio-router print-config [--config FILE]
  steam-audio-router version

Commands:
  run           Follow PipeWire and route streams (default).
  once          Evaluate the streams present now, act (unless --dry-run),
                print a report and exit. Handy to test a running session.
  install       Copy this binary to ~/.local/bin, write the config and a
                systemd --user unit (or an XDG autostart entry when there is
                no systemd). --enable also starts it.
  uninstall     Stop the service and remove binary, unit and autostart entry.
  print-config  Show the effective configuration.

Common flags:
  --config FILE   Configuration file (default ~/.config/steam-audio-router/config.toml)
  --dry-run       Log every decision but never run pw-link
  --log-level L   debug, info, warning or error (overrides the config)
  --log-file F    Append logs to F instead of stdout (used without systemd)
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	defaultCfg := "~/.config/steam-audio-router/config.toml"

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println("steam-audio-router", version)
		return 0
	case "help", "--help", "-h":
		fmt.Printf(usageText, version)
		return 0
	case "uninstall":
		return runUninstall()
	case "install":
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		enable := fs.Bool("enable", false, "enable and start the service after installing")
		useSystemd := fs.Bool("systemd", false, "force the systemd --user unit")
		useAutostart := fs.Bool("autostart", false, "force the XDG autostart entry")
		noService := fs.Bool("no-service", false, "install files only")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		opts := installOptions{Mode: "auto", Enable: *enable}
		switch {
		case *useSystemd:
			opts.Mode = "systemd"
		case *useAutostart:
			opts.Mode = "autostart"
		case *noService:
			opts.Mode = "none"
		}
		return runInstall(opts)
	case "run", "once", "print-config":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		cfgPath := fs.String("config", defaultCfg, "configuration file")
		dryRun := fs.Bool("dry-run", false, "never run pw-link")
		fs.BoolVar(dryRun, "n", false, "alias of --dry-run")
		logLevel := fs.String("log-level", "", "debug, info, warning, error")
		logFile := fs.String("log-file", "", "append logs to this file")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if *logFile != "" {
			if err := openLogFile(*logFile); err != nil {
				Errorf("Cannot open log file: %v", err)
				return 1
			}
		}
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			Errorf("Invalid configuration: %v", err)
			return 1
		}
		level := cfg.LogLevel
		if *logLevel != "" {
			level = *logLevel
		}
		lv, err := parseLevel(level)
		if err != nil {
			Errorf("%v", err)
			return 1
		}
		logger.SetLevel(lv)
		if cmd == "print-config" {
			fmt.Print(formatConfig(cfg))
			return 0
		}
		dry := *dryRun || cfg.DryRun
		dryText := ""
		if dry {
			dryText = ", DRY-RUN"
		}
		Infof("steam-audio-router %s starting (mode %s%s, target %v)", version, cfg.SelectionMode, dryText, cfg.TargetNodePatterns)
		switch exe, running, rt3 := procfs.detectSteamClient(); {
		case rt3:
			Infof("Steam client detected: %s (SteamRT3)", exe)
		case running:
			Warnf("Steam is running but is not the SteamRT3 client (%s): only SteamRT3 is supported, nothing will be linked", exe)
		default:
			Infof("Steam is not running yet; waiting (only the SteamRT3 client is supported)")
		}
		tailer := newSteamLogTailer(cfg.SteamLogPath, cfg.SteamLogTailBytes, procfs)
		router := newRouter(cfg, dry, procfs, tailer)
		return runService(router, cmd == "once")
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
	fmt.Fprintf(os.Stderr, usageText, version)
	return 2
}

// openLogFile redirects the logger to a file, rotating it once past 5 MiB.
func openLogFile(path string) error {
	path = expandHome(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	logger.mu.Lock()
	logger.w = f
	logger.journal = false
	logger.mu.Unlock()
	return nil
}

func formatConfig(c Config) string {
	q := func(list []string) string {
		parts := make([]string, len(list))
		for i, s := range list {
			parts[i] = fmt.Sprintf("%q", s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	f := func(list []float64) string {
		parts := make([]string, len(list))
		for i, v := range list {
			parts[i] = fmt.Sprintf("%g", v)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf(`target_node_patterns = %s
steam_ancestor_comms = %s
selection_mode = %q
steam_log_path = %q
fallback_when_log_unavailable = %v
include_steam_itself = %v
log_level = %q
dry_run = %v
poll_interval = %g
reconcile_interval = %g
link_verify_delay = %g
max_route_attempts = %d
unroute_on_disable = %v
unroute_on_exit = %v
steam_log_tail_bytes = %d
pw_restart_backoff = %s
`, q(c.TargetNodePatterns), q(c.SteamAncestorComms), c.SelectionMode, c.SteamLogPath, c.FallbackWhenLogUnavailable,
		c.IncludeSteamItself, c.LogLevel, c.DryRun, c.PollInterval, c.ReconcileInterval, c.LinkVerifyDelay,
		c.MaxRouteAttempts, c.UnrouteOnDisable, c.UnrouteOnExit, c.SteamLogTailBytes, f(c.PwRestartBackoff))
}
