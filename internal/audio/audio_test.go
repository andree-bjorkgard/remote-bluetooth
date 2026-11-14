package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAddr   = "00:0A:45:19:F3:A6"
	testCard   = "bluez_card.00_0A_45_19_F3_A6"
	testBTSink = "bluez_output.00_0A_45_19_F3_A6.1"
	testConfig = `{"name":"bluez_output.58_18_62_63_08_B3.1"}`
	metaKey    = "default.configured.audio.sink"
)

// fakeRunner is a tiny model of PipeWire as seen through pactl and
// pw-metadata. It answers the commands the Manager uses and records them.
type fakeRunner struct {
	mu        sync.Mutex
	cards     map[string]string // card name -> active profile
	sinks     []fakeSink
	inputs    map[int]int // sink-input index -> sink index
	def       string
	confVal   string
	confSet   bool
	modules   map[int]string // module id -> sink name
	nextIndex int
	nextMod   int
	calls     [][]string
	failCmd   string // a command that fails, for error paths
}

type fakeSink struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Owner int    `json:"owner_module,omitempty"`
}

func newFake() *fakeRunner {
	return &fakeRunner{
		cards:     map[string]string{},
		inputs:    map[int]int{},
		modules:   map[int]string{},
		nextIndex: 100,
		nextMod:   500,
	}
}

func (f *fakeRunner) addSink(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addSinkLocked(name)
}

func (f *fakeRunner) addSinkLocked(name string) int {
	f.nextIndex++
	f.sinks = append(f.sinks, fakeSink{Index: f.nextIndex, Name: name})
	return f.nextIndex
}

func (f *fakeRunner) sinkIndexLocked(name string) (int, bool) {
	for _, s := range f.sinks {
		if s.Name == name {
			return s.Index, true
		}
	}
	return 0, false
}

func (f *fakeRunner) sinkNameLocked(idx int) string {
	for _, s := range f.sinks {
		if s.Index == idx {
			return s.Name
		}
	}
	return ""
}

// removeSinkLocked drops the sink with that index and, like PipeWire, moves its streams to
// the default sink.
func (f *fakeRunner) removeSinkLocked(idx int) {
	out := f.sinks[:0]
	for _, s := range f.sinks {
		if s.Index != idx {
			out = append(out, s)
		}
	}
	f.sinks = out
	target, _ := f.sinkIndexLocked(f.def)
	for in, s := range f.inputs {
		if s == idx {
			f.inputs[in] = target
		}
	}
}

func (f *fakeRunner) removeSinkNamedLocked(name string) {
	if idx, ok := f.sinkIndexLocked(name); ok {
		f.removeSinkLocked(idx)
	}
}

func (f *fakeRunner) removeSink(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeSinkNamedLocked(name)
}

func (f *fakeRunner) addInput(idx int, sink string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sinkIndexLocked(sink)
	if !ok {
		panic("no sink " + sink)
	}
	f.inputs[idx] = s
}

func (f *fakeRunner) inputSink(idx int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.inputs[idx]
	if !ok {
		return ""
	}
	return f.sinkNameLocked(s)
}

func (f *fakeRunner) profile(card string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cards[card]
}

func (f *fakeRunner) defaultSink() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.def
}

func (f *fakeRunner) configured() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.confVal, f.confSet
}

func (f *fakeRunner) moduleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.modules)
}

func (f *fakeRunner) called(cmd ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := strings.Join(cmd, " ")
	n := 0
	for _, c := range f.calls {
		if strings.Join(c, " ") == want {
			n++
		}
	}
	return n
}

func (f *fakeRunner) calledPrefix(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := strings.Join(prefix, " ")
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), want) {
			n++
		}
	}
	return n
}

