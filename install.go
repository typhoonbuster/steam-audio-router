package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

//go:embed config.toml
var embeddedConfig string

//go:embed steam-audio-router.service
var embeddedUnit string

//go:embed steam-audio-router.desktop
var embeddedDesktop string

const (
	unitName    = "steam-audio-router.service"
	desktopName = "steam-audio-router.desktop"
	binName     = "steam-audio-router"
)

// installPaths centralises every location touched by install/uninstall.
type installPaths struct {
	Home, Bin, ConfigDir, Config, ConfigExample, Unit, Desktop, LogFile string
}

func defaultPaths() (installPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, err
	}
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	if cfgHome == "" {
		cfgHome = filepath.Join(home, ".config")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	p := installPaths{
		Home:      home,
		Bin:       filepath.Join(home, ".local", "bin", binName),
		ConfigDir: filepath.Join(cfgHome, binName),
		Unit:      filepath.Join(cfgHome, "systemd", "user", unitName),
		Desktop:   filepath.Join(cfgHome, "autostart", desktopName),
		LogFile:   filepath.Join(stateHome, binName, binName+".log"),
	}
	p.Config = filepath.Join(p.ConfigDir, "config.toml")
	p.ConfigExample = filepath.Join(p.ConfigDir, "config.example.toml")
	return p, nil
}

type installOptions struct {
	Mode   string // "auto", "systemd", "autostart", "none"
	Enable bool
}

func step(format string, args ...any) { fmt.Printf("==> "+format+"\n", args...) }

// systemdUserAvailable reports whether `systemctl --user` can talk to a user manager.
func systemdUserAvailable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// installBinary copies the running executable to ~/.local/bin unless it
// already runs from there.
func installBinary(p installPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if exe == p.Bin {
		step("Binary already in place: %s", p.Bin)
		return nil
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(p.Bin), 0o755); err != nil {
		return err
	}
	tmp := p.Bin + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.Bin); err != nil {
		return err
	}
	step("Binary installed: %s", p.Bin)
	return nil
}

func runInstall(opts installOptions) int {
	p, err := defaultPaths()
	if err != nil {
		Errorf("%v", err)
		return 1
	}
	for _, tool := range []string{"pw-dump", "pw-link"} {
		if _, err := exec.LookPath(tool); err != nil {
			Warnf("%s not found in PATH: it ships with the pipewire package and is required at runtime", tool)
		}
	}
	if err := installBinary(p); err != nil {
		Errorf("Cannot install binary: %v", err)
		return 1
	}
	if err := writeFile(p.ConfigExample, []byte(embeddedConfig), 0o644); err != nil {
		Errorf("Cannot write %s: %v", p.ConfigExample, err)
		return 1
	}
	if _, err := os.Stat(p.Config); os.IsNotExist(err) {
		if err := writeFile(p.Config, []byte(embeddedConfig), 0o644); err != nil {
			Errorf("Cannot write %s: %v", p.Config, err)
			return 1
		}
		step("Config written: %s", p.Config)
	} else {
		step("Existing config kept: %s (new example: %s)", p.Config, p.ConfigExample)
	}
	if _, err := loadConfig(p.Config); err != nil {
		Errorf("Config is invalid: %v", err)
		return 1
	}

	mode := opts.Mode
	if mode == "auto" {
		if systemdUserAvailable() {
			mode = "systemd"
		} else {
			mode = "autostart"
		}
	}
	switch mode {
	case "systemd":
		return installSystemd(p, opts.Enable)
	case "autostart":
		return installAutostart(p, opts.Enable)
	case "none":
		step("No service installed (--no-service). Run it yourself with: %s run", p.Bin)
		return 0
	}
	Errorf("unknown mode %q", mode)
	return 1
}

