# rofi-bt redesign and headphone handoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A rofi menu with one row per Bluetooth device across both machines, and a "Play here" action that makes another machine let go of multipoint headphones for a moment without disconnecting them.

**Architecture:** BlueZ reports a per-device audio state (`none`/`idle`/`sending`) from `MediaTransport1`. Each remote-bluetooth server gets an `internal/audio` manager that releases a device by turning its PipeWire card profile off (streams parked on a null sink) and restores it exactly, exposed as `Release`/`Restore` RPCs. The `client` package orchestrates Play here (release the senders, wait until this machine sends, settle, restore). rofi-bt is rewritten around a pure view model (merge, status lines, actions) plus screen navigation.

**Tech Stack:** Go 1.26, godbus/dbus v5, gRPC + protobuf (protoc via `make generate`), `pactl` 17 (`-f json`) and `pw-metadata` from PipeWire, rofi-blocks via `github.com/andree-bjorkgard/rofi` v0.3.0.

**Spec:** `docs/superpowers/specs/2026-11-14-rofi-bt-handoff-design.md`; mockups `.oskestra/designs/rofi-bt.figcraft`.

Repos:
- **RB** = remote-bluetooth, this worktree (`/home/andree/.config/oskestra/worktrees/remote-bluetooth/familiarize-yourself-with-this-project-t`, branch `osk/familiarize-yourself-with-this-project-t`). Tasks 1–6.
- **RBT** = rofi-bt (`/home/andree/go/src/github.com/andree-bjorkgard/rofi-bt`, branch `remote-bluetooth-v2`). Tasks 7–8. Build with `GOWORK=/tmp/rbt-gowork/go.work` (already points its `replace` at RB); recreate it if missing:
  ```
  go 1.26.0
  use /home/andree/go/src/github.com/andree-bjorkgard/rofi-bt
  replace github.com/andree-bjorkgard/remote-bluetooth/v2 => <RB path>
  ```

## Global Constraints

