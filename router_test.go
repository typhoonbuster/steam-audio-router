package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { logger.SetLevel(LevelError + 1) } // silence logs in tests

type fakeProc struct {
	comm  string
	ppid  int
	start int64
	exe   string // optional /proc/<pid>/exe target
}

const rt3Exe = "/home/u/.local/share/Steam/steamrt64/steam"
const legacyExe = "/home/u/.local/share/Steam/ubuntu12_32/steam"

// Realistic tree copied from the test machine:
//
//	1 systemd -> 6509 steam -> 6560 srt-client-cont -> 6571 x86_64-linux-gn
//	-> 17809 steam-runtime-s -> 17810 reaper -> 17812 srt-bwrap -> 17875 pv-adverb
//	-> 17998 Spelunky.exe ; 6665 steam (client inside the container) ; 6067 Discord
var baseProcs = map[int]fakeProc{
	1:     {"systemd", 0, 10, ""},
	6509:  {"steam", 1, 100, rt3Exe},
	6560:  {"srt-client-cont", 6509, 101, ""},
	6571:  {"x86_64-linux-gn", 6560, 102, ""},
	6665:  {"steam", 6560, 103, rt3Exe},
	17809: {"steam-runtime-s", 6571, 200, ""},
	17810: {"reaper", 17809, 201, ""},
	17812: {"srt-bwrap", 17810, 202, ""},
	17875: {"pv-adverb", 17812, 203, ""},
	17998: {"Spelunky.exe", 17875, 204, ""},
	18100: {"audio helper", 17998, 205, ""}, // child of the game, comm with a space
	6067:  {"Discord", 1, 50, ""},
	7000:  {"wine-preloader", 1, 60, ""}, // Wine outside Steam
	7001:  {"OtherGame.exe", 7000, 61, ""},
	8000:  {"weird (name)", 1, 70, ""}, // comm with parentheses
	// Legacy (ubuntu12_32) Steam client with its own game
	5000: {"steam", 1, 300, legacyExe},
	5001: {"reaper", 5000, 301, ""},
	5002: {"OldGame.exe", 5001, 302, ""},
}

func makeProc(t *testing.T, root string, procs map[int]fakeProc) {
	t.Helper()
	for pid, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fields := []string{"S", fmt.Sprint(p.ppid)}
		for i := 0; i < 17; i++ {
			fields = append(fields, "0")
		}
		fields = append(fields, fmt.Sprint(p.start)) // index 19 -> field 22
		for i := 0; i < 30; i++ {
			fields = append(fields, "0")
		}
		line := fmt.Sprintf("%d (%s) %s\n", pid, p.comm, strings.Join(fields, " "))
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, "exe"))
		if p.exe != "" {
			if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 0 0 0 0\nbtime 1000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	t       *testing.T
	fs      ProcFS
	logPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proc")
	makeProc(t, root, baseProcs)
	return &fixture{t: t, fs: ProcFS{Root: root}, logPath: filepath.Join(tmp, "streaming_log.txt")}
}

func (f *fixture) addProc(pid int, p fakeProc) { makeProc(f.t, f.fs.Root, map[int]fakeProc{pid: p}) }

func (f *fixture) removeProc(pid int) { os.RemoveAll(filepath.Join(f.fs.Root, fmt.Sprint(pid))) }

func ts(offset time.Duration) string {
	return time.Now().Add(offset).Format("2006-01-02 15:04:05")
}

func captureLine(pid int, state string) string {
	return fmt.Sprintf("[%s][123.456] Telling process %d to change audio capture state: %s", ts(0), pid, state)
}