// btSinkFor returns the sink a card exposes for a profile, if any.
func btSinkFor(card, profile string) string {
	if profile == "off" || !strings.HasPrefix(card, "bluez_card.") {
		return ""
	}
	return "bluez_output." + strings.TrimPrefix(card, "bluez_card.") + ".1"
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	cmd := strings.Join(append([]string{name}, args...), " ")
	fail := func() ([]byte, error) { return nil, fmt.Errorf("fake: unsupported %q", cmd) }
	if f.failCmd != "" && cmd == f.failCmd {
		return nil, fmt.Errorf("fake: %q failed", cmd)
	}

	switch name {
	case "pactl":
		switch {
		case cmd == "pactl -f json list cards":
			type card struct {
				Name          string `json:"name"`
				ActiveProfile string `json:"active_profile"`
			}
			out := []card{{Name: "alsa_card.pci-0000_00_1f.3", ActiveProfile: "output:analog-stereo+input:analog-stereo"}}
			for n, p := range f.cards {
				out = append(out, card{Name: n, ActiveProfile: p})
			}
			return json.Marshal(out)
		case cmd == "pactl -f json list sinks":
			return json.Marshal(f.sinks)
		case cmd == "pactl -f json list sink-inputs":
			type input struct {
				Index  int            `json:"index"`
				Sink   int            `json:"sink"`
				Corked bool           `json:"corked"`
				Props  map[string]any `json:"properties"`
			}
			out := []input{}
			for i, s := range f.inputs {
				out = append(out, input{Index: i, Sink: s, Props: map[string]any{}})
			}
			return json.Marshal(out)
		case cmd == "pactl get-default-sink":
			return []byte(f.def + "\n"), nil
		case cmd == "pactl load-module module-null-sink sink_name=rbt_hold sink_properties=device.description=Released-for-handoff":
			f.nextMod++
			f.modules[f.nextMod] = "rbt_hold"
			f.addSinkLocked("rbt_hold")
			f.sinks[len(f.sinks)-1].Owner = f.nextMod
			return []byte(strconv.Itoa(f.nextMod) + "\n"), nil
		case len(args) == 2 && args[0] == "set-default-sink":
			if _, ok := f.sinkIndexLocked(args[1]); !ok {
				return nil, fmt.Errorf("fake: no sink %s", args[1])
			}
			f.def = args[1]
			f.confVal = `{"name":"` + args[1] + `"}`
			f.confSet = true
			return nil, nil
		case len(args) == 3 && args[0] == "move-sink-input":
			idx, err := strconv.Atoi(args[1])
			if err != nil {
				return fail()
			}
			if _, ok := f.inputs[idx]; !ok {
				return nil, fmt.Errorf("fake: no sink-input %d", idx)
			}
			s, ok := f.sinkIndexLocked(args[2])
			if !ok {
				return nil, fmt.Errorf("fake: no sink %s", args[2])
			}
			f.inputs[idx] = s
			return nil, nil
		case len(args) == 3 && args[0] == "set-card-profile":
			card, prof := args[1], args[2]
			old, ok := f.cards[card]
			if !ok {
				return nil, fmt.Errorf("fake: no card %s", card)
			}
			if old == prof {
				return nil, nil
			}
			if s := btSinkFor(card, old); s != "" {
				f.removeSinkNamedLocked(s)
			}
			f.cards[card] = prof
			if s := btSinkFor(card, prof); s != "" {
				f.addSinkLocked(s)
			}
			return nil, nil
		case len(args) == 2 && args[0] == "unload-module":
			id, err := strconv.Atoi(args[1])
			if err != nil {
				return fail()
			}
			if _, ok := f.modules[id]; !ok {
				return nil, fmt.Errorf("fake: no module %d", id)
			}
			delete(f.modules, id)
			for _, s := range f.sinks {
				if s.Owner == id {
					f.removeSinkLocked(s.Index)
					break
				}
			}
			return nil, nil
		}
	case "pw-metadata":
		switch {
		case cmd == "pw-metadata 0 "+metaKey:
			out := "Found \"default\" metadata 40\n"
			if f.confSet {
				out += "update: id:0 key:'" + metaKey + "' value:'" + f.confVal + "' type:'Spa:String:JSON'\n"
			}
			return []byte(out), nil
		case cmd == "pw-metadata -d 0 "+metaKey:
			f.confVal, f.confSet = "", false
			return nil, nil
		case len(args) == 4 && args[0] == "0" && args[1] == metaKey && args[3] == "Spa:String:JSON":
			f.confVal, f.confSet = args[2], true
			return nil, nil
		}
	}
	return fail()
}

