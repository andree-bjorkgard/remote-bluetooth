# rofi-bt redesign and headphone handoff

Design: `.oskestra/designs/rofi-bt.figcraft` (brief `rofi-bt.md` beside it).
Prior art: `/tmp/bt-handoff/` (README + `bt-handoff.sh`, a tested bash
prototype of the release step).

## Why

The owner has two Linux machines, `andree` (desktop) and `ingenting` (xps17),
both running remote-bluetooth v2, and multipoint headphones (Audio-Technica
ATH-M50xBT2, `00:0A:45:19:F3:A6`) connected to both. Multipoint headphones play
whichever machine is streaming A2DP and only switch when that stream stops. An
app on the other machine that keeps a silent stream open (browser tab, Slack,
a game) holds the headphones, so starting sound on this machine plays nothing.
Today the workaround is disconnecting the headphones on the other machine.

Separately, the rofi menu lists every device once per machine
(`ATH-M50xBT2` and `andree/ATH-M50xBT2`), which makes it hard to tell which is
which.

## Goals

1. **Merged menu.** One row per physical device across all machines; selecting
   one opens that device's screen with its actions.
2. **Play here.** From the machine you are on, one action makes the other
   machine let go of the headphones without disconnecting, so they switch to
   this one. It is a one-shot nudge only for when the switch does not happen
   by itself; normally the user never touches it.
3. **Back to normal by itself.** The machine that let go returns to its normal
   state on its own: as soon as this machine is streaming to the headphones,
   or after 15 s at the latest. Later, the same action from the other machine
   moves the sound back.
4. **Move here** for single-host devices (mice, keyboards): disconnect on the
   other machine, then connect here, in one action.

## Non-goals

- An automatic "follow the audio" mode (peak detection, auto-release after
  silence). Ideas in `/tmp/bt-handoff/README.md`; not now.
- Microphone / headset (HFP) profiles. Only A2DP playback.
- Handoff for non-audio devices beyond Move here.
- Publishing to GitHub; that is a separate step.

## Anti-goals (failure even if it works)

- A machine left released (profile `off`) after the handoff, including after
  a crash or service restart.
- Sound from the releasing machine's laptop speakers while it is released.
- The user's configured default sink (`default.configured.audio.sink` in
  PipeWire metadata) changed permanently.
- Having to undo anything by hand.

## Constraints

- Both machines: Arch, PipeWire + WirePlumber + `pipewire-pulse`; `pactl`
  (17, supports `-f json`) and `pw-metadata` installed. The server runs as a
  systemd **user** service, so it can reach the session's PipeWire.
- remote-bluetooth v2 (this repo) is the transport: mDNS discovery, gRPC
  over mutual TLS. rofi-bt (`~/go/src/github.com/andree-bjorkgard/rofi-bt`)
  uses its `client` and `bluez` packages. rofi-bt is a rofi-blocks program:
  stdout is the protocol, it lives only while the menu is open, Esc kills it.
- The menu must look like the existing rofi theme (`~/.config/rofi/styling/
  media.rasi`, `colors.rasi`); the design file already does.

## Approaches considered

- **A. Audio control inside the remote-bluetooth service, calling `pactl` and
  `pw-metadata` (chosen).** Same commands the prototype proved on this
  setup; one long-running Go program holds the state, so restore and cleanup
  are easy to get right.
- B. Inside the service via a Go PulseAudio-protocol library. No external
  commands, but a new dependency and more code for the same result, and
  `pw-metadata` would still be a command.
- C. Keep the bash script and have the service run it. Least code, but bash
  state files, a background watcher and `jq` on both machines; the fragile
  part of the prototype.

The owner picked A.

## How it works

### Audio state, from BlueZ

`bluez.Device` gains `Audio AudioState`: `none` (no A2DP transport), `idle`,
`sending` (this machine is streaming to the headphones; that does not mean
the headphones play it, see "Tested by hand"). It comes from `org.bluez.MediaTransport1.State` on the device's
child objects (`…/dev_XX/sepN/fdN`): any transport `active` or `pending` →
`sending`, any transport present → `idle`, none → `none`.
The watcher already receives InterfacesAdded/Removed and PropertiesChanged
under the adapter path; today it ignores child paths and must now map
MediaTransport1 on `dev_XX/...` back to its device.

The proto `Device` gains `AudioState audio_state` with the same values plus
`released` (set by the server while it is holding a release for that
device). The server's change stream reports audio-state changes like any
other property change.

### Release on the holding machine (server side)

New RPC `Release(ReleaseRequest{address, hold_seconds})` and
`Restore(RestoreRequest{address})`. Only trusted devices; `hold_seconds`
capped at 30 (default 15).

Release, for the device's PipeWire card `bluez_card.<ADDR_WITH_UNDERSCORES>`:
1. Save `default.configured.audio.sink` (from `pw-metadata 0`), the
   current default sink, and for every sink-input the sink it is on.
2. Load `module-null-sink sink_name=rbt_hold
   sink_properties=device.description=Released-for-handoff`, set it as the
   default sink, move every sink-input to it.
3. `pactl set-card-profile <card> off`. The BR/EDR link stays up; the A2DP
   transport goes away.
4. Mark the device `released`; start the hold timer.

Restore (on `Restore`, hold timer expiry, or server shutdown):
1. Set the card profile back to the one active before release (normally
   `a2dp-sink`), wait up to 5 s for the `bluez_output.<ADDR>.*` sink.
2. Set the saved default sink again and move every saved sink-input back
   to its own saved sink (a stream that started during the hold goes to the
   default sink).
3. Write back the saved `default.configured.audio.sink` exactly
   (`Spa:String:JSON`), or delete the key if it was unset.