func (f *fixture) writeLog(lines ...string) {
	if err := os.WriteFile(f.logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appendLog(lines ...string) {
	fh, err := os.OpenFile(f.logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	fh.WriteString(strings.Join(lines, "\n") + "\n")
}

func (f *fixture) tailer() *SteamLogTailer {
	t := newSteamLogTailer(f.logPath, 65536, f.fs)
	t.bootTime = 1000000
	return t
}

func (f *fixture) router(mode string, dryRun bool, mutate func(*Config)) (*Router, *[][]string) {
	cfg := defaultConfig()
	cfg.SelectionMode = mode
	cfg.SteamLogPath = f.logPath
	if mutate != nil {
		mutate(&cfg)
	}
	if err := validate(&cfg); err != nil {
		f.t.Fatal(err)
	}
	r := newRouter(cfg, dryRun, f.fs, f.tailer())
	calls := &[][]string{}
	r.runCmd = func(args []string) bool {
		if r.dryRun {
			f.t.Fatalf("runCmd called in dry-run: %v", args)
		}
		*calls = append(*calls, args)
		return true
	}
	return r, calls
}

// ---- graph builders ------------------------------------------------------

func node(id int, name, mclass string, client int, serial int, extra map[string]any) pwObject {
	props := map[string]any{"node.name": name, "media.class": mclass, "object.serial": float64(serial)}
	if client != 0 {
		props["client.id"] = float64(client)
	}
	for k, v := range extra {
		props[k] = v
	}
	return pwObject{ID: id, Type: typeNode, Info: pwInfo{State: "running", ChangeMask: []string{"props"}, Props: props}}
}

func stream(id int, name string, client int, extra map[string]any) pwObject {
	return node(id, name, "Stream/Output/Audio", client, id*10, extra)
}

func client(id, pid int, extra map[string]any) pwObject {
	props := map[string]any{"pipewire.sec.pid": float64(pid), "object.serial": float64(id * 10)}
	for k, v := range extra {
		props[k] = v
	}
	return pwObject{ID: id, Type: typeClient, Info: pwInfo{Props: props}}
}

func link(id, outNode, outPort, inNode, inPort int) pwObject {
	return pwObject{ID: id, Type: typeLink, Info: pwInfo{OutputNodeID: &outNode, OutputPortID: &outPort,
		InputNodeID: &inNode, InputPortID: &inPort, State: "active"}}
}

// port builds a Port object. monitor marks a sink's monitor output.
func port(id, nodeID int, direction, name, channel string, index int, monitor bool) pwObject {
	props := map[string]any{"node.id": float64(nodeID), "port.name": name, "port.id": float64(index), "object.serial": float64(id)}
	if channel != "" {
		props["audio.channel"] = channel
	}
	if monitor {
		props["port.monitor"] = true
	}
	return pwObject{ID: id, Type: typePort, Info: pwInfo{Direction: direction, Props: props}}
}

// stereoOut adds output_FL/FR ports (ids base, base+1) to a stream node.
func stereoOut(nodeID, base int) []pwObject {
	return []pwObject{port(base, nodeID, "output", "output_FL", "FL", 0, false), port(base+1, nodeID, "output", "output_FR", "FR", 1, false)}
}

// sinkPorts adds playback_FL/FR inputs (ids base, base+1) and monitor outputs (base+2, base+3).
func sinkPorts(nodeID, base int) []pwObject {
	return []pwObject{
		port(base, nodeID, "input", "playback_FL", "FL", 0, false), port(base+1, nodeID, "input", "playback_FR", "FR", 1, false),
		port(base+2, nodeID, "output", "monitor_FL", "FL", 0, true), port(base+3, nodeID, "output", "monitor_FR", "FR", 1, true),
	}
}

func graph(r *Router, withTarget bool, extra ...pwObject) {
	objs := []pwObject{
		client(207, 17998, nil), stream(203, "Spelunky.exe", 207, nil),
		client(111, 6067, nil), stream(109, "WEBRTC VoiceEngine", 111, nil),
		client(321, 18100, nil), stream(320, "helper", 321, nil),
	}
	objs = append(objs, stereoOut(203, 173)...) // Spelunky ports 173, 174
	objs = append(objs, stereoOut(320, 330)...) // helper ports 330, 331
	objs = append(objs, []pwObject{
		client(300, 7001, nil), stream(301, "OtherGame.exe", 300, nil),
		client(196, 2123, map[string]any{"client.api": "pipewire-pulse", "application.process.id": float64(6665)}),
		node(168, "Steam", "Stream/Input/Audio", 196, 1680, map[string]any{"application.process.id": float64(6665)}),
		stream(400, "SteamUI", 196, map[string]any{"application.process.id": float64(6665), "client.api": "pipewire-pulse"}),
		client(181, 2123, nil),
	}...)
	if withTarget {
		objs = append(objs,
			node(180, "SteamProcessRecording", "Audio/Sink", 181, 3688, map[string]any{"node.link-group": "combine-sink-2123-13"}),
			stream(100, "output.SteamProcessRecording_easyeffects_sink", 181, map[string]any{"node.link-group": "combine-sink-2123-13"}))
		objs = append(objs, sinkPorts(180, 95)...) // target inputs 95 (FL), 96 (FR); monitors 97, 98
	}
	objs = append(objs, extra...)
	r.state.Apply(objs)
	r.updateTarget()
}

func decisions(r *Router) map[int]Decision {
	r.tailer.Poll()
	out := map[int]Decision{}
	for _, o := range r.state.OfType(typeNode) {
		if isStream(o) {
			out[o.ID] = r.evaluate(o)
		}
	}
	return out
}

// linkCalls counts `pw-link <out> <in>` calls whose output port belongs to the given port set.
func linkCalls(calls [][]string, outPorts ...string) int {
	n := 0
	for _, c := range calls {
		if len(c) == 3 && c[0] == "pw-link" {
			for _, p := range outPorts {
				if c[1] == p {
					n++
				}
			}
		}
	}
	return n
}

func hasCall(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if fmt.Sprint(c) == fmt.Sprint(want) {
			return true
		}
	}
	return false
}

// ---- /proc ---------------------------------------------------------------

func TestProcStatParsing(t *testing.T) {
	f := newFixture(t)
	st, ok := f.fs.Stat(8000)
	if !ok || st.Comm != "weird (name)" || st.PPID != 1 || st.StartTime != 70 {
		t.Fatalf("bad parse: %+v %v", st, ok)
	}
	if st, _ := f.fs.Stat(18100); st.Comm != "audio helper" {
		t.Fatalf("comm with space: %q", st.Comm)
	}
	if _, ok := f.fs.Stat(424242); ok {
		t.Fatal("missing pid should not parse")
	}
	if _, ok := f.fs.Stat(0); ok {
		t.Fatal("pid 0 should not parse")
	}
}

func TestAncestryAndSteamDetection(t *testing.T) {
	f := newFixture(t)
	names := defaultConfig().SteamAncestorComms
	anc, chain := findSteamAncestor(f.fs, 17998, names, false)
	want := []int{17998, 17875, 17812, 17810, 17809, 6571, 6560, 6509, 1}
	if len(chain) != len(want) {
		t.Fatalf("chain %v", chain)
	}
	for i, pid := range want {
		if chain[i].PID != pid {
			t.Fatalf("chain[%d]=%d want %d", i, chain[i].PID, pid)
		}
	}
	if anc == nil || anc.PID != 17810 {
		t.Fatalf("first Steam ancestor should be reaper, got %v", anc)
	}
	for _, pid := range []int{7001, 6067, 6509} {
		if a, _ := findSteamAncestor(f.fs, pid, names, false); a != nil {
			t.Fatalf("pid %d should have no strict Steam ancestor, got %v", pid, a)
		}
	}
	if a, _ := findSteamAncestor(f.fs, 6509, names, true); a == nil || a.PID != 6509 {
		t.Fatal("includeSelf should match steam itself")
	}
	if !f.fs.IsDescendantOrSelf(18100, 17998) || !f.fs.IsDescendantOrSelf(17998, 17998) || f.fs.IsDescendantOrSelf(7001, 17998) {
		t.Fatal("descendant checks")
	}
	f.addProc(9001, fakeProc{"a", 9002, 1, ""})
	f.addProc(9002, fakeProc{"b", 9001, 1, ""})
	if n := len(f.fs.Ancestry(9001)); n != 2 {
		t.Fatalf("loop guard: %d", n)
	}
}

func TestSteamRT3Detection(t *testing.T) {
	f := newFixture(t)
	if exe, rt3 := steamClientInChain(f.fs, f.fs.Ancestry(17998)); !rt3 || exe != rt3Exe {
		t.Fatalf("SteamRT3 chain: %q %v", exe, rt3)
	}
	if exe, rt3 := steamClientInChain(f.fs, f.fs.Ancestry(5002)); rt3 || exe != legacyExe {
		t.Fatalf("legacy chain: %q %v", exe, rt3)
	}
	if _, rt3 := steamClientInChain(f.fs, f.fs.Ancestry(6067)); rt3 {
		t.Fatal("no steam in chain")
	}
	if exe, running, rt3 := f.fs.detectSteamClient(); !running || !rt3 || exe != rt3Exe {
		t.Fatalf("detect: %q %v %v", exe, running, rt3)
	}
}

func TestLegacyClientIsRefused(t *testing.T) {
	f := newFixture(t)
	r, _ := f.router(ModeAllChildren, true, nil)
	graph(r, true, client(600, 5002, nil), stream(601, "OldGame.exe", 600, nil))
	d := decisions(r)
	if d[601].Route || !strings.Contains(d[601].Reason, "not SteamRT3") || !strings.Contains(d[601].Reason, legacyExe) {
		t.Fatalf("legacy client must be refused: %+v", d[601])
	}
	if !d[203].Route {
		t.Fatalf("SteamRT3 game must still be linked: %s", d[203].Reason)
	}
}

func TestCommTruncation(t *testing.T) {
	if !commMatches("steam-runtime-l", []string{"steam-runtime-launcher-service"}) {
		t.Fatal("truncated match")
	}
	if !commMatches("srt-client-cont", []string{"srt-client-container"}) {
		t.Fatal("truncated match 2")
	}
	if commMatches("steam.sh", []string{"steam"}) {
		t.Fatal("steam.sh must not match steam")
	}
}

func TestNormalizeName(t *testing.T) {
	for _, s := range []string{"SteamProcessRecording", "steam_process_recording", "Steam Process-Recording", "STEAMPROCESSRECORDING.2"} {
		if !strings.Contains(normalizeName(s), "steamprocessrecording") {
			t.Fatalf("normalize %q -> %q", s, normalizeName(s))
		}
	}
}

// ---- Steam log -------------------------------------------------------------

func TestTailerCycle(t *testing.T) {
	f := newFixture(t)
	f.writeLog("noise", captureLine(17998, "enabled"))
	tl := f.tailer()
	if !tl.Poll() || len(tl.EnabledPIDs()) != 1 || tl.EnabledPIDs()[0] != 17998 {
		t.Fatalf("enabled: %v", tl.EnabledPIDs())
	}
	if tl.Poll() {
		t.Fatal("no change expected")
	}
	f.appendLog(captureLine(17998, "disabled"))
	if !tl.Poll() || len(tl.EnabledPIDs()) != 0 {
		t.Fatal("disabled")
	}
	f.appendLog(fmt.Sprintf("[%s][1.0] Recording process audio: 17998", ts(0)))
	if !tl.Poll() || len(tl.EnabledPIDs()) != 1 {
		t.Fatal("Recording process audio should enable")
	}
}

func TestTailerDeadOrRecycledPID(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(424242, "enabled"),
		"[2006-01-01 00:00:00][1.0] Telling process 17998 to change audio capture state: enabled")
	tl := f.tailer()
	tl.bootTime = float64(time.Now().Unix()) // 17998 "started" now, after the 2006 line
	tl.Poll()
	if len(tl.EnabledPIDs()) != 0 {
		t.Fatalf("dead/recycled pids must be ignored: %v", tl.EnabledPIDs())
	}
}

func TestTailerMissingThenRotation(t *testing.T) {
	f := newFixture(t)
	tl := f.tailer()
	tl.Poll()
	if tl.Available != AvailNo {
		t.Fatal("missing file -> AvailNo")
	}
	f.writeLog(captureLine(17998, "enabled"))
	tl.Poll()
	if tl.Available != AvailYes || len(tl.EnabledPIDs()) != 1 {
		t.Fatal("file appeared")
	}
	os.Rename(f.logPath, f.logPath+".previous")
	f.writeLog("fresh start")
	if !tl.Poll() || len(tl.EnabledPIDs()) != 0 {
		t.Fatal("rotation must reset state")
	}
}

func TestTailerTailOnlyOnFirstOpen(t *testing.T) {
	f := newFixture(t)
	var lines []string
	for i := 0; i < 5000; i++ {
		lines = append(lines, fmt.Sprintf("padding %05d", i))
	}
	lines = append(lines, captureLine(17998, "enabled"))
	f.writeLog(lines...)
	tl := newSteamLogTailer(f.logPath, 4096, f.fs)
	tl.bootTime = 1000000
	tl.Poll()
	if len(tl.EnabledPIDs()) != 1 {
		t.Fatal("tail replay should find the last line")
	}
}

func TestTailerPruneWhenProcessDies(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	tl := f.tailer()
	tl.Poll()
	f.removeProc(17998)
	if !tl.Poll() || len(tl.EnabledPIDs()) != 0 {
		t.Fatal("dead process must be pruned")
	}
}

// ---- pw-dump decoding ------------------------------------------------------

func TestDecodeBatchFormats(t *testing.T) {
	raw := `[
	  {"id": 203, "type": "PipeWire:Interface:Node", "info": {"state": "running", "change-mask": ["props"], "props": {"node.name": "Spelunky.exe", "client.id": 207, "media.class": "Stream/Output/Audio"}}},
	  {"id": 138, "type": "PipeWire:Interface:Link", "info": {"output-node-id": 117, "output-port-id": 120, "input-node-id": 104, "input-port-id": 103, "state": "active"}},
	  {"id": 80, "type": "PipeWire:Interface:Port", "info": {"direction": "output", "props": {"audio.channel": "FL", "node.id": 76, "port.id": 0, "port.monitor": true, "port.name": "monitor_FL"}}},
	  {"id": 78, "type": "PipeWire:Interface:Port", "info": {"direction": "input", "props": {"audio.channel": "FL", "node.id": 76, "port.id": 0, "port.name": "playback_FL"}}},
	  {"id": 39, "type": "PipeWire:Interface:Metadata", "props": {"metadata.name": "default"}, "metadata": [{"subject": 0, "key": "default.audio.sink", "type": "Spa:String:JSON", "value": {"name": "x"}}]},
	  {"id": 207, "info": null}
	]`
	var batch []pwRaw
	if err := json.Unmarshal([]byte(raw), &batch); err != nil {
		t.Fatal(err)
	}
	objs := decodeBatch(batch)
	if len(objs) != 6 || objs[5].ID != 207 || !objs[5].Removed {
		t.Fatalf("removal: %+v", objs[5])
	}
	if cid, _ := propInt(objs[0].props(), "client.id"); cid != 207 {
		t.Fatal("client.id")
	}
	st := newPwState()
	st.Apply(objs[:5])
	links := st.LinksBetween(117, 104)
	if len(links) != 1 || links[0].OutPort != 120 || links[0].InPort != 103 || links[0].ID != 138 {
		t.Fatalf("links: %+v", links)
	}
	ins := st.Ports(76, "input")
	if len(ins) != 1 || ins[0].ID != 78 || ins[0].Channel != "FL" || ins[0].Name != "playback_FL" {
		t.Fatalf("input ports: %+v", ins)
	}
	if outs := st.Ports(76, "output"); len(outs) != 0 {
		t.Fatalf("monitor ports must be excluded: %+v", outs)
	}
	a, u, rm := st.Apply(objs[5:])
	if len(a) != 0 || len(u) != 0 || len(rm) != 0 { // 207 was never added
		t.Fatal("removing unknown id")
	}
	st.Apply([]pwObject{client(207, 1, nil)})
	if _, _, rm := st.Apply(objs[5:]); len(rm) != 1 {
		t.Fatal("removal of known id")
	}
}

func TestPlanLinks(t *testing.T) {
	fl := portInfo{ID: 1, Name: "output_FL", Channel: "FL", Index: 0}
	fr := portInfo{ID: 2, Name: "output_FR", Channel: "FR", Index: 1}
	mono := portInfo{ID: 3, Name: "output_MONO", Channel: "MONO", Index: 0}
	dFL := portInfo{ID: 10, Name: "playback_FL", Channel: "FL", Index: 0}
	dFR := portInfo{ID: 11, Name: "playback_FR", Channel: "FR", Index: 1}
	// stereo -> stereo by channel, whatever the order
	pairs := planLinks([]portInfo{fr, fl}, []portInfo{dFL, dFR})
	if len(pairs) != 2 || pairs[0][0].ID != 1 || pairs[0][1].ID != 10 || pairs[1][0].ID != 2 || pairs[1][1].ID != 11 {
		t.Fatalf("stereo: %+v", pairs)
	}
	// mono fans out to both
	pairs = planLinks([]portInfo{mono}, []portInfo{dFL, dFR})
	if len(pairs) != 2 || pairs[0][0].ID != 3 || pairs[1][0].ID != 3 {
		t.Fatalf("mono: %+v", pairs)
	}
	// unnamed channels fall back to positions
	pairs = planLinks([]portInfo{{ID: 5, Index: 0}, {ID: 6, Index: 1}}, []portInfo{{ID: 20, Index: 0}, {ID: 21, Index: 1}})
	if len(pairs) != 2 || pairs[1][0].ID != 6 || pairs[1][1].ID != 21 {
		t.Fatalf("positional: %+v", pairs)
	}
	// 5.1 source into stereo target: extra channels unlinked
	src := []portInfo{fl, fr, {ID: 7, Channel: "FC", Index: 2}, {ID: 8, Channel: "LFE", Index: 3}}
	if pairs = planLinks(src, []portInfo{dFL, dFR}); len(pairs) != 2 {
		t.Fatalf("surround: %+v", pairs)
	}
	if pairs = planLinks(nil, []portInfo{dFL}); len(pairs) != 0 {
		t.Fatal("no source ports")
	}
}

func TestJSONStreamDecoder(t *testing.T) {
	// pw-dump -m prints concatenated arrays; json.Decoder must split them.
	src := `[{"id":1,"type":"x"}]` + "\n" + `[{"id":2,"info":null}]` + "\n"
	dec := json.NewDecoder(strings.NewReader(src))
	var n int
	for {
		var raw []pwRaw
		if err := dec.Decode(&raw); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("expected 2 batches, got %d", n)
	}
}

// ---- router decisions ------------------------------------------------------

func TestTargetDetectionTolerant(t *testing.T) {
	f := newFixture(t)
	r, _ := f.router(ModeSteamLog, true, func(c *Config) {
		c.TargetNodePatterns = []string{"steam_record_processing", "Steam Process Recording"}
	})
	graph(r, true)
	if r.target == nil || r.target.ID != 180 || r.target.Serial != 3688 || r.target.Name != "SteamProcessRecording" {
		t.Fatalf("target %+v", r.target)
	}
}

func TestSteamLogModeRoutesOnlyRequestedGame(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, _ := f.router(ModeSteamLog, true, nil)
	graph(r, true)
	d := decisions(r)
	if !d[203].Route || d[203].PID != 17998 || d[203].PIDSource != "client.pipewire.sec.pid" {
		t.Fatalf("Spelunky: %+v", d[203])
	}
	if !d[320].Route {
		t.Fatalf("child of the game: %s", d[320].Reason)
	}
	if d[109].Route || !strings.Contains(d[109].Reason, "no Steam ancestor") {
		t.Fatalf("Discord: %s", d[109].Reason)
	}
	if d[301].Route || !strings.Contains(d[301].Reason, "no Steam ancestor") {
		t.Fatalf("Wine outside Steam: %s", d[301].Reason)
	}
	if d[400].Route || !strings.Contains(d[400].Reason, "Steam itself") || d[400].PID != 6665 {
		t.Fatalf("Steam UI: %+v", d[400])
	}
	if d[100].Route || !strings.Contains(d[100].Reason, "combine") {
		t.Fatalf("combine stream: %s", d[100].Reason)
	}
}

func TestSteamLogModeWithoutRequestRoutesNothing(t *testing.T) {
	f := newFixture(t)
	f.writeLog("nothing relevant")
	r, _ := f.router(ModeSteamLog, true, nil)
	graph(r, true)
	d := decisions(r)
	if d[203].Route || !strings.Contains(d[203].Reason, "not capturing") {
		t.Fatalf("%s", d[203].Reason)
	}
}

func TestTwoGamesOnlyStreamedOne(t *testing.T) {
	f := newFixture(t)
	f.addProc(19000, fakeProc{"Game2.exe", 17875, 300, ""})
	f.writeLog(captureLine(19000, "enabled"))
	r, _ := f.router(ModeSteamLog, true, nil)
	graph(r, true, client(500, 19000, nil), stream(501, "Game2.exe", 500, nil))
	d := decisions(r)
	if !d[501].Route || d[203].Route {
		t.Fatalf("game2=%v spelunky=%v", d[501].Route, d[203].Route)
	}
}

func TestAllChildrenMode(t *testing.T) {
	f := newFixture(t)
	r, _ := f.router(ModeAllChildren, true, nil)
	graph(r, true)
	d := decisions(r)
	if !d[203].Route || d[301].Route || d[400].Route {
		t.Fatal("all-steam-children decisions")
	}
}

func TestFallbackWhenLogMissing(t *testing.T) {
	f := newFixture(t)
	r, _ := f.router(ModeSteamLog, true, nil) // no log file at all
	graph(r, true)
	d := decisions(r)
	if !d[203].Route || !strings.Contains(d[203].Reason, ModeAllChildren) {
		t.Fatalf("fallback: %s", d[203].Reason)
	}
	r2, _ := f.router(ModeSteamLog, true, func(c *Config) { c.FallbackWhenLogUnavailable = false })
	graph(r2, true)
	if decisions(r2)[203].Route {
		t.Fatal("no fallback requested")
	}
}

func TestNoTarget(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, _ := f.router(ModeSteamLog, true, nil)
	graph(r, false)
	d := decisions(r)
	if d[203].Route || !strings.Contains(d[203].Reason, "target node absent") {
		t.Fatal(d[203].Reason)
	}
}

func TestDryRunNeverRunsCommandsAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, _ := f.router(ModeSteamLog, true, nil) // runCmd fails the test if called
	graph(r, true)
	r.tailer.Poll()
	r.Reconcile("t1")
	r.Reconcile("t2")
	r.Reconcile("t3")
	if rec := r.routed[203]; rec == nil || !rec.Simulated {
		t.Fatal("dry-run should record a simulated route")
	}
}