// standardFake is the machine from the brief: BT headphones on a2dp-sink,
// Spotify on a filter sink and another stream on the BT sink.
func standardFake(profile string) *fakeRunner {
	f := newFake()
	f.cards[testCard] = profile
	f.addSink("alsa_output.pci-0000_00_1f.3.analog-stereo")
	f.addSink("jamesdsp_sink")
	f.addSink(testBTSink)
	f.def = testBTSink
	f.confVal, f.confSet = testConfig, true
	f.addInput(7, "jamesdsp_sink") // Spotify
	f.addInput(9, testBTSink)      // hog
	return f
}

type changeRec struct {
	mu     sync.Mutex
	events []string
}

func (c *changeRec) fn(addr string, released bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, fmt.Sprintf("%s=%v", addr, released))
}

func (c *changeRec) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func newTestManager(t *testing.T, f *fakeRunner, ch *changeRec) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	var cb func(string, bool)
	if ch != nil {
		cb = ch.fn
	}
	m := New(f, dir, cb)
	m.sinkPoll = 5 * time.Millisecond
	m.sinkWait = 100 * time.Millisecond
	return m, dir
}

func statePath(dir string) string {
	return filepath.Join(dir, "release-00_0A_45_19_F3_A6.json")
}

func TestReleaseAndRestoreRoundTrip(t *testing.T) {
	f := standardFake("a2dp-sink")
	ch := &changeRec{}
	m, dir := newTestManager(t, f, ch)
	ctx := context.Background()

	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if p := f.profile(testCard); p != "off" {
		t.Errorf("profile after release = %q, want off", p)
	}
	if d := f.defaultSink(); d != "rbt_hold" {
		t.Errorf("default after release = %q, want rbt_hold", d)
	}
	for _, in := range []int{7, 9} {
		if s := f.inputSink(in); s != "rbt_hold" {
			t.Errorf("input %d after release on %q, want rbt_hold", in, s)
		}
	}
	if !m.Released(testAddr) {
		t.Error("Released() = false after Release")
	}
	if got := ch.list(); len(got) != 1 || got[0] != testAddr+"=true" {
		t.Errorf("onChange after release = %v", got)
	}
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Errorf("state file: %v", err)
	}

	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile after restore = %q", p)
	}
	if d := f.defaultSink(); d != testBTSink {
		t.Errorf("default after restore = %q", d)
	}
	if s := f.inputSink(7); s != "jamesdsp_sink" {
		t.Errorf("Spotify on %q, want jamesdsp_sink", s)
	}
	if s := f.inputSink(9); s != testBTSink {
		t.Errorf("hog on %q, want %s", s, testBTSink)
	}
	if v, set := f.configured(); !set || v != testConfig {
		t.Errorf("configured = %q,%v, want %q", v, set, testConfig)
	}
	if n := f.moduleCount(); n != 0 {
		t.Errorf("%d modules still loaded", n)
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file still there: %v", err)
	}
	if m.Released(testAddr) {
		t.Error("Released() = true after Restore")
	}
	if got := ch.list(); len(got) != 2 || got[1] != testAddr+"=false" {
		t.Errorf("onChange after restore = %v", got)
	}
}

func TestRestoreUsesPreviousProfile(t *testing.T) {
	f := standardFake("headset-head-unit")
	m, _ := newTestManager(t, f, nil)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatal(err)
	}
	if p := f.profile(testCard); p != "headset-head-unit" {
		t.Errorf("profile = %q, want headset-head-unit", p)
	}
}

