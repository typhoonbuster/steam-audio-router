package main

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TargetInfo describes the node streams are linked to (Steam's capture sink).
type TargetInfo struct {
	ID        int
	Serial    int
	Name      string
	LinkGroup string
	ClientID  int
}

// Decision is the outcome of evaluating one Stream/Output/Audio node.
type Decision struct {
	Route     bool
	Reason    string
	PID       int
	PIDSource string
	Proc      *ProcStat
	SteamAnc  *ProcStat
	Chain     []ProcStat
}

// portPair identifies a link by its (output port, input port) ids.
type portPair [2]int

// RouteRecord remembers what we did to a node and to which process it belonged.
type RouteRecord struct {
	PID          int
	StartTime    int64
	TargetSerial int
	Attempts     int
	Created      map[portPair]bool // links created by us (adopted ones are not here)
	Verified     bool
	Simulated    bool
	GaveUpLogged bool
}

// Router owns the decision logic and the pw-link actions.
type Router struct {
	cfg    Config
	dryRun bool
	fs     ProcFS
	state  *PwState
	tailer *SteamLogTailer

	target         *TargetInfo
	routed         map[int]*RouteRecord
	pendingVerify  map[int]time.Time
	lastDecision   map[int]string
	fallbackWarned bool

	runCmd func(args []string) bool // external command runner, swapped in tests
	now    func() time.Time
}

func newRouter(cfg Config, dryRun bool, fs ProcFS, tailer *SteamLogTailer) *Router {
	r := &Router{cfg: cfg, dryRun: dryRun || cfg.DryRun, fs: fs, state: newPwState(), tailer: tailer,
		routed: map[int]*RouteRecord{}, pendingVerify: map[int]time.Time{}, lastDecision: map[int]string{},
		now: time.Now}
	r.runCmd = r.execCommand
	return r
}

// ResetState forgets everything learnt from a previous PipeWire connection.
func (r *Router) ResetState() {
	r.state.Clear()
	r.routed = map[int]*RouteRecord{}
	r.pendingVerify = map[int]time.Time{}
	r.lastDecision = map[int]string{}
	r.target = nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (r *Router) effectiveMode() string {
	if r.cfg.SelectionMode == ModeSteamLog && r.tailer.Available == AvailNo && r.cfg.FallbackWhenLogUnavailable {
		if !r.fallbackWarned {
			Warnf("Steam log unavailable: falling back to mode %s", ModeAllChildren)
			r.fallbackWarned = true
		}
		return ModeAllChildren
	}
	if r.tailer.Available == AvailYes && r.fallbackWarned {
		Infof("Steam log available again: back to mode %s", ModeSteamLog)
		r.fallbackWarned = false
	}
	return r.cfg.SelectionMode
}

// findTarget picks the node matching target_node_patterns: never a stream,
// exact (normalized) name preferred, then the most recently created.
func (r *Router) findTarget() *TargetInfo {
	patterns := make([]string, len(r.cfg.TargetNodePatterns))
	for i, p := range r.cfg.TargetNodePatterns {
		patterns[i] = normalizeName(p)
	}
	var best *TargetInfo
	bestExact := false
	for _, o := range r.state.OfType(typeNode) {
		props := o.props()
		if mc, _ := propString(props, "media.class"); strings.HasPrefix(mc, "Stream/") {
			continue
		}
		name, _ := propString(props, "node.name")
		desc, _ := propString(props, "node.description")
		var norms []string
		for _, n := range []string{name, desc} {
			if n != "" {
				norms = append(norms, normalizeName(n))
			}
		}
		exact, partial := false, false
		for _, n := range norms {
			for _, p := range patterns {
				if n == p {
					exact = true
				}
				if strings.Contains(n, p) {
					partial = true
				}
			}
		}
		if !partial {
			continue
		}
		serial, ok := propInt(props, "object.serial")
		if !ok {
			serial = o.ID
		}
		if name == "" {
			name = desc
		}
		lg, _ := propString(props, "node.link-group")
		cid, _ := propInt(props, "client.id")
		cand := &TargetInfo{ID: o.ID, Serial: serial, Name: name, LinkGroup: lg, ClientID: cid}
		if best == nil || (exact && !bestExact) || (exact == bestExact && serial > best.Serial) {
			best, bestExact = cand, exact
		}
	}
	return best
}

// resolvePID finds the process owning a stream. pipewire.sec.pid on the
// Client object is filled by the daemon from SO_PEERCRED (kernel-verified).
// For pipewire-pulse clients that PID is pipewire-pulse itself, so we use
// application.process.id reported by libpulse instead.
func (r *Router) resolvePID(node *pwObject) (int, string) {
	props := node.props()
	var cprops map[string]any
	if cid, ok := propInt(props, "client.id"); ok {
		if c := r.state.Get(cid); c != nil {
			cprops = c.props()
		}
	}
	api, _ := propString(props, "client.api")
	if api == "" {
		api, _ = propString(cprops, "client.api")
	}
	if api == "pipewire-pulse" {
		if pid, ok := propInt(props, "application.process.id"); ok {
			return pid, "node.application.process.id (pulse)"
		}
		if pid, ok := propInt(cprops, "application.process.id"); ok {
			return pid, "client.application.process.id (pulse)"
		}
	}
	if pid, ok := propInt(cprops, "pipewire.sec.pid"); ok {
		return pid, "client.pipewire.sec.pid"
	}
	if pid, ok := propInt(props, "application.process.id"); ok {
		return pid, "node.application.process.id"
	}
	if pid, ok := propInt(cprops, "application.process.id"); ok {
		return pid, "client.application.process.id"
	}
	return 0, "none"
}

func nodeLabel(o *pwObject) string {
	props := o.props()
	name, ok := propString(props, "node.name")
	if !ok {
		name, ok = propString(props, "application.name")
	}
	if !ok {
		name = "?"
	}
	return fmt.Sprintf("%s (id %d)", name, o.ID)
}

func isStream(o *pwObject) bool {
	mc, _ := propString(o.props(), "media.class")
	return mc == "Stream/Output/Audio"
}

// ---------------------------------------------------------------------------
// Decision
// ---------------------------------------------------------------------------

func (r *Router) evaluate(node *pwObject) Decision {
	props := node.props()
	if !isStream(node) {
		return Decision{Reason: "not a Stream/Output/Audio node"}
	}
	if r.target == nil {
		return Decision{Reason: "target node absent (no Remote Play session?)"}
	}
	if node.ID == r.target.ID {
		return Decision{Reason: "this is the target node"}
	}
	if lg, _ := propString(props, "node.link-group"); lg != "" && lg == r.target.LinkGroup {
		return Decision{Reason: "internal stream of the target sink (combine-stream)"}
	}
	pid, src := r.resolvePID(node)
	if pid == 0 {
		return Decision{Reason: "owner PID unknown (Client object not received yet?)"}
	}
	st, ok := r.fs.Stat(pid)
	if !ok {
		return Decision{Reason: fmt.Sprintf("process %d is gone or unreadable", pid), PID: pid, PIDSource: src}
	}
	names := r.cfg.SteamAncestorComms
	anc, chain := findSteamAncestor(r.fs, pid, names, false)
	d := Decision{PID: pid, PIDSource: src, Proc: &st, SteamAnc: anc, Chain: chain}
	if anc == nil {
		d.Reason = fmt.Sprintf("no Steam ancestor (chain: %s)", chainString(chain))
		return d
	}
	if !r.cfg.IncludeSteamItself && commMatches(st.Comm, names) {
		d.Reason = fmt.Sprintf("the process is Steam itself (%s)", st.Label())
		return d
	}
	// Only the SteamRT3 client is supported: its binary runs from steamrt64/.
	if exe, rt3 := steamClientInChain(r.fs, chain); !rt3 {
		if exe == "" {
			exe = "unknown"
		}
		d.Reason = fmt.Sprintf("the Steam client in the chain is not SteamRT3 (exe: %s); only the SteamRT3 client is supported", exe)
		return d
	}
	if r.effectiveMode() == ModeSteamLog {
		match := 0
		for _, epid := range r.tailer.EnabledPIDs() {
			if pid == epid || r.fs.IsDescendantOrSelf(pid, epid) {
				match = epid
				break
			}
		}
		if match == 0 {
			asked := "none"
			if pids := r.tailer.EnabledPIDs(); len(pids) > 0 {
				asked = fmt.Sprint(pids)
			}
			d.Reason = fmt.Sprintf("Steam is not capturing this process (PIDs requested by Steam: %s; chain: %s)",
				asked, chainString(chain))
			return d
		}
		d.Route = true
		d.Reason = fmt.Sprintf("Steam descendant via %s, and process %d requested by Steam (active Remote Play); chain: %s",
			anc.Label(), match, chainString(chain))
		return d
	}
	d.Route = true
	d.Reason = fmt.Sprintf("Steam descendant via %s (mode %s); chain: %s", anc.Label(), ModeAllChildren, chainString(chain))
	return d
}

// ---------------------------------------------------------------------------
// Actions: additive links with pw-link. The stream's existing links (to the
// user's sink, effects chain, ...) are never touched; we only add the links
// that are missing towards the target, and only ever remove links we added.
// ---------------------------------------------------------------------------

// command runs an external command unless in dry-run mode.
func (r *Router) command(args []string) bool {
	if r.dryRun {
		Infof("[DRY-RUN] %s", strings.Join(args, " "))
		return true
	}
	return r.runCmd(args)
}

func (r *Router) execCommand(args []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			Errorf("Command timed out: %s", strings.Join(args, " "))
		} else {
			Errorf("Command failed: %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return false
	}
	Debugf("OK: %s", strings.Join(args, " "))
	return true
}

func (r *Router) linkPorts(out, in int) bool {
	return r.command([]string{"pw-link", strconv.Itoa(out), strconv.Itoa(in)})
}

func (r *Router) unlink(linkID int) bool {
	return r.command([]string{"pw-link", "-d", strconv.Itoa(linkID)})
}

// missingLinks returns the planned port pairs that do not exist yet, plus the
// number of planned pairs (0 when ports are not known yet).
func (r *Router) missingLinks(nid int) (missing []portPair, planned int, names map[portPair]string) {
	src := r.state.Ports(nid, "output")
	dst := r.state.Ports(r.target.ID, "input")
	plan := planLinks(src, dst)
	existing := map[portPair]bool{}
	for _, l := range r.state.LinksBetween(nid, r.target.ID) {
		existing[portPair{l.OutPort, l.InPort}] = true
	}
	names = map[portPair]string{}
	for _, p := range plan {
		pair := portPair{p[0].ID, p[1].ID}
		names[pair] = fmt.Sprintf("%s -> %s:%s", p[0].Name, r.target.Name, p[1].Name)
		if !existing[pair] {
			missing = append(missing, pair)
		}
	}
	return missing, len(plan), names
}

func (r *Router) ensureRouted(node *pwObject, d Decision, announce bool) {
	label := nodeLabel(node)
	nid := node.ID
	serial := r.target.Serial
	rec := r.routed[nid]
	if rec != nil && rec.PID == d.PID && rec.StartTime != d.Proc.StartTime {
		Infof("%s: PID %d was recycled (starttime %d -> %d), re-evaluating", label, d.PID, rec.StartTime, d.Proc.StartTime)
		delete(r.routed, nid)
		rec = nil
	}
	if announce {
		Infof("Stream %s [pid %d via %s]: LINK -> %s: %s", label, d.PID, d.PIDSource, r.target.Name, d.Reason)
	}
	missing, planned, names := r.missingLinks(nid)
	if planned == 0 {
		Debugf("Stream %s: ports not known yet, waiting", label)
		return
	}
	if len(missing) == 0 {
		if rec == nil || rec.TargetSerial != serial {
			r.routed[nid] = &RouteRecord{PID: d.PID, StartTime: d.Proc.StartTime, TargetSerial: serial,
				Created: map[portPair]bool{}, Verified: true}
			Infof("Stream %s already linked to %s (%d links): adopted, nothing to do", label, r.target.Name, planned)
		} else if !rec.Verified {
			rec.Verified = true
			Infof("Links effective: %s -> %s (%d links)", label, r.target.Name, planned)
		}
		delete(r.pendingVerify, nid)
		return
	}
	if rec != nil && rec.TargetSerial == serial {
		if rec.Simulated {
			return // dry-run: already announced
		}
		if rec.Attempts >= r.cfg.MaxRouteAttempts {
			if !rec.GaveUpLogged {
				Errorf("Stream %s: %d attempts and still %d missing link(s) to %s, giving up until the target, the stream or the session changes",
					label, rec.Attempts, len(missing), r.target.Name)
				rec.GaveUpLogged = true
			}
			return
		}
		if deadline, pending := r.pendingVerify[nid]; pending && r.now().Before(deadline) {
			return // give PipeWire time to report the links we just asked for
		}
	}
	if rec == nil || rec.TargetSerial != serial {
		rec = &RouteRecord{PID: d.PID, StartTime: d.Proc.StartTime, TargetSerial: serial, Created: map[portPair]bool{}}
		r.routed[nid] = rec
	}
	rec.Attempts++
	rec.Simulated = r.dryRun
	suffix := ""
	if r.dryRun {
		suffix = " [DRY-RUN]"
	}
	Infof("Adding %d link(s) %s -> %s (attempt %d/%d)%s", len(missing), label, r.target.Name, rec.Attempts,
		r.cfg.MaxRouteAttempts, suffix)
	allOK := true
	for _, pair := range missing {
		Infof("  pw-link %s (ports %d -> %d)%s", names[pair], pair[0], pair[1], suffix)
		if r.linkPorts(pair[0], pair[1]) {
			if !r.dryRun {
				rec.Created[pair] = true
			}
		} else {
			allOK = false
		}
	}
	if !r.dryRun && (allOK || len(rec.Created) > 0) {
		r.pendingVerify[nid] = r.now().Add(secs(r.cfg.LinkVerifyDelay))
	}
}

// removeOwnLinks deletes the links we created between the node and the
// target. Links added by someone else (qpwgraph, the user) are left alone.
func (r *Router) removeOwnLinks(nid int, rec *RouteRecord, label string) {
	if r.target == nil || len(rec.Created) == 0 {
		return
	}
	for _, l := range r.state.LinksBetween(nid, r.target.ID) {
		if rec.Created[portPair{l.OutPort, l.InPort}] {
			Infof("  pw-link -d %d (ports %d -> %d)", l.ID, l.OutPort, l.InPort)
			r.unlink(l.ID)
		}
	}
}

func (r *Router) ensureUnrouted(node *pwObject, d Decision, announce bool) {
	nid := node.ID
	label := nodeLabel(node)
	if announce {
		if d.PID != 0 {
			Infof("Stream %s [pid %d via %s]: DO NOT LINK: %s", label, d.PID, d.PIDSource, d.Reason)
		} else {
			Debugf("Stream %s: skipped: %s", label, d.Reason)
		}
	}
	rec := r.routed[nid]
	if rec == nil {
		return
	}
	delete(r.pendingVerify, nid)
	delete(r.routed, nid)
	if r.target == nil || rec.TargetSerial != r.target.Serial {
		Infof("Stream %s: target gone, its links went with it", label)
		return
	}
	if !r.cfg.UnrouteOnDisable {
		Infof("Stream %s: no longer eligible, links kept (unroute_on_disable = false)", label)
		return
	}
	if len(rec.Created) == 0 {
		Infof("Stream %s: no longer eligible; its links were not ours, left untouched", label)
		return
	}
	suffix := ""
	if r.dryRun {
		suffix = " [DRY-RUN]"
	}
	Infof("Removing the %d link(s) we added for %s (%s)%s", len(rec.Created), label, d.Reason, suffix)
	r.removeOwnLinks(nid, rec, label)
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

func (r *Router) updateTarget() bool {
	nw := r.findTarget()
	old := r.target
	changed := (nw == nil) != (old == nil) || (nw != nil && old != nil && (nw.ID != old.ID || nw.Serial != old.Serial))
	r.target = nw
	if changed {
		if nw == nil {
			Infof("Target node gone (%s, id %d)", old.Name, old.ID)
		} else {
			Infof("Target node detected: %s (id %d, serial %d)", nw.Name, nw.ID, nw.Serial)
		}
		r.lastDecision = map[int]string{} // everything is re-evaluated and re-announced
	}
	return changed
}

// Reconcile evaluates every output stream and adds/removes links.
func (r *Router) Reconcile(reason string) {
	r.updateTarget()
	tname := "none"
	if r.target != nil {
		tname = r.target.Name
	}
	Debugf("Reconcile (%s): target=%s, Steam PIDs=%v", reason, tname, r.tailer.EnabledPIDs())
	for _, node := range r.state.OfType(typeNode) {
		if !isStream(node) {
			continue
		}
		d := r.evaluate(node)
		sig := strconv.FormatBool(d.Route) + "|" + d.Reason
		announce := r.lastDecision[node.ID] != sig
		r.lastDecision[node.ID] = sig
		if d.Route {
			r.ensureRouted(node, d, announce)
		} else {
			r.ensureUnrouted(node, d, announce)
		}
	}
	for nid := range r.routed {
		if r.state.Get(nid) == nil {
			Infof("Linked stream id %d disappeared: forgotten (its links went with it)", nid)
			delete(r.routed, nid)
			delete(r.pendingVerify, nid)
		}
	}
	for nid := range r.lastDecision {
		if r.state.Get(nid) == nil {
			delete(r.lastDecision, nid)
		}
	}
}

// CheckPending verifies, after link_verify_delay, that the links we asked
// for really exist in the graph; otherwise it forces a new attempt.
func (r *Router) CheckPending() {
	now := r.now()
	for nid, deadline := range r.pendingVerify {
		if now.Before(deadline) {
			continue
		}
		delete(r.pendingVerify, nid)
		node := r.state.Get(nid)
		rec := r.routed[nid]
		if node == nil || rec == nil || r.target == nil {
			continue
		}
		missing, planned, _ := r.missingLinks(nid)
		label := nodeLabel(node)
		if planned > 0 && len(missing) == 0 {
			rec.Verified = true
			Infof("Links effective: %s -> %s (%d links)", label, r.target.Name, planned)
		} else {
			Warnf("Stream %s: %d link(s) to %s still missing %.1fs after pw-link (attempt %d/%d)", label, len(missing),
				r.target.Name, r.cfg.LinkVerifyDelay, rec.Attempts, r.cfg.MaxRouteAttempts)
			delete(r.lastDecision, nid) // force a new attempt at the next reconcile
		}
	}
}

// HandleBatch applies a pw-dump batch and reports whether to reconcile.
// Only node/client/port/link additions or removals and node property changes
// matter.
func (r *Router) HandleBatch(batch []pwObject) bool {
	added, updated, removed := r.state.Apply(batch)
	if len(removed) > 0 {
		return true // type of a removed object is unknown: reconciling is cheap
	}
	for _, id := range added {
		if o := r.state.Get(id); o != nil {
			switch o.Type {
			case typeNode, typeClient, typeLink, typePort:
				return true
			}
		}
	}
	for _, id := range updated {
		o := r.state.Get(id)
		if o == nil || o.Type != typeNode {
			continue
		}
		for _, m := range o.Info.ChangeMask {
			if m == "props" {
				return true
			}
		}
	}
	return false
}

// Shutdown optionally removes the links we added, then releases the log.
func (r *Router) Shutdown() {
	if r.cfg.UnrouteOnExit && len(r.routed) > 0 && r.target != nil {
		Infof("Shutdown: removing the links we added for %d stream(s)", len(r.routed))
		for nid, rec := range r.routed {
			if node := r.state.Get(nid); node != nil && !rec.Simulated {
				r.removeOwnLinks(nid, rec, nodeLabel(node))
			}
			delete(r.routed, nid)
		}
	}
	r.tailer.Close()
}

// Report renders the `once` summary.
func (r *Router) Report() string {
	var b strings.Builder
	if r.target != nil {
		fmt.Fprintf(&b, "Target: %s (id %d, serial %d)\n", r.target.Name, r.target.ID, r.target.Serial)
	} else {
		fmt.Fprintf(&b, "Target: NONE (no node matches %v)\n", r.cfg.TargetNodePatterns)
	}
	mode := r.effectiveMode()
	if r.cfg.SelectionMode == ModeSteamLog {
		avail := "unavailable"
		if r.tailer.Available == AvailYes {
			avail = "ok"
		}
		fmt.Fprintf(&b, "Mode: %s (Steam log: %s)\n", mode, avail)
	} else {
		fmt.Fprintf(&b, "Mode: %s\n", mode)
	}
	pids := r.tailer.EnabledPIDs()
	if len(pids) == 0 {
		b.WriteString("PIDs Steam asks to capture: none\n")
	} else {
		fmt.Fprintf(&b, "PIDs Steam asks to capture: %v\n", pids)
	}
	fmt.Fprintf(&b, "Dry-run: %v\n\n", r.dryRun)
	var streams []*pwObject
	for _, o := range r.state.OfType(typeNode) {
		if isStream(o) {
			streams = append(streams, o)
		}
	}
	if len(streams) == 0 {
		b.WriteString("No Stream/Output/Audio node.\n")
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].ID < streams[j].ID })
	for _, node := range streams {
		d := r.evaluate(node)
		verdict := "IGNORE "
		if d.Route {
			verdict = "LINK   "
		}
		pid := "?"
		if d.PID != 0 {
			pid = strconv.Itoa(d.PID)
		}
		linked := ""
		if r.target != nil {
			if n := len(r.state.LinksBetween(node.ID, r.target.ID)); n > 0 {
				linked = fmt.Sprintf("[already linked: %d] ", n)
			}
		}
		fmt.Fprintf(&b, "%s %-44s pid=%-7s %s%s\n", verdict, nodeLabel(node), pid, linked, d.Reason)
	}
	return b.String()
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