func TestRealApplyAddsOnlyMissingLinksThenVerifies(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, func(c *Config) { c.LinkVerifyDelay = 0.01 })
	now := time.Now()
	r.now = func() time.Time { return now }
	// Spelunky is already wired to the user's sink (node 76): that link must survive untouched.
	graph(r, true, link(500, 203, 173, 76, 1), link(501, 203, 174, 76, 2))
	r.tailer.Poll()
	r.Reconcile("t1")
	want := [][]string{
		{"pw-link", "173", "95"}, {"pw-link", "174", "96"}, // Spelunky FL/FR -> target FL/FR
		{"pw-link", "330", "95"}, {"pw-link", "331", "96"}, // child "helper"
	}
	if fmt.Sprint(*calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v", *calls)
	}
	// PipeWire reports the new links
	r.state.Apply([]pwObject{link(900, 203, 173, 180, 95), link(901, 203, 174, 180, 96), link(902, 320, 330, 180, 95), link(903, 320, 331, 180, 96)})
	now = now.Add(time.Second)
	r.CheckPending()
	r.Reconcile("t2")
	if len(*calls) != 4 || !r.routed[203].Verified {
		t.Fatalf("idempotence: %d calls, verified=%v", len(*calls), r.routed[203].Verified)
	}
	// Steam stops capturing -> only OUR links are removed, by link id
	f.appendLog(captureLine(17998, "disabled"))
	r.tailer.Poll()
	r.Reconcile("t3")
	for _, id := range []string{"900", "901", "902", "903"} {
		if !hasCall(*calls, "pw-link", "-d", id) {
			t.Fatalf("expected removal of link %s, calls %v", id, *calls)
		}
	}
	for _, id := range []string{"500", "501"} {
		if hasCall(*calls, "pw-link", "-d", id) {
			t.Fatalf("the user's own link %s must never be removed", id)
		}
	}
	if _, still := r.routed[203]; still {
		t.Fatal("record should be gone")
	}
}