func TestRestoreMovesNewAndOrphanedStreamsToDefault(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, _ := newTestManager(t, f, nil)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.addInput(11, "rbt_hold") // started during the hold
	f.removeSink("jamesdsp_sink")
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, in := range []int{7, 11} {
		if s := f.inputSink(in); s != testBTSink {
			t.Errorf("input %d on %q, want %s", in, s, testBTSink)
		}
	}
}

func TestRestoreDeletesUnsetConfiguredSink(t *testing.T) {
	f := standardFake("a2dp-sink")
	f.confVal, f.confSet = "", false
	m, _ := newTestManager(t, f, nil)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatal(err)
	}
	if f.called("pw-metadata", "-d", "0", metaKey) != 1 {
		t.Error("pw-metadata -d not called")
	}
	if _, set := f.configured(); set {
		t.Error("configured sink still set")
	}
}

func TestReleaseWithoutCardChangesNothing(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, dir := newTestManager(t, f, nil)
	err := m.Release(context.Background(), "58:18:62:63:08:B3", time.Minute)
	if !errors.Is(err, ErrNoCard) {
		t.Fatalf("err = %v, want ErrNoCard", err)
	}
	if f.calledPrefix("pactl", "load-module") != 0 {
		t.Error("load-module called")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("state dir not empty: %v", entries)
	}
}

func TestReleaseRejectsBadAddress(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, _ := newTestManager(t, f, nil)
	if err := m.Release(context.Background(), "nope", time.Minute); err == nil {
		t.Fatal("Release accepted a bad address")
	}
	if len(f.calls) != 0 {
		t.Errorf("commands run for bad address: %v", f.calls)
	}
}

func TestReleaseFailsBeforeChangesWhenStateUnwritable(t *testing.T) {
	f := standardFake("a2dp-sink")
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(f, dir, nil) // stateDir is a regular file: cannot be created
	if err := m.Release(context.Background(), testAddr, time.Minute); err == nil {
		t.Fatal("Release succeeded without a state file")
	}
	if f.calledPrefix("pactl", "load-module") != 0 || f.calledPrefix("pactl", "set-") != 0 || f.calledPrefix("pactl", "move-") != 0 {
		t.Errorf("changes made: %v", f.calls)
	}
	if m.Released(testAddr) {
		t.Error("Released() = true")
	}
}

func TestHoldExpiryRestores(t *testing.T) {
	f := standardFake("a2dp-sink")
	ch := &changeRec{}
	m, dir := newTestManager(t, f, ch)
	if err := m.Release(context.Background(), testAddr, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for m.Released(testAddr) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Released flips before the file is removed and onChange runs; wait for both.
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath(dir)); errors.Is(err, os.ErrNotExist) && len(ch.list()) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.Released(testAddr) {
		t.Fatal("still released after hold expired")
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile = %q", p)
	}
	if got := ch.list(); len(got) != 2 || got[1] != testAddr+"=false" {
		t.Errorf("onChange = %v", got)
	}
}

// fakeTimers replaces time.AfterFunc so hold timers fire only when a test
// says so.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { t.stopped = true; return true }

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return t
}

func (ft *fakeTimers) get(i int) *fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.timers[i]
}

func (ft *fakeTimers) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.timers)
}

func TestReleaseTwiceExtendsHold(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, _ := newTestManager(t, f, nil)
	ft := &fakeTimers{}
	m.afterFunc = ft.afterFunc
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, testAddr, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := f.calledPrefix("pactl", "load-module"); n != 1 {
		t.Errorf("load-module called %d times", n)
	}
	if ft.count() != 2 {
		t.Fatalf("%d timers armed, want 2", ft.count())
	}
	first, second := ft.get(0), ft.get(1)
	if !first.stopped {
		t.Error("first hold timer not stopped")
	}
	if second.d != 20*time.Second {
		t.Errorf("second hold = %v, want 20s", second.d)
	}
	first.f() // the first hold's timer firing late must not restore
	if !m.Released(testAddr) {
		t.Fatal("first hold's timer ended the extended hold")
	}
	second.f()
	if m.Released(testAddr) {
		t.Fatal("extended hold's timer did not restore")
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile = %q", p)
	}
}

