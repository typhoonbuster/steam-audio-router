# steam-audio-router

Adds the missing PipeWire links between the Steam game currently streamed
with **Remote Play** and Steam's capture node, so the remote client gets
sound. Nothing else in your graph is touched: the game keeps playing to
whatever sink you chose, Steam's capture node simply receives a copy. One
static binary, no runtime dependency beyond PipeWire's own `pw-dump` /
`pw-link`, works with or without systemd.

## Why this exists

The current Steam client for Linux (the SteamRT3 / 64-bit runtime, still in
beta at the time of writing) does not capture the game's audio when you
stream it with Remote Play: the remote client sees the picture but hears
nothing. This is a long-standing issue on Linux, and it survived the move
to the new runtime. Everything else about SteamRT3 is a clear step forward,
Remote Play included: input, latency and compatibility work far better than
with the old client, sound being the one thing left broken. Rather than go
back, this project fills the gap: it attaches the game's audio to Steam's
capture node so the remote clients hear the game, and leaves the rest of
your audio setup untouched.

> On current Steam clients (beta, SteamRT3) the capture node is named
> **`SteamProcessRecording`**. Matching is tolerant (case, spaces, dashes,
> underscores, suffixes) and the pattern list is configurable.

## The problem in detail

When a Remote Play session starts, Steam:

1. creates, through pipewire-pulse, a virtual sink `SteamProcessRecording`
   (a combine-stream with `steam.autounload = true`, mirrored to the default
   sink so local playback continues);
2. opens a capture stream `Steam` on that sink's monitor;
3. logs in `logs/streaming_log.txt`
   `Telling process 17998 to change audio capture state: enabled` then
   `Recording process audio: 17998`, and **asks the game process itself**
   (through the overlay) to move its output to that sink.

Step 3 only works when the game talks to libpulse. Games whose audio stack
speaks PipeWire natively (SDL3, recent Wine/Proton builds, native Linux
engines) are never connected; their node does not even carry
`application.process.id`, so Steam cannot find them. The fix is the same
thing you would do by hand in qpwgraph: draw `Game:output_FL/FR ->
SteamProcessRecording:playback_FL/FR` next to the existing cables. This tool
draws those cables automatically, for the right game only, and nothing more.

## Requirements

- **The SteamRT3 Steam client only** (the 64-bit runtime, whose binary runs
  from `~/.local/share/Steam/steamrt64/`). The router checks the executable
  of the `steam` process in each stream's ancestry and refuses to link
  anything launched by the legacy `ubuntu12_32` client; the journal says so
  explicitly.
- PipeWire with `pw-dump` and `pw-link` (both in the `pipewire` package) and
  a session manager such as WirePlumber.
- Go ≥ 1.22, only to build.

## Install

```bash
make build
```

```bash
./release/steam-audio-router install
```

`install` copies the binary to `~/.local/bin/steam-audio-router`, writes
`~/.config/steam-audio-router/config.toml` (kept if it already exists; a
fresh `config.example.toml` is always written) and:

- with a working `systemctl --user`: writes
  `~/.config/systemd/user/steam-audio-router.service` and runs
  `daemon-reload`;
- otherwise: writes an XDG autostart entry
  `~/.config/autostart/steam-audio-router.desktop` that starts the router
  with your desktop session and logs to
  `~/.local/state/steam-audio-router/steam-audio-router.log`.

Force one or the other with `--systemd`, `--autostart`, or install files only
with `--no-service`. Nothing is started yet.

### Test first, with a game streaming

```bash
steam-audio-router once --dry-run
```

`once` evaluates the streams present right now, prints one line per
`Stream/Output/Audio` node with the decision and its reason, and exits.
Example captured on the development machine (Spelunky streaming, Discord
open):