func TestPartiallyLinkedAddsOnlyTheMissingOne(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, nil)
	graph(r, true, link(900, 203, 173, 180, 95)) // FL already linked by hand
	r.tailer.Poll()
	r.Reconcile("t1")
	if linkCalls(*calls, "173") != 0 || linkCalls(*calls, "174") != 1 {
		t.Fatalf("calls %v", *calls)
	}
	// On disable, the hand-made FL link stays, our FR link goes
	r.state.Apply([]pwObject{link(901, 203, 174, 180, 96)})
	f.appendLog(captureLine(17998, "disabled"))
	r.tailer.Poll()
	r.Reconcile("t2")
	if hasCall(*calls, "pw-link", "-d", "900") || !hasCall(*calls, "pw-link", "-d", "901") {
		t.Fatalf("calls %v", *calls)
	}
}

func TestWaitsForPortsThenLinks(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, nil)
	graph(r, true)
	// Remove Spelunky's ports: the node arrived before its ports
	r.state.Apply([]pwObject{{ID: 173, Removed: true}, {ID: 174, Removed: true}})
	r.tailer.Poll()
	r.Reconcile("t1")
	if linkCalls(*calls, "173", "174") != 0 {
		t.Fatalf("must not link without ports: %v", *calls)
	}
	if r.HandleBatch(stereoOut(203, 173)) != true {
		t.Fatal("new ports must trigger a reconcile")
	}
	r.Reconcile("t2")
	if linkCalls(*calls, "173", "174") != 2 {
		t.Fatalf("expected 2 links after ports appeared: %v", *calls)
	}
}