func TestStaleTimerDoesNotRestoreNewerRelease(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, _ := newTestManager(t, f, nil)
	ft := &fakeTimers{}
	m.afterFunc = ft.afterFunc
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	ft.get(0).f() // first release's timer fires after the second release
	if !m.Released(testAddr) {
		t.Fatal("first release's timer restored the second release")
	}
	if p := f.profile(testCard); p != "off" {
		t.Errorf("profile = %q, want off", p)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreWhenNotReleasedIsNoop(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, _ := newTestManager(t, f, nil)
	if err := m.Restore(context.Background(), testAddr); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("commands run: %v", f.calls)
	}
}

func TestRecoverRestoresFromStateFile(t *testing.T) {
	f := standardFake("a2dp-sink")
	// Simulate a crash mid-hold: profile off, null sink loaded, streams parked.
	if _, err := f.Run(context.Background(), "pactl", "load-module", "module-null-sink", "sink_name=rbt_hold", "sink_properties=device.description=Released-for-handoff"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.def = "rbt_hold"
	f.mu.Unlock()
	if _, err := f.Run(context.Background(), "pactl", "move-sink-input", "7", "rbt_hold"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(context.Background(), "pactl", "set-card-profile", testCard, "off"); err != nil {
		t.Fatal(err)
	}
	if s := f.inputSink(7); s != "rbt_hold" {
		t.Fatalf("setup: Spotify on %q, want rbt_hold", s)
	}
	f.calls = nil

	dir := t.TempDir()
	st := `{"address":"` + testAddr + `","card":"` + testCard + `","profile":"a2dp-sink",` +
		`"default_sink":"` + testBTSink + `","configured_sink":` + strconv.Quote(testConfig) + `,` +
		`"configured_set":true,"module":"501","streams":{"7":"jamesdsp_sink","9":"` + testBTSink + `"}}`
	if err := os.WriteFile(statePath(dir), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := &changeRec{}
	m := New(f, dir, ch.fn)
	m.sinkPoll = 5 * time.Millisecond
	m.sinkWait = 100 * time.Millisecond
	if err := m.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile = %q", p)
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file still there: %v", err)
	}
	if f.moduleCount() != 0 {
		t.Error("module still loaded")
	}
	if s := f.inputSink(7); s != "jamesdsp_sink" {
		t.Errorf("Spotify on %q", s)
	}
	if d := f.defaultSink(); d != testBTSink {
		t.Errorf("default = %q", d)
	}
}

func TestCloseRestoresAll(t *testing.T) {
	f := standardFake("a2dp-sink")
	ch := &changeRec{}
	m, dir := newTestManager(t, f, ch)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile = %q", p)
	}
	if d := f.defaultSink(); d != testBTSink {
		t.Errorf("default = %q", d)
	}
	if m.Released(testAddr) {
		t.Error("still released after Close")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("state files left: %v", entries)
	}
	if f.moduleCount() != 0 {
		t.Error("module still loaded")
	}
	if got := ch.list(); len(got) != 2 || got[1] != testAddr+"=false" {
		t.Errorf("onChange = %v", got)
	}
	if err := m.Release(ctx, testAddr, time.Minute); !errors.Is(err, ErrClosed) {
		t.Errorf("Release after Close = %v, want ErrClosed", err)
	}
}

func TestReleaseOtherDeviceIsBusy(t *testing.T) {
	const addr2 = "58:18:62:63:08:B3"
	const card2 = "bluez_card.58_18_62_63_08_B3"
	f := standardFake("a2dp-sink")
	f.mu.Lock()
	f.cards[card2] = "a2dp-sink"
	f.addSinkLocked("bluez_output.58_18_62_63_08_B3.1")
	f.mu.Unlock()
	m, dir := newTestManager(t, f, nil)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	if err := m.Release(ctx, addr2, time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("second device: err = %v, want ErrBusy", err)
	}
	f.mu.Lock()
	after := len(f.calls)
	f.mu.Unlock()
	if after != before {
		t.Errorf("busy Release ran %d commands", after-before)
	}
	if m.Released(addr2) {
		t.Error("second device released")
	}
	if p := f.profile(card2); p != "a2dp-sink" {
		t.Errorf("second card profile = %q", p)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("state files: %v", entries)
	}
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Errorf("same device again: %v", err)
	}
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, addr2, time.Minute); err != nil {
		t.Errorf("second device after restore: %v", err)
	}
}