```
Target: SteamProcessRecording (id 181, serial 4034)
Mode: steam-log (Steam log: ok)
PIDs Steam asks to capture: [17998]
Dry-run: true

IGNORE  WEBRTC VoiceEngine (id 109)   pid=6067   no Steam ancestor (chain: Discord[6067] <- Discord[5914] <- systemd[1698] <- systemd[1])
IGNORE  output.SteamProcessRecording_easyeffects_sink (id 174)   pid=?   internal stream of the target sink (combine-stream)
LINK    Spelunky.exe (id 203)   pid=17998   Steam descendant via reaper[17810], and process 17998 requested by Steam (active Remote Play); chain: Spelunky.exe[17998] <- pv-adverb[17875] <- srt-bwrap[17812] <- reaper[17810] <- steam-runtime-s[17809] <- x86_64-linux-gn[6571] <- srt-client-cont[6560] <- steam[6509] <- ...
```

`steam-audio-router once` (without `--dry-run`) adds the links once and
exits: a good way to confirm the remote client gets sound before enabling the
service.

### Enable

```bash
steam-audio-router install --enable
```

With systemd this enables and (re)starts the unit; without systemd it starts
the router in the background now and at every login. Equivalent manual
commands with systemd:

```bash
systemctl --user enable --now steam-audio-router.service
```

```bash
journalctl --user -u steam-audio-router -f
```

Without systemd (runit, OpenRC, dinit, s6, a bare window manager…) you can
also run it from any user-level supervisor or session script:

```bash
steam-audio-router run --log-file ~/.local/state/steam-audio-router/steam-audio-router.log
```

### Uninstall

```bash
steam-audio-router uninstall
```

Stops the service, removes the unit, the autostart entry and the binary.
`~/.config/steam-audio-router` is kept.

## Configuration

`~/.config/steam-audio-router/config.toml`, fully commented. Main keys:

| Key | Default | Meaning |
|---|---|---|
| `target_node_patterns` | `["SteamProcessRecording", "SteamRecordProcessing"]` | Names (or fragments) of the capture node |
| `steam_ancestor_comms` | `steam`, `steamwebhelper`, `reaper`, … | `comm` names accepted as Steam ancestors |
| `selection_mode` | `"steam-log"` | `steam-log`: only the game Steam declares; `all-steam-children`: every Steam descendant |
| `steam_log_path` | `~/.steam/steam/logs/streaming_log.txt` | Steam's streaming log |
| `fallback_when_log_unavailable` | `true` | Switch to `all-steam-children` if the log is unreadable |
| `log_level` | `"info"` | `debug`, `info`, `warning`, `error` |
| `dry_run` | `false` | Log decisions, never run `pw-link` |
| `poll_interval` | `1.0` | Seconds between Steam-log checks (PipeWire is event-driven) |
| `unroute_on_disable` | `true` | Remove the links *we* added when Steam stops capturing the process |
| `unroute_on_exit` | `false` | Remove the links we added on SIGTERM |

`steam-audio-router print-config` shows the effective values. Restart the
service after editing.

## Checking that the links are there

A successful session in the journal:

```
Target node detected: SteamProcessRecording (id 181, serial 4034)
Steam requests audio capture of process Spelunky.exe[17998]
Stream Spelunky.exe (id 203) [pid 17998 via client.pipewire.sec.pid]: LINK -> SteamProcessRecording: Steam descendant via reaper[17810], and process 17998 requested by Steam (active Remote Play); chain: ...
Adding 2 link(s) Spelunky.exe (id 203) -> SteamProcessRecording (attempt 1/3)
  pw-link output_FL -> SteamProcessRecording:playback_FL (ports 173 -> 95)
  pw-link output_FR -> SteamProcessRecording:playback_FR (ports 174 -> 96)
Links effective: Spelunky.exe (id 203) -> SteamProcessRecording (2 links)
```

Independent check:

```bash
pw-link -l | grep -A2 'Spelunky.exe:output'
```