func TestRetryLimitWhenLinksNeverAppear(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, func(c *Config) { c.LinkVerifyDelay = 0.01; c.MaxRouteAttempts = 2 })
	now := time.Now()
	r.now = func() time.Time { return now }
	graph(r, true)
	r.tailer.Poll()
	for i := 0; i < 6; i++ {
		r.Reconcile("t")
		now = now.Add(time.Second)
		r.CheckPending()
	}
	if n := linkCalls(*calls, "173"); n != 2 {
		t.Fatalf("expected 2 attempts, got %d", n)
	}
}

func TestAdoptsExistingLinksAndNeverRemovesThem(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, nil)
	graph(r, true, link(900, 203, 173, 180, 95), link(901, 203, 174, 180, 96))
	r.tailer.Poll()
	r.Reconcile("t1")
	if linkCalls(*calls, "173", "174") != 0 || !r.routed[203].Verified {
		t.Fatalf("existing links should be adopted: %v", *calls)
	}
	f.appendLog(captureLine(17998, "disabled"))
	r.tailer.Poll()
	r.Reconcile("t2")
	if hasCall(*calls, "pw-link", "-d", "900") || hasCall(*calls, "pw-link", "-d", "901") {
		t.Fatalf("adopted links must not be removed: %v", *calls)
	}
}