func TestParseConfiguredSink(t *testing.T) {
	out := "Found \"default\" metadata 40\n" +
		"update: id:0 key:'default.configured.audio.sink' value:'{\"name\":\"bluez_output.58_18_62_63_08_B3.1\"}' type:'Spa:String:JSON'\n"
	v, set := parseConfiguredSink([]byte(out))
	if !set || v != `{"name":"bluez_output.58_18_62_63_08_B3.1"}` {
		t.Errorf("got %q,%v", v, set)
	}
	if _, set := parseConfiguredSink([]byte("Found \"default\" metadata 40\n")); set {
		t.Error("unset key reported as set")
	}
}

func TestReleaseRollsBackWhenProfileChangeFails(t *testing.T) {
	f := standardFake("a2dp-sink")
	f.failCmd = "pactl set-card-profile " + testCard + " off"
	m, dir := newTestManager(t, f, nil)
	if err := m.Release(context.Background(), testAddr, time.Minute); err == nil {
		t.Fatal("Release succeeded")
	}
	if m.Released(testAddr) {
		t.Error("Released() = true")
	}
	if d := f.defaultSink(); d != testBTSink {
		t.Errorf("default = %q", d)
	}
	if s := f.inputSink(7); s != "jamesdsp_sink" {
		t.Errorf("Spotify on %q", s)
	}
	if s := f.inputSink(9); s != testBTSink {
		t.Errorf("hog on %q", s)
	}
	if v, set := f.configured(); !set || v != testConfig {
		t.Errorf("configured = %q,%v", v, set)
	}
	if f.moduleCount() != 0 {
		t.Error("module still loaded")
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file left: %v", err)
	}
}

func TestRestoreWhenCardGone(t *testing.T) {
	f := standardFake("a2dp-sink")
	ch := &changeRec{}
	m, dir := newTestManager(t, f, ch)
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.cards, testCard) // headphones disconnected during the hold
	f.mu.Unlock()
	_ = m.Restore(ctx, testAddr) // default sink is gone too, so errors are expected
	if m.Released(testAddr) {
		t.Error("still released")
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file left: %v", err)
	}
	if f.moduleCount() != 0 {
		t.Error("module still loaded")
	}
	if got := ch.list(); len(got) != 2 {
		t.Errorf("onChange = %v", got)
	}
}

func TestRestoreKeepsReleaseWhenProfileFails(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, dir := newTestManager(t, f, nil)
	m.retryDelay = 20 * time.Millisecond
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failCmd = "pactl set-card-profile " + testCard + " a2dp-sink"
	f.mu.Unlock()
	if err := m.Restore(ctx, testAddr); err == nil {
		t.Fatal("Restore reported success")
	}
	if !m.Released(testAddr) {
		t.Fatal("dropped a release whose profile was never restored")
	}
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Errorf("state file: %v", err)
	}
	f.mu.Lock()
	f.failCmd = ""
	f.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for m.Released(testAddr) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("retry did not restore profile: %q", p)
	}
}

// cancelRunner fails every command once its context is done, like
// exec.CommandContext, and cancels the test's context right after cancelAfter
// has run.
type cancelRunner struct {
	*fakeRunner
	cancelAfter string
	cancel      context.CancelFunc
}