4. Unload the null sink; clear `released`.

Release is idempotent (a second Release extends the hold). Restore of a
device that is not released is a no-op. The prototype's MPRIS pausing is
**dropped**: we restore within seconds, so pausing players would just leave
them paused.

Crash safety: before step 2 the server writes the saved state (card, previous
profile, configured sink, null-sink module id) to
`$XDG_RUNTIME_DIR/remote-bluetooth/release-<ADDR>.json` and removes it after
restore. On start-up the server restores anything such a file describes.

### Play here (the machine you are on)

1. Offered when the device is connected here and at least one other server
   reports it `sending`.
2. Call `Release(addr, 15)` on every other server reporting it `sending`.
3. Once every release returned, wait until this machine is `sending` for
   the device (it usually already is: the user pressed Play here because
   they hear nothing), then a 3 s settle, then `Restore(addr)` on those
   servers. With the holders released, the only stream left is this
   machine's, so the headphones switch within the settle time; the test
   showed restoring afterwards does not take them back.
4. If this machine is not `sending` within 15 s, report that nothing is
   playing here; the servers restore on their own at 15 s.

Success cannot be verified from BlueZ (see the test), so the notification
says what was done: `<alias>: sound moved here / <machine> let go and is back
to normal`.

This orchestration lives in the **client library**
(`client.PlayHere(ctx, address, …)`; builder's call on the exact shape) so
rofi-bt and a CLI command share it. If the rofi menu is closed mid-way, the
holding servers still restore at 15 s; only the notification is lost. A CLI
command `remote-bluetooth play-here ADDR` is added for testing and for
binding to a key.

### Move here (single-host devices)

`Disconnect` on the server(s) where it is connected, then `Connect`
locally. Offered when the device is connected elsewhere and not here.

### rofi-bt menu

Exactly as the design file shows:

- **Device list.** Merge local devices (package `bluez`) and remote ones
  (`client`) by address. Row: icon, alias, battery (when connected), and a
  second line:
  - `▶ Sound from <machine>` / `▶ Sound from here` (green) when exactly one
    machine is `sending`; `▶ Sound from both` when several are (BlueZ cannot
    tell which one the headphones play, so Play here stays offered on every
    machine that is sending); followed by `· connected to both` /
    `· also here`;
  - `Connected here` (green) `· and on <machine>`;
  - `Connected on <machine>` (neutral);
  - `Not connected` (dim), `· paired on <machine>` when it is only known
    elsewhere. Not-connected rows dim their text with markup; the icon stays
    as is, because rofi-blocks cannot fade one row's icon without changing
    the shared rofi theme (`media.rasi` is also used by rofi-media), and the
    theme is not to be touched. (Builder's call; deviates from the design.)
  Order: by recent use (existing rofi history), new devices appended; the
  list must not reorder while open.
- **Device screen.** Prompt = alias; message = audio line + connections and
  battery; then actions, first one preselected:
  - headphones with another machine sending: `Play here`, `Disconnect here`,
    `Disconnect on <machine>`, `Back`;
  - connected elsewhere only: `Move here`, `Disconnect on <machine>`, `Back`;
  - not connected: `Connect here`, `Connect on <machine>` (when paired
    there), `Back`;
  - connected here: `Disconnect here` (+ the elsewhere actions), `Back`.
  Each action row has a one-line hint as in the design.
- **While Play here runs** the menu stays open: the message turns yellow
  (`… Moving the sound here: <machine> has let go`) and the first row shows
  `Switching…`. Then a notification (`notify-send`): success
  `<alias>: sound moved here / <machine> let go and is back to normal`, or
  `Nothing is playing here / start the sound here, then try again`.
- Navigation: option values encode screen and action; `Back` returns to the
  list; the list and the device screen keep updating live.

## Tested by hand (2026-11-14)

Setup: ATH-M50xBT2 connected to both machines. A = ingenting, B = andree.

1. A played a Firefox video; then a silent stream (`pacat` from
   `/dev/zero`, like `speech-dispatcher-dummy`) started on B. A kept the
   headphones (first come, first served).
2. A paused, then played again: **silent**. B's invisible stream held the
   headphones; the real-life problem, reproduced.
3. Release on B (steps as below): the headphones **switched to A** at once.
   B stayed connected.
4. Restore on B 20 s later, with the silent stream still running and moved
   back to the headphones: **A kept the sound**. No flip back.

So the design holds: release, then restore once this machine is streaming.

Two things the test showed that the design now accounts for:

- **BlueZ cannot tell who the headphones are playing.** With A playing and
  B's silent stream running, *both* machines reported their transport
  `active`. The transport state only says a machine is *sending*.
- **Restore must put every stream back on the sink it came from.** On B,
  Spotify ran through `jamesdsp_sink`; moving all streams to the default
  sink rerouted it. The default sink itself (`pactl get-default-sink`) and
  the configured default (metadata) are both saved and restored.

Not seen yet: the real-life hogs (dictation / headset-mic use, which puts
the headphones in HFP "call" mode; `speech-dispatcher-dummy` was found with
an open stream on A). Release turns the whole card profile off, which ends
HFP too, so the same mechanism applies; to be confirmed the next time it
happens for real.

## Left to the builder

- Exact Go APIs, proto field names, error codes.
- How `pactl` output is parsed (`-f json` preferred), polling vs
  `pactl subscribe` for waiting on the sink.
- Glyphs and exact copy beyond what the design shows.
- Tests: fake `pactl`/`pw-metadata` (e.g. an interface around a command
  runner) for the release/restore sequence; watcher tests for transport
  state.