must list, under each output port of the game, **both** your usual sink (for
example `Easy Effects Sink:playback_FL`) and `SteamProcessRecording:playback_FL`.
The first cable is yours and was never touched; the second is the one this
tool added.

In **qpwgraph** you see the same: the game's outputs go to your sink *and* to
`SteamProcessRecording`, whose monitor feeds the `Steam` capture stream.
qpwgraph's patchbay in exclusive mode would remove cables it does not know
about, so leave it off (or add the same cables to your patchbay).

When the remote client goes back to the Steam UI, Steam logs "Starting
desktop stream" and disables the capture: the journal shows `Steam stops
audio capture ...` / `Removing the 2 link(s) we added ...`, then the links
come back when the game regains focus (set `unroute_on_disable = false` to
keep them). At the end of the session Steam destroys the sink, the links die
with it, and the game keeps playing to your sink as it always did.

## Debugging

```bash
steam-audio-router once --dry-run --log-level debug
```

- **"target node absent"**: no Remote Play session, or the sink name
  changed. `pw-dump | grep -i record` then adjust `target_node_patterns`.
- **"Steam is not capturing this process (PIDs requested by Steam: none)"**:
  Steam has not written `Telling process ... enabled` yet. Check
  `grep "audio capture state" ~/.steam/steam/logs/streaming_log.txt | tail`.
  If Valve changed the format, set `selection_mode = "all-steam-children"`.
- **"no Steam ancestor (chain: ...)"**: the chain shows exactly which
  processes the stream came through; extend `steam_ancestor_comms` if Steam
  renamed a launcher.
- **"the Steam client in the chain is not SteamRT3"**: the game was started
  by the legacy 32-bit client. Switch Steam to the SteamRT3 client (beta) or
  use another tool; this one is deliberately limited to SteamRT3.
- **"2 link(s) to SteamProcessRecording still missing 2.0s after pw-link"**:
  the links were created but disappeared, or `pw-link` failed (its error is
  logged just above). Something is removing cables: an exclusive qpwgraph
  patchbay, a WirePlumber script. The router retries `max_route_attempts`
  times.
- Unit tests: `make test`. End-to-end test on the real daemon without
  touching your nodes (temporary silent sink and stream, torn down after
  25 s at most): `make integration`.

## Design notes

**Events, not polling.** The router spawns `pw-dump -m` and decodes its JSON
output with `encoding/json`'s streaming decoder; it wakes up only when
PipeWire emits a change (node, client, port, link). The only polling is a
`stat()` of the Steam log every second plus a full reconciliation every 10 s
as a safety net over a few dozen in-memory objects. If PipeWire restarts,
`pw-dump` exits; the router restarts it with a backoff (1, 2, 5, 10 s),
starts from an empty state and re-evaluates everything. Why not libpipewire
through cgo? It would need PipeWire headers at build time and make the binary
dynamic; `pw-dump`/`pw-link` are part of PipeWire itself, so there is no
extra dependency, and `pw-link` creates exactly the same Link objects
qpwgraph does when you draw a cable.

**Owner PID from `pipewire.sec.pid`, not `application.process.id`.** The
game's node carries no process property at all. Its Client object does:
`pipewire.sec.pid` is filled by the PipeWire daemon from `SO_PEERCRED` on the
Unix socket, so the kernel vouches for it and a client cannot lie. The one
exception is clients going through pipewire-pulse, for which `pipewire.sec.pid`
is the pipewire-pulse daemon; the router detects `client.api = pipewire-pulse`
and falls back to `application.process.id` set by libpulse (what Steam
itself relies on).

**Ancestry, not names.** `/proc/<pid>/stat` is walked PPID by PPID up to PID
1; the stream "comes from Steam" if a strict ancestor's `comm` is in the
configurable list (compared after 15-byte truncation, like the kernel:
`steam-runtime-launcher-service` becomes `steam-runtime-l`). The chain seen on
SteamRT3 + Proton is `Game.exe <- pv-adverb <- srt-bwrap <- reaper <-
steam-runtime-supervisor <- x86_64-linux-gnu-srt-launcher-service <-
srt-client-container <- steam`. Two facts make this reliable:
pressure-vessel shares the host PID namespace (`/proc/<game>/ns/pid` is the
same as ours), so PIDs seen by PipeWire and by Steam are the same; and
`reaper` is a child subreaper, so an orphaned game process is re-parented to
`reaper`, not to PID 1, and the chain survives. A Wine, SDL or Proton
application started outside Steam (Lutris, umu, a terminal) has none of these
ancestors and is never touched, whatever its `application.name`.

**Only the game of the active session.** Steam writes the PID it wants to
capture in `streaming_log.txt`. The router tails that file (replays the tail
at startup, detects rotation when Steam restarts) and routes a stream only
when its PID is that process or one of its descendants, **and** it descends
from Steam. Two games running side by side: only the streamed one is routed;
when Steam switches to "desktop stream", routing is removed and re-applied on
resume. If the log becomes unreadable, the router falls back (configurable)
to "all Steam descendants".

**PID + start time against recycling.** Every PID is stored with field 22 of
`/proc/<pid>/stat` (clock ticks since boot). An "enabled" log line whose PID
now belongs to a process started *after* the line is ignored; a captured PID
whose start time changed is pruned; a routed stream whose PID was recycled is
re-evaluated from scratch.

**Additive links with `pw-link`, not `target.object`.** The obvious
PipeWire way to "send a stream somewhere" is the `target.object` metadata
(what `pactl move-sink-input` does). It *moves* the stream: WirePlumber tears
down the links to your sink and rebuilds them towards the target. That is
wrong here for two reasons. First, the host must keep hearing the game
exactly as configured (sink choice, effects chain, per-app volume), and that
is the user's business, not ours. Second, Steam's `SteamProcessRecording`
combine sink does not necessarily reach a real output on your system: on the
development machine its output stream is left unconnected, so a moved game
would simply go silent locally. What is actually missing is one extra pair of
cables, the ones you would draw in qpwgraph: `Game:output_FL ->
SteamProcessRecording:playback_FL` and the same for FR. The router therefore
pairs the game's output ports with the target's playback ports by
`audio.channel` (a mono source fans out to every target channel; unnamed
channels fall back to positions; extra surround channels are left alone),
creates only the pairs that do not exist yet with `pw-link` (lingering links,
the same Link objects a patchbay creates), and never touches any other link.
A link is destroyed by PipeWire itself when either port disappears, so when
the game quits or Steam tears the sink down there is nothing to clean up.

**Idempotence and verification.** Before acting, the router compares the
planned pairs with the real `Link` objects; a stream already wired to the
target (by you, by a patchbay, by a previous run) is adopted without any
command and those links are never removed. After `pw-link` it waits
`link_verify_delay` and confirms the links exist, otherwise retries up to
`max_route_attempts`. On "disabled" it removes only the links it created
itself. Decisions are logged at INFO only when they change.

**Clean shutdown.** SIGTERM/SIGINT/SIGHUP end the loop, `pw-dump` is
terminated, the Steam log closed, and with `unroute_on_exit = true` the links
the router added are removed. Under journald, lines carry `<N>` priority prefixes so
`journalctl -p warning` filters correctly; with `--log-file` they get
timestamps and the file is rotated past 5 MiB.

## Known limits

- Depends on the wording of two lines in `streaming_log.txt`; the
  `all-steam-children` mode is the escape hatch.
- Steam streams one game per host; should Valve allow several concurrent
  sessions, every announced PID would be linked to the same sink.
- A game with more channels than the capture sink (5.1 into stereo) only
  gets its FL/FR linked; center and LFE are not downmixed.
- Sounds of the Steam client itself (UI) are not linked
  (`include_steam_itself = false`).