func installSystemd(p installPaths, enable bool) int {
	if err := writeFile(p.Unit, []byte(embeddedUnit), 0o644); err != nil {
		Errorf("Cannot write %s: %v", p.Unit, err)
		return 1
	}
	step("systemd user unit written: %s", p.Unit)
	if err := os.Remove(p.Desktop); err == nil {
		step("Removed autostart entry %s (systemd mode takes over)", p.Desktop)
	}
	if err := systemctl("daemon-reload"); err != nil {
		Errorf("systemctl --user daemon-reload failed: %v", err)
		return 1
	}
	if !enable {
		fmt.Printf("\nInstalled. Test without changing anything (with a game streaming):\n    %s once --dry-run\n"+
			"Enable the service:\n    systemctl --user enable --now %s\nFollow the logs:\n    journalctl --user -u %s -f\n",
			binName, unitName, binName)
		return 0
	}
	step("Enabling and starting %s", unitName)
	if err := systemctl("enable", "--now", unitName); err != nil {
		Errorf("enable failed: %v", err)
		return 1
	}
	_ = systemctl("--no-pager", "status", unitName)
	fmt.Printf("\nFollow the logs with: journalctl --user -u %s -f\n", binName)
	return 0
}

func installAutostart(p installPaths, enable bool) int {
	execLine := fmt.Sprintf("%s run --config %s --log-file %s", p.Bin, p.Config, p.LogFile)
	content := strings.Replace(embeddedDesktop, "__EXEC__", execLine, 1)
	if err := writeFile(p.Desktop, []byte(content), 0o644); err != nil {
		Errorf("Cannot write %s: %v", p.Desktop, err)
		return 1
	}
	step("XDG autostart entry written: %s (starts with your desktop session)", p.Desktop)
	step("Logs will go to %s", p.LogFile)
	if systemdUserAvailable() {
		if _, err := os.Stat(p.Unit); err == nil {
			_ = systemctl("disable", "--now", unitName)
			_ = os.Remove(p.Unit)
			_ = systemctl("daemon-reload")
			step("Removed systemd unit (autostart mode takes over)")
		}
	}
	if !enable {
		fmt.Printf("\nInstalled. Test without changing anything (with a game streaming):\n    %s once --dry-run\n"+
			"Start it now (it also starts at next login):\n    %s install --autostart --enable\n", binName, binName)
		return 0
	}
	killRunning(p.Bin)
	if err := os.MkdirAll(filepath.Dir(p.LogFile), 0o755); err != nil {
		Errorf("Cannot create %s: %v", filepath.Dir(p.LogFile), err)
		return 1
	}
	cmd := exec.Command(p.Bin, "run", "--config", p.Config, "--log-file", p.LogFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		Errorf("Cannot start %s: %v", p.Bin, err)
		return 1
	}
	_ = cmd.Process.Release()
	step("Started in the background (pid %d). Logs: tail -f %s", cmd.Process.Pid, p.LogFile)
	return 0
}

// killRunning sends SIGTERM to every process of ours whose executable is bin.
func killRunning(bin string) {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		if exe == bin {
			if proc, err := os.FindProcess(pid); err == nil && proc.Signal(syscall.SIGTERM) == nil {
				step("Stopped running instance (pid %d)", pid)
			}
		}
	}
}

func runUninstall() int {
	p, err := defaultPaths()
	if err != nil {
		Errorf("%v", err)
		return 1
	}
	if _, err := os.Stat(p.Unit); err == nil && systemdUserAvailable() {
		step("Stopping and disabling %s", unitName)
		_ = systemctl("disable", "--now", unitName)
	}
	for _, f := range []string{p.Unit, p.Desktop} {
		if err := os.Remove(f); err == nil {
			step("Removed %s", f)
		} else if !errors.Is(err, os.ErrNotExist) {
			Warnf("Cannot remove %s: %v", f, err)
		}
	}
	if systemdUserAvailable() {
		_ = systemctl("daemon-reload")
	}
	killRunning(p.Bin)
	if err := os.Remove(p.Bin); err == nil {
		step("Removed %s", p.Bin)
	} else if !errors.Is(err, os.ErrNotExist) {
		Warnf("Cannot remove %s: %v", p.Bin, err)
	}
	step("Done. Configuration kept in %s (delete it yourself if unwanted).", p.ConfigDir)
	return 0
}