- Only A2DP playback is handled; release turns the **whole** card profile `off` (this also ends HFP/headset mode).
- Hold: default **15 s**, maximum **30 s**; Play here settle **3 s**; Play here waits at most **15 s** for this machine to send.
- Null sink: `module-null-sink sink_name=rbt_hold sink_properties=device.description=Released-for-handoff`.
- Card name `bluez_card.<ADDR with _>`, sink prefix `bluez_output.<ADDR with _>.`.
- Never leave a machine released: restore on Restore RPC, hold expiry, server shutdown, and on start-up from `$XDG_RUNTIME_DIR/remote-bluetooth/release-<ADDR with _>.json`.
- Restore puts back exactly: the previous card profile, the default sink, every saved sink-input on its own saved sink, and `default.configured.audio.sink` (deleted with `pw-metadata -d 0 default.configured.audio.sink` when it was unset).
- Do not pause media players (the prototype's MPRIS step is dropped).
- Do not touch `~/.config/rofi` theme files. Not-connected rows dim their **text** only.
- Copy, verbatim from the spec: `▶ Sound from <machine>` / `▶ Sound from here` / `▶ Sound from both`; `Connected here`; `Connected on <machine>`; `Not connected`; `· paired on <machine>`; actions `Play here`, `Move here`, `Connect here`, `Connect on <machine>`, `Disconnect here`, `Disconnect on <machine>`, `Back`; progress `… Moving the sound here: <machine> has let go` and row `Switching…`; notifications `<alias>: sound moved here` / `<machine> let go and is back to normal`, and `Nothing is playing here` / `start the sound here, then try again`.
- Commit after each task (the user has approved commits on these branches); never push.

## Review Focus

1. **Server killed while a device is released** → next start restores it from the state file (Task 3 `TestRecoverRestoresFromStateFile`).
2. **Card was in headset mode (dictation) when released** → restore brings back `headset-head-unit`, not `a2dp-sink` (Task 3 `TestRestoreUsesPreviousProfile`).
3. **A stream started during the hold, or a stream's saved sink no longer exists** → it lands on the restored default sink and restore still completes (Task 3 `TestRestoreMovesNewAndOrphanedStreamsToDefault`).
4. **Release on a machine where the headphones are not connected / have no card** → `NotFound`, nothing loaded or changed (Task 3 `TestReleaseWithoutCardChangesNothing`, Task 4 `TestReleaseUnknownDevice`).
5. **One of the holders is unreachable during Play here** → the others are still released and restored, and the result names the failure (Task 5 `TestPlayHereOneHolderUnreachable`).

---

### Task 1: Audio state from BlueZ transports

**Files:**
- Modify: `bluez/bluez.go` (Device field, Devices/Device), `bluez/watch.go` (transport tracking)
- Test: `bluez/bluez_test.go`

**Interfaces:**
- Produces: `type AudioState uint8` with `AudioNone`, `AudioIdle`, `AudioSending` and `func (AudioState) String() string` (`"none"`, `"idle"`, `"sending"`); field `Device.Audio AudioState`.

- [ ] **Step 1: Write the failing tests** in `bluez/bluez_test.go`:
  - `TestAudioStateFromObjects`: build a managed-objects map with the device at `devPath` and `org.bluez.MediaTransport1` objects at `devPath+"/sep1/fd0"`; call `audioFromObjects(objs, devPath)`; assert `AudioNone` with no transport, `AudioIdle` with State `"idle"`, `AudioSending` with `"active"` and with `"pending"`, and `AudioSending` when one of two transports is active.
  - `TestWatcherTransport`: after the device-added signal (existing helpers), `InterfacesAdded` at `devPath+"/sep2/fd0"` with `MediaTransport1{State:"idle"}` → one change, `Audio == AudioIdle`; `PropertiesChanged` on that path, iface `org.bluez.MediaTransport1`, `State:"active"` → `AudioSending`; same again → no change; `InterfacesRemoved` of `MediaTransport1` on that path → `AudioNone`; a transport on a child path of an unknown device → no change.
- [ ] **Step 2:** Run `go test ./bluez -run 'Audio|Transport'` — expect compile failure (`audioFromObjects`, `AudioIdle` undefined).
- [ ] **Step 3: Implement.** `const transportIface = "org.bluez.MediaTransport1"`. `audioFromObjects(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, dev dbus.ObjectPath) AudioState` scans paths with prefix `dev+"/"`. Use it in `Devices`; make `Device(ctx, addr)` look the device up from `managedObjects` so it gets `Audio` too. In `watcher`, keep `transports map[dbus.ObjectPath]string` (state by transport path); map a child path to its device by taking the first `/dev_XX` segment under the adapter; recompute that device's `Audio` and emit a `Change` only when it differs. Seed transports in `Watch` from the same managed-objects call as the initial devices (change `Watch` to call `managedObjects` once and build both).
- [ ] **Step 4:** Run `go test -race ./bluez` — PASS; `go vet ./...` clean.
- [ ] **Step 5: Commit** `bluez: report per-device audio state from MediaTransport1`.

### Task 2: Protocol: audio state, Release and Restore

**Files:**
- Modify: `proto/remotebt/v1/remotebt.proto`; regenerate `internal/pb/remotebtv1/*` with `make generate`
- Modify: `internal/server/server.go` (`toProto`), `client/server.go` (`fromProto`), `client/client.go` (`Device`)
- Test: `internal/server/server_test.go`, `client/client_test.go`

**Interfaces:**
- Produces (proto):
  ```proto
  enum AudioState { AUDIO_STATE_NONE = 0; AUDIO_STATE_IDLE = 1; AUDIO_STATE_SENDING = 2; }
  // in Device:
  AudioState audio_state = 9;
  bool released = 10;   // this server is holding a release for the device
  rpc Release(ReleaseRequest) returns (ReleaseResponse);
  rpc Restore(RestoreRequest) returns (RestoreResponse);
  message ReleaseRequest { string address = 1; uint32 hold_seconds = 2; }  // 0 = default 15, capped at 30
  message ReleaseResponse {}
  message RestoreRequest { string address = 1; }
  message RestoreResponse {}
  ```
- Produces (Go): `client.Device` gains `Released bool` (beside the embedded `bluez.Device`, whose `Audio` now travels).

- [ ] **Step 1: Write the failing tests:** in `server_test.go` extend `TestListDevicesOnlyTrusted` fixtures with `Audio: bluez.AudioSending` on the headset and assert `resp.Devices[0].AudioState == pb.AudioState_AUDIO_STATE_SENDING`; in `client_test.go` `TestEndToEnd` assert `devs[0].Audio == bluez.AudioSending` (set it in the `headset` fixture).
- [ ] **Step 2:** `make generate` after editing the proto; run `go test ./...` — the two assertions FAIL (fields zero).
- [ ] **Step 3: Implement** the mapping in `toProto`/`fromProto` (unknown enum values → `AudioNone`). Leave `Release`/`Restore` unimplemented (the embedded `Unimplemented…` server returns `Unimplemented`) until Task 4.
- [ ] **Step 4:** `go test -race ./...` — PASS.
- [ ] **Step 5: Commit** `proto: audio state on devices, Release/Restore RPCs`.

### Task 3: `internal/audio`: release and restore with pactl

**Files:**
- Create: `internal/audio/audio.go` (Manager), `internal/audio/pactl.go` (command wrappers + parsing), `internal/audio/state.go` (state file)
- Test: `internal/audio/audio_test.go` (fake runner)

**Interfaces:**
- Produces:
  ```go
  type Runner interface { Run(ctx context.Context, name string, args ...string) ([]byte, error) }
  type ExecRunner struct{}                       // os/exec, stderr in the error
  var ErrNoCard = errors.New("audio: no bluetooth audio card for device")
  func New(r Runner, stateDir string, onChange func(address string, released bool)) *Manager
  func (m *Manager) Release(ctx context.Context, address string, hold time.Duration) error // idempotent: extends hold
  func (m *Manager) Restore(ctx context.Context, address string) error                   // no-op if not released
  func (m *Manager) Released(address string) bool
  func (m *Manager) Recover(ctx context.Context) error   // restore every state file found
  func (m *Manager) Close(ctx context.Context) error     // restore everything still released
  ```
- Commands (exact): `pactl -f json list cards` (fields `name`, `active_profile`), `pactl get-default-sink`, `pactl -f json list sink-inputs` (`index`, `sink`), `pactl -f json list sinks` (`index`, `name`), `pw-metadata 0 default.configured.audio.sink` (parse `value:'…'` from the `update:` line; absent → unset), `pactl load-module module-null-sink sink_name=rbt_hold sink_properties=device.description=Released-for-handoff` (prints module id), `pactl set-default-sink <name>`, `pactl move-sink-input <index> <sink name>`, `pactl set-card-profile <card> <profile>`, `pw-metadata 0 default.configured.audio.sink <value> Spa:String:JSON`, `pw-metadata -d 0 default.configured.audio.sink`, `pactl unload-module <id>`.
- State file `release-<ADDR_>.json`: `{"address","card","profile","default_sink","configured_sink","configured_set","module","streams":{"<index>":"<sink name>"}}`, written before the null sink is loaded, removed after restore.

- [ ] **Step 1: Write the failing tests** with a `fakeRunner` that keeps a tiny PipeWire model (cards with profiles, sinks, sink-inputs, metadata, modules), answers the commands above from it, and records every call:
  - `TestReleaseAndRestoreRoundTrip`: card `a2dp-sink`, default sink `bluez_output.00_0A_45_19_F3_A6.1`, configured `{"name":"bluez_output.58_18_62_63_08_B3.1"}`, streams `Spotify→jamesdsp_sink`, `hog→bluez_output…`. After `Release`: profile `off`, default `rbt_hold`, both streams on `rbt_hold`, `Released()==true`, `onChange(addr,true)` called, state file exists. After `Restore`: profile `a2dp-sink`, default back, `Spotify→jamesdsp_sink`, `hog→bluez_output…`, configured value identical, module unloaded, state file gone, `onChange(addr,false)`.
  - `TestRestoreUsesPreviousProfile`: card starts in `headset-head-unit` → restored to `headset-head-unit`.
  - `TestRestoreMovesNewAndOrphanedStreamsToDefault`: a stream added during the hold, and a saved stream whose sink was removed, both end on the restored default sink; Restore returns nil.
  - `TestRestoreDeletesUnsetConfiguredSink`: metadata unset before → `pw-metadata -d 0 default.configured.audio.sink` called.
  - `TestReleaseWithoutCardChangesNothing`: no matching card → `errors.Is(err, ErrNoCard)`, no `load-module`, no state file.
  - `TestHoldExpiryRestores`: `Release(…, 50*time.Millisecond)` → restored within 1 s without calling Restore.
  - `TestReleaseTwiceExtendsHold`: second Release does not load a second module; hold restarts.
  - `TestRecoverRestoresFromStateFile`: write a state file, `New(...).Recover` → card profile restored, file removed.
  - `TestCloseRestoresAll`.
- [ ] **Step 2:** `go test ./internal/audio` — FAIL (package missing).
- [ ] **Step 3: Implement.** One mutex around the per-address state map; timers via `time.AfterFunc` calling `Restore(context.Background(), addr)`. Release order: find card (else `ErrNoCard`) → read default, configured, streams → write state file → load null sink → set default → move streams → profile `off` → mark released, `onChange`, start timer. Restore order (spec): profile back → poll `list sinks` every 100 ms up to 5 s for the `bluez_output.<ADDR_>.` prefix (continue even if it never appears, but still restore the rest) → set default → move saved streams to their saved sinks, unknown/missing → default → metadata write or delete → unload module → remove file → `onChange(false)`. Restore collects errors (`errors.Join`) instead of stopping at the first.
- [ ] **Step 4:** `go test -race ./internal/audio` — PASS.
- [ ] **Step 5: Commit** `audio: release and restore a bluetooth card for handoff`.

### Task 4: Server: Release/Restore RPCs and `released` in device events

**Files:**
- Modify: `internal/server/server.go`, `cmd/remote-bluetooth/main.go` (`serve`)
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `audio.Manager` (Task 3) through
  ```go
  type Audio interface {
      Release(ctx context.Context, address string, hold time.Duration) error
      Restore(ctx context.Context, address string) error
      Released(address string) bool
  }
  ```
- Produces: `func New(backend Backend, audio Audio, log *slog.Logger) *Server` (audio may be nil → Release/Restore return `Unimplemented`); `func (s *Server) SetReleased(address string, released bool)` (re-broadcasts the device with `released` set; the manager's `onChange`).

- [ ] **Step 1: Write the failing tests** with a fake `Audio` added to `internal/fakebt` as `type Audio struct` (`func NewAudio() *Audio`, records calls as `"release <ADDR> <hold>"` / `"restore <ADDR>"` via `Calls() []string`, `FailRelease error` field, `OnChange func(string, bool)` called like the real manager) so Task 5 can reuse it:
  - `TestReleaseRestore`: Release on the trusted headset with `hold_seconds: 0` → the fake audio recorded `15*time.Second`; `hold_seconds: 99` → `30*time.Second`; a WatchDevices stream receives an update with `Released == true`, then after Restore one with `false`.
  - `TestReleaseUnknownDevice`: untrusted/unknown address → `codes.NotFound`, the fake audio not called; malformed → `InvalidArgument`; `FailRelease = audio.ErrNoCard` → `codes.FailedPrecondition`.
- [ ] **Step 2:** run, expect FAIL (signature of `New`).
- [ ] **Step 3: Implement** both RPCs through the existing `call` helper pattern (trusted check, normalized address, logging). Update all `New(...)` call sites (tests, `client_test.go`'s `startServer`, `serve`). In `serve`: `dir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "remote-bluetooth")` (fall back to `os.TempDir()`), `mgr := audio.New(audio.ExecRunner{}, dir, srv.SetReleased)` — construct `srv` first with a forwarding closure, then `mgr.Recover(ctx)` before serving (log errors, don't exit), and `mgr.Close(context.Background())` after `g.Stop()`.
- [ ] **Step 4:** `go test -race ./...` — PASS.
- [ ] **Step 5: Commit** `server: Release/Restore RPCs backed by the audio manager`.

### Task 5: Client: Release, Restore and PlayHere

**Files:**
- Create: `client/playhere.go`
- Modify: `client/server.go` (rpc wrappers, `released` mapping)
- Test: `client/playhere_test.go`

**Interfaces:**
- Produces:
  ```go
  func (c *Client) Release(ctx context.Context, server, address string, hold time.Duration) error
  func (c *Client) Restore(ctx context.Context, server, address string) error
  var ErrNotHeldElsewhere   = errors.New("client: no other computer is sending to this device")
  var ErrNothingPlayingHere = errors.New("client: nothing is playing here")
  type PlayHereResult struct { Released []string; Failed map[string]error }
  func (c *Client) PlayHere(ctx context.Context, address string, sendingHere func(context.Context) bool) (PlayHereResult, error)
  // package vars for tests: playHereHold = 15*time.Second, playHereWait = 15*time.Second, playHereSettle = 3*time.Second, playHerePoll = 200*time.Millisecond
  ```
  Algorithm (spec "Play here"): holders = reachable servers whose device `Audio == bluez.AudioSending`; none → `ErrNotHeldElsewhere`. Release on each in parallel (failures into `Failed`, success into `Released`); none succeeded → return the joined error. Poll `sendingHere` every `playHerePoll` up to `playHereWait`; timeout → `ErrNothingPlayingHere` (do **not** Restore; holders restore by their own hold). Then sleep `playHereSettle`, then Restore on every released server (errors into `Failed`), return result, nil.

- [ ] **Step 1: Write the failing tests** (reuse `startServer` from `client_test.go`, now with a fake `server.Audio` exported from `internal/fakebt` as `fakebt.Audio` so both test packages share it; set the vars to 10–50 ms):
  - `TestPlayHere`: two servers, headset `AudioSending` on server 1, `AudioIdle` on server 2; `sendingHere` returns true → result `Released == [server1]`, fake audio on server 1 saw Release then Restore, server 2 untouched.
  - `TestPlayHereNotHeld`: no server sending → `ErrNotHeldElsewhere`, no Release calls.
  - `TestPlayHereNothingHere`: `sendingHere` always false → `ErrNothingPlayingHere`, Release called, Restore not called.
  - `TestPlayHereOneHolderUnreachable`: both servers report the headset sending; server 2's `fakebt.Audio` has `FailRelease` set (a down server's devices disappear from the client, so a failing Release is the reachable stand-in) → result `Released == [server1]`, `Failed` has server 2, server 1 saw Restore.
- [ ] **Step 2:** run, expect FAIL.
- [ ] **Step 3: Implement** per the Interfaces block; `Release`/`Restore` follow `connect`'s pattern (`withDefaultTimeout`, `fromStatus`, `FailedPrecondition` mapped to a new `ErrNoAudio`).
- [ ] **Step 4:** `go test -race ./...` — PASS.
- [ ] **Step 5: Commit** `client: Release, Restore and PlayHere`.

### Task 6: CLI `play-here`, install and real test

**Files:**
- Modify: `cmd/remote-bluetooth/main.go`, `README.md` (commands table)

**Interfaces:**
- Consumes: `client.PlayHere`, `bluez.Open`, `Adapter.Device` (Task 1).

- [ ] **Step 1: Implement** `remote-bluetooth play-here [flags] ADDRESS`: client flags as `control`; wait up to `-wait` (default 5 s) for at least one ServerUp; `sendingHere` = local `adapter.Device(ctx, addr)` has `Audio == bluez.AudioSending`; print `sound moved here; <servers> let go and are back to normal`, or the error. Add a `devices` column `AUDIO` showing `sending`/`idle`/`-` and `released` when set.
- [ ] **Step 2:** `make test` — PASS. `make build`.
- [ ] **Step 3: Install on both machines.** xps17 has `v2` checked out, so push to a side ref and fast-forward there: `git push xps17:go/src/github.com/andree-bjorkgard/remote-bluetooth HEAD:refs/heads/v2-next`, then on xps17 `cd ~/go/src/github.com/andree-bjorkgard/remote-bluetooth && git status --short` (must be empty) `&& git merge --ff-only v2-next`. Then `make install && systemctl --user restart remote-bluetooth` on both, and `systemctl --user is-active remote-bluetooth` → `active`.
- [ ] **Step 4: Real test with the user** (A = ingenting, B = andree, ATH on both): start a silent hog on B (`pacat --playback --raw --format=s16le --rate=48000 --channels=2 --device=bluez_output.00_0A_45_19_F3_A6.1 < /dev/zero`), user plays on A and hears nothing, run `remote-bluetooth play-here 00:0A:45:19:F3:A6` **on A over ssh**; user confirms sound; check on B: profile `a2dp-sink`, Spotify on `jamesdsp_sink`, no `rbt_hold`, no state file. Kill the hog.
- [ ] **Step 5: Commit** `cli: play-here command and audio column`.

### Task 7: rofi-bt view model

**Files:**
- Create: `view.go` (pure), `view_test.go`

**Interfaces:**
- Consumes: `bluez.Device` (with `Audio`), `client.Device` (with `Server`, `Released`).
- Produces:
  ```go
  type presence struct { Machine string; Here bool; Dev bluez.Device; Released bool } // one per machine that knows the device
  type deviceView struct { Address string; Alias, Icon string; Battery int /* -1 none */; On []presence }
  func merge(local []bluez.Device, remote []client.Device) []deviceView          // trusted only, by address, stable order: as first seen
  func (v deviceView) status() (status, detail string, tone tone)                // tone: toneOn, toneNeutral, toneDim
  type actionKind int // actPlayHere, actMoveHere, actConnectHere, actConnectOn, actDisconnectHere, actDisconnectOn, actBack
  type action struct { Kind actionKind; Machine string; Label, Hint string }
  func (v deviceView) actions() []action
  func (v deviceView) message() []string // device screen message lines
  ```
  Rules (spec "rofi-bt menu", copy from Global Constraints): status — one machine `sending` → `▶ Sound from <m>` / `▶ Sound from here`; several → `▶ Sound from both`; then detail `· connected to both` / `· also here`; else connected here → `Connected here` + `· and on <m>`; connected elsewhere → `Connected on <m>`; else `Not connected` + `· paired on <m>` when known only elsewhere. Actions in order: Play here (connected here and some other machine `sending`), Move here (connected elsewhere, not here), Connect here (not connected anywhere, known here), Connect on <m> (not connected anywhere, known on m), Disconnect here (connected here), Disconnect on <m> (each machine connected), Back. Hints as in the design file.

- [ ] **Step 1: Write table tests** `TestStatus` and `TestActions` with the five situations of the design: ATH sending on ingenting + connected here; ATH sending on both; Glove80 connected here and on ingenting; MX Vertical connected on ingenting only; MX Master 3S known here only, not connected. Assert exact strings and action order. `TestMergeTrustedOnlyStableOrder`.
- [ ] **Step 2:** `GOWORK=/tmp/rbt-gowork/go.work go test ./...` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** tests PASS, `go vet` clean.
- [ ] **Step 5: Commit** (RBT) `view model for the merged menu`.

### Task 8: rofi-bt screens, navigation and Play here

**Files:**
- Modify: `main.go` (event loop, rendering); create `screens.go` (option building), `screens_test.go`

**Interfaces:**
- Consumes: Task 7 view model; `client.PlayHere`, `Client.Connect/Disconnect`, `bluez.Adapter`.
- Produces: option values `dev|<ADDR>` on the list, `act|<ADDR>|<kind>|<machine>` on a device screen (kind as int, machine may be empty); `func listOptions(views []deviceView, here string) []rofi.Option`, `func deviceOptions(v deviceView, busy bool) []rofi.Option`.

- [ ] **Step 1: Write tests** for `listOptions`/`deviceOptions`: values encode as above; dim rows wrap name and detail in `<span color="#928374">`; status in `#61C766` for toneOn; device screen first option is the first action; `busy` replaces the Play here row with `Switching…`; markup escaped.
- [ ] **Step 2:** FAIL, then **Step 3: implement**: screen state `{screen: list|device, addr string, busy bool}`; list → Return on `dev|` opens the device screen; `Back` returns; Esc is rofi's. Re-render the current screen on every local/remote change. Actions run in goroutines (as today) — Move here = Disconnect on each machine connected, then Connect locally. Play here: set `busy`, message line `… Moving the sound here: <m> has let go` in `#FDD835`, call `remote.PlayHere(ctx, addr, sendingHere)` with `sendingHere` reading the local view's `Audio`; then `notify-send` per Global Constraints copy (`ErrNothingPlayingHere` → the "Nothing is playing here" notification). Message line on the list: `<hostname> (this computer) · <other machines>`.
- [ ] **Step 4:** tests PASS; smoke test as before (`(sleep 4) | REMOTE_BLUETOOTH_SERVERS=… timeout 3 /tmp/rofi-bt` and parse the last JSON) shows merged rows with the spec's status lines.
- [ ] **Step 5:** `go install` on both machines (B with the GOWORK file; A from its checkout after pushing the RBT branch over ssh to `refs/heads/remote-bluetooth-v2-next` and `git merge --ff-only` there, as in Task 6), let the user open the menu and try Play here for real.
- [ ] **Step 6: Commit** (RBT) `merged device menu with Play here`.