func TestPIDRecycledIsPruned(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, _ := f.router(ModeSteamLog, true, nil)
	graph(r, true)
	r.tailer.Poll()
	r.Reconcile("t1")
	if r.routed[203].StartTime != 204 {
		t.Fatal("record start time")
	}
	f.addProc(17998, fakeProc{"Spelunky.exe", 17875, 999, ""}) // same PID, new start time
	r.tailer.Poll()
	if len(r.tailer.EnabledPIDs()) != 0 {
		t.Fatal("recycled pid must be pruned from the Steam list")
	}
}

func TestTargetDisappearsForgetsWithoutCommands(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, nil)
	graph(r, true)
	r.tailer.Poll()
	r.Reconcile("t1")
	before := len(*calls)
	r.state.Apply([]pwObject{{ID: 180, Removed: true}, {ID: 100, Removed: true}, {ID: 95, Removed: true}, {ID: 96, Removed: true}})
	r.Reconcile("t2")
	if r.target != nil {
		t.Fatal("target should be gone")
	}
	if len(*calls) != before {
		t.Fatalf("no command expected when the target vanishes (links die with it): %v", (*calls)[before:])
	}
	if _, still := r.routed[203]; still {
		t.Fatal("record should be gone")
	}
}

func TestUnrouteOnExit(t *testing.T) {
	f := newFixture(t)
	f.writeLog(captureLine(17998, "enabled"))
	r, calls := f.router(ModeSteamLog, false, func(c *Config) { c.UnrouteOnExit = true })
	graph(r, true)
	r.tailer.Poll()
	r.Reconcile("t1")
	r.state.Apply([]pwObject{link(900, 203, 173, 180, 95), link(901, 203, 174, 180, 96)})
	r.Shutdown()
	if !hasCall(*calls, "pw-link", "-d", "900") || !hasCall(*calls, "pw-link", "-d", "901") {
		t.Fatalf("exit should remove our links: %v", *calls)
	}
}