func (c *cancelRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := c.fakeRunner.Run(ctx, name, args...)
	if c.cancelAfter != "" && strings.Join(append([]string{name}, args...), " ") == c.cancelAfter {
		c.cancel()
	}
	return out, err
}

func assertFullyRestored(t *testing.T, f *fakeRunner, m *Manager, dir string) {
	t.Helper()
	if m.Released(testAddr) {
		t.Error("still released")
	}
	if p := f.profile(testCard); p != "a2dp-sink" {
		t.Errorf("profile = %q", p)
	}
	if d := f.defaultSink(); d != testBTSink {
		t.Errorf("default = %q", d)
	}
	if s := f.inputSink(7); s != "jamesdsp_sink" {
		t.Errorf("Spotify on %q", s)
	}
	if s := f.inputSink(9); s != testBTSink {
		t.Errorf("hog on %q", s)
	}
	if v, set := f.configured(); !set || v != testConfig {
		t.Errorf("configured = %q,%v", v, set)
	}
	if f.moduleCount() != 0 {
		t.Error("module still loaded")
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file left: %v", err)
	}
}

func TestRestoreWithCancelledContextCompletes(t *testing.T) {
	f := standardFake("a2dp-sink")
	r := &cancelRunner{fakeRunner: f}
	dir := t.TempDir()
	m := New(r, dir, nil)
	m.sinkPoll = 5 * time.Millisecond
	m.sinkWait = 100 * time.Millisecond
	if err := m.Release(context.Background(), testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Restore(ctx, testAddr); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertFullyRestored(t, f, m, dir)
}

func TestRestoreSurvivesCancelAfterProfileStep(t *testing.T) {
	f := standardFake("a2dp-sink")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelRunner{fakeRunner: f, cancel: cancel}
	dir := t.TempDir()
	m := New(r, dir, nil)
	m.sinkPoll = 5 * time.Millisecond
	m.sinkWait = 100 * time.Millisecond
	if err := m.Release(context.Background(), testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	r.cancelAfter = "pactl set-card-profile " + testCard + " a2dp-sink"
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test setup: context was never cancelled")
	}
	assertFullyRestored(t, f, m, dir)
}

func TestRestoreKeepsReleaseWhenDefaultSinkFails(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, dir := newTestManager(t, f, nil)
	ft := &fakeTimers{}
	m.afterFunc = ft.afterFunc
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failCmd = "pactl set-default-sink " + testBTSink
	f.mu.Unlock()
	if err := m.Restore(ctx, testAddr); err == nil {
		t.Fatal("Restore reported success")
	}
	if !m.Released(testAddr) {
		t.Fatal("dropped a release whose default sink was never restored")
	}
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Errorf("state file: %v", err)
	}
	retry := ft.get(ft.count() - 1)
	if retry.d != m.retryDelay {
		t.Errorf("retry armed for %v, want %v", retry.d, m.retryDelay)
	}
	f.mu.Lock()
	f.failCmd = ""
	f.mu.Unlock()
	retry.f()
	assertFullyRestored(t, f, m, dir)
}

func TestRestoreKeepsReleaseWhenUnloadFails(t *testing.T) {
	f := standardFake("a2dp-sink")
	m, dir := newTestManager(t, f, nil)
	ft := &fakeTimers{}
	m.afterFunc = ft.afterFunc
	ctx := context.Background()
	if err := m.Release(ctx, testAddr, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failCmd = "pactl unload-module 501"
	f.mu.Unlock()
	if err := m.Restore(ctx, testAddr); err == nil {
		t.Fatal("Restore reported success")
	}
	if !m.Released(testAddr) {
		t.Fatal("dropped a release whose hold sink is still loaded")
	}
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Errorf("state file: %v", err)
	}
	f.mu.Lock()
	f.failCmd = ""
	f.mu.Unlock()
	ft.get(ft.count() - 1).f()
	assertFullyRestored(t, f, m, dir)
}