func TestHandleBatchTriggers(t *testing.T) {
	f := newFixture(t)
	r, _ := f.router(ModeSteamLog, true, nil)
	if !r.HandleBatch([]pwObject{client(1, 1, nil)}) {
		t.Fatal("new client must trigger")
	}
	paramsOnly := client(1, 1, nil)
	paramsOnly.Info.ChangeMask = []string{"params"}
	if r.HandleBatch([]pwObject{paramsOnly}) {
		t.Fatal("client update must not trigger")
	}
	n := stream(5, "x", 1, nil)
	r.HandleBatch([]pwObject{n})
	n.Info.ChangeMask = []string{"params"}
	if r.HandleBatch([]pwObject{n}) {
		t.Fatal("params-only node update must not trigger")
	}
	n.Info.ChangeMask = []string{"props"}
	if !r.HandleBatch([]pwObject{n}) {
		t.Fatal("props change must trigger")
	}
	if !r.HandleBatch([]pwObject{port(99, 5, "output", "output_FL", "FL", 0, false)}) {
		t.Fatal("new port must trigger")
	}
	if !r.HandleBatch([]pwObject{{ID: 5, Removed: true}}) {
		t.Fatal("removal must trigger")
	}
}

// ---- config ----------------------------------------------------------------

func TestConfigParsing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")
	os.WriteFile(p, []byte(`
# comment
selection_mode = "all-steam-children" # trailing comment
poll_interval = 0.05
log_level = "debug"
dry_run = true
steam_ancestor_comms = [
  "steam",   # with comment
  "rea\"per",
]
pw_restart_backoff = [1, 2.5]
unknown_key = "ignored"
`), 0o644)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SelectionMode != ModeAllChildren || cfg.PollInterval != 0.25 || cfg.LogLevel != "debug" || !cfg.DryRun {
		t.Fatalf("%+v", cfg)
	}
	if len(cfg.SteamAncestorComms) != 2 || cfg.SteamAncestorComms[1] != `rea"per` {
		t.Fatalf("list: %v", cfg.SteamAncestorComms)
	}
	if len(cfg.PwRestartBackoff) != 2 || cfg.PwRestartBackoff[1] != 2.5 {
		t.Fatalf("floats: %v", cfg.PwRestartBackoff)
	}
	os.WriteFile(p, []byte(`selection_mode = "bogus"`), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("invalid mode must fail")
	}
	os.WriteFile(p, []byte(`[section]`), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("tables must fail")
	}
	if _, err := loadConfig(filepath.Join(dir, "missing.toml")); err != nil {
		t.Fatal("missing file uses defaults")
	}
	// The shipped example must parse to the defaults.
	vals, err := parseTOML(embeddedConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg = defaultConfig()
	if err := applyValues(&cfg, vals); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg) != fmt.Sprint(defaultConfig()) {
		t.Fatalf("embedded config differs from defaults:\n%+v\n%+v", cfg, defaultConfig())
	}
}
