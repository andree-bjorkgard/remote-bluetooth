// Package audio lets go of Bluetooth headphones so another machine can
// play on them, and puts this machine's audio back afterwards. It turns the
// headphones' PipeWire card profile off with pactl, parks the streams that
// were playing on a null sink, and later restores the card profile, default
// sink, every stream's sink and default.configured.audio.sink exactly.
package audio

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
)

// ErrNoCard is returned by Release when PipeWire has no card for the device.
var ErrNoCard = errors.New("audio: no bluetooth audio card for device")

// ErrClosed is returned by Release after Close.
var ErrClosed = errors.New("audio: manager closed")

const (
	defaultSinkPoll   = 100 * time.Millisecond
	defaultSinkWait   = 5 * time.Second
	defaultRetryDelay = 5 * time.Second
)

// Manager releases and restores Bluetooth audio cards. It is safe for
// concurrent use.
type Manager struct {
	p        pactl
	dir      string
	onChange func(address string, released bool)

	sinkPoll   time.Duration // how often Restore polls for the Bluetooth sink
	sinkWait   time.Duration // how long Restore waits for it
	retryDelay time.Duration // when to retry a restore whose profile change failed

	// ops serializes everything that changes the audio system, so a
	// timer-driven restore never interleaves with Release or Restore.
	ops sync.Mutex

	mu     sync.Mutex // guards the fields below
	held   map[string]*heldRelease
	gen    uint64 // bumped for every timer armed; stale timers see a newer gen
	seq    uint64 // release order, for restoring in reverse
	closed bool
}

type heldRelease struct {
	st    *releaseState
	timer *time.Timer
	gen   uint64
	seq   uint64
}

// New returns a Manager that runs commands with r and keeps its state files
// in stateDir. onChange, if not nil, is called whenever a device becomes
// released or restored; it is called without the Manager's locks held.
func New(r Runner, stateDir string, onChange func(address string, released bool)) *Manager {
	return &Manager{
		p:          pactl{r: r},
		dir:        stateDir,
		onChange:   onChange,
		sinkPoll:   defaultSinkPoll,
		sinkWait:   defaultSinkWait,
		retryDelay: defaultRetryDelay,
		held:       map[string]*heldRelease{},
	}
}

func underscored(addr string) string { return strings.ReplaceAll(addr, ":", "_") }

func (m *Manager) notify(addr string, released bool) {
	if m.onChange != nil {
		m.onChange(addr, released)
	}
}

// armLocked (re)starts h's timer. m.mu must be held.
func (m *Manager) armLocked(addr string, h *heldRelease, d time.Duration) {
	if h.timer != nil {
		h.timer.Stop()
	}
	m.gen++
	gen := m.gen
	h.gen = gen
	h.timer = time.AfterFunc(d, func() { m.expire(addr, gen) })
}

// Release lets go of the device's audio for hold, after which it is restored
// automatically. Releasing a device that is already released only restarts
// its hold.
func (m *Manager) Release(ctx context.Context, address string, hold time.Duration) error {
	addr, err := bluez.NormalizeAddress(address)
	if err != nil {
		return err
	}
	if hold <= 0 {
		return fmt.Errorf("audio: hold must be positive, got %v", hold)
	}

	m.ops.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.ops.Unlock()
		return ErrClosed
	}
	if h, ok := m.held[addr]; ok {
		m.armLocked(addr, h, hold)
		m.mu.Unlock()
		m.ops.Unlock()
		return nil
	}
	m.mu.Unlock()

	st, err := m.release(ctx, addr)
	if err != nil {
		m.ops.Unlock()
		return err
	}
	m.mu.Lock()
	m.seq++
	h := &heldRelease{st: st, seq: m.seq}
	m.held[addr] = h
	m.armLocked(addr, h, hold)
	m.mu.Unlock()
	m.ops.Unlock()

	m.notify(addr, true)
	return nil
}

// release does the work of Release. On failure after the first change it
// puts everything back before returning. m.ops must be held.
func (m *Manager) release(ctx context.Context, addr string) (*releaseState, error) {
	under := underscored(addr)
	cardName := "bluez_card." + under

	cards, err := m.p.cards(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(cards, func(c card) bool { return c.Name == cardName })
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoCard, addr)
	}
	profile := cards[i].ActiveProfile

	defSink, err := m.p.defaultSink(ctx)
	if err != nil {
		return nil, err
	}
	confVal, confSet, err := m.p.configuredSink(ctx)
	if err != nil {
		return nil, err
	}
	inputs, err := m.p.sinkInputs(ctx)
	if err != nil {
		return nil, err
	}
	sinks, err := m.p.sinks(ctx)
	if err != nil {
		return nil, err
	}
	sinkNames := make(map[int]string, len(sinks))
	for _, s := range sinks {
		sinkNames[s.Index] = s.Name
	}
	streams := map[string]string{}
	for _, in := range inputs {
		if name, ok := sinkNames[in.Sink]; ok {
			streams[strconv.Itoa(in.Index)] = name
		}
	}

	st := &releaseState{
		Address:        addr,
		Card:           cardName,
		Profile:        profile,
		DefaultSink:    defSink,
		ConfiguredSink: confVal,
		ConfiguredSet:  confSet,
		Streams:        streams,
	}
	path := stateFile(m.dir, under)
	if err := writeState(path, st); err != nil {
		return nil, err
	}

	id, err := m.p.loadHoldSink(ctx)
	if err != nil {
		return nil, errors.Join(err, removeState(path))
	}
	st.Module = id
	if err := writeState(path, st); err != nil {
		bg := context.WithoutCancel(ctx)
		return nil, errors.Join(err, m.p.unloadModule(bg, id), removeState(path))
	}

	moves := make(map[int]string, len(streams))
	for _, in := range inputs {
		if _, ok := streams[strconv.Itoa(in.Index)]; ok {
			moves[in.Index] = holdSinkName
		}
	}
	err = m.p.setDefaultSink(ctx, holdSinkName)
	if err == nil {
		err = m.moveStreams(ctx, moves)
	}
	if err == nil {
		err = m.p.setCardProfile(ctx, cardName, "off")
	}
	if err != nil {
		_, rerr := m.restore(context.WithoutCancel(ctx), st)
		return nil, errors.Join(err, rerr)
	}
	return st, nil
}

// moveStreams moves each sink-input to its sink. A stream that ended in the
// meantime is not an error.
func (m *Manager) moveStreams(ctx context.Context, moves map[int]string) error {
	failed := map[int]error{}
	for idx, sinkName := range moves {
		if err := m.p.moveSinkInput(ctx, idx, sinkName); err != nil {
			failed[idx] = err
		}
	}
	if len(failed) == 0 {
		return nil
	}
	inputs, err := m.p.sinkInputs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range inputs {
		if err, ok := failed[in.Index]; ok {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Restore puts the device's audio back as it was before Release. It does
// nothing if the device is not released.
func (m *Manager) Restore(ctx context.Context, address string) error {
	addr, err := bluez.NormalizeAddress(address)
	if err != nil {
		return err
	}
	m.ops.Lock()
	m.mu.Lock()
	h, ok := m.held[addr]
	if ok {
		h.timer.Stop()
	}
	m.mu.Unlock()
	if !ok {
		m.ops.Unlock()
		return nil
	}
	restored, err := m.finishRestore(ctx, addr, h)
	m.ops.Unlock()
	if restored {
		m.notify(addr, false)
	}
	return err
}

// expire is the hold timer. It restores the device unless the release it
// was armed for has since been restored, extended or replaced.
func (m *Manager) expire(addr string, gen uint64) {
	m.ops.Lock()
	m.mu.Lock()
	h, ok := m.held[addr]
	stale := !ok || h.gen != gen
	m.mu.Unlock()
	if stale {
		m.ops.Unlock()
		return
	}
	restored, _ := m.finishRestore(context.Background(), addr, h)
	m.ops.Unlock()
	if restored {
		m.notify(addr, false)
	}
}

// finishRestore restores h and forgets it. If the card profile could not be
// put back, the device stays released and a retry is scheduled. m.ops must
// be held.
func (m *Manager) finishRestore(ctx context.Context, addr string, h *heldRelease) (bool, error) {
	ok, err := m.restore(ctx, h.st)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ok {
		if !m.closed {
			m.armLocked(addr, h, m.retryDelay)
		}
		return false, err
	}
	delete(m.held, addr)
	return true, err
}

// restore puts st back. It carries on past failures and returns them all.
// done is false when the card profile could not be restored; the state file
// is then kept so the restore can be retried. m.ops must be held.
func (m *Manager) restore(ctx context.Context, st *releaseState) (done bool, err error) {
	var errs []error
	under := underscored(st.Address)

	profileErr := m.p.setCardProfile(ctx, st.Card, st.Profile)
	cardGone := false
	if profileErr != nil && m.cardGone(ctx, st.Card) {
		// The headphones went away; there is no profile left to put back.
		profileErr, cardGone = nil, true
	}
	errs = append(errs, profileErr)

	var sinks []sink
	if profileErr == nil && !cardGone && st.Profile != "off" {
		sinks = m.waitForSink(ctx, "bluez_output."+under+".")
	} else {
		var lerr error
		sinks, lerr = m.p.sinks(ctx)
		errs = append(errs, lerr)
	}
	hasSink := func(name string) bool {
		return slices.ContainsFunc(sinks, func(s sink) bool { return s.Name == name })
	}

	if st.DefaultSink != "" {
		errs = append(errs, m.p.setDefaultSink(ctx, st.DefaultSink))
	}

	// Streams go back to their saved sinks. Saved streams whose sink is gone,
	// and streams that started on the hold sink, go to the default sink.
	fallback := ""
	if hasSink(st.DefaultSink) {
		fallback = st.DefaultSink
	}
	holdIdx := map[int]bool{}
	for _, s := range sinks {
		if s.Name == holdSinkName {
			holdIdx[s.Index] = true
		}
	}
	inputs, ierr := m.p.sinkInputs(ctx)
	errs = append(errs, ierr)
	moves := map[int]string{}
	for _, in := range inputs {
		target := ""
		if saved, ok := st.Streams[strconv.Itoa(in.Index)]; ok {
			target = fallback
			if hasSink(saved) {
				target = saved
			}
		} else if holdIdx[in.Sink] {
			target = fallback
		}
		if target != "" {
			moves[in.Index] = target
		}
	}
	errs = append(errs, m.moveStreams(ctx, moves))

	if st.ConfiguredSet {
		errs = append(errs, m.p.setConfiguredSink(ctx, st.ConfiguredSink))
	} else {
		errs = append(errs, m.p.deleteConfiguredSink(ctx))
	}

	if st.Module != "" && m.ownsHoldSink(sinks, st.Module) {
		errs = append(errs, m.p.unloadModule(ctx, st.Module))
	}

	if profileErr != nil {
		return false, errors.Join(errs...)
	}
	errs = append(errs, removeState(stateFile(m.dir, under)))
	return true, errors.Join(errs...)
}

// cardGone reports whether PipeWire positively has no card by that name.
func (m *Manager) cardGone(ctx context.Context, name string) bool {
	cards, err := m.p.cards(ctx)
	return err == nil && !slices.ContainsFunc(cards, func(c card) bool { return c.Name == name })
}

// ownsHoldSink reports whether module is the one that created a hold sink,
// so a module id reused after a PipeWire restart is never unloaded. If pactl
// does not report sink owners, any hold sink counts.
func (m *Manager) ownsHoldSink(sinks []sink, module string) bool {
	for _, s := range sinks {
		if s.Name == holdSinkName && (s.owner() == "" || s.owner() == module) {
			return true
		}
	}
	return false
}

// waitForSink polls the sink list until a sink with prefix appears or
// m.sinkWait passes, and returns the last list it got.
func (m *Manager) waitForSink(ctx context.Context, prefix string) []sink {
	deadline := time.Now().Add(m.sinkWait)
	for {
		sinks, err := m.p.sinks(ctx)
		if err == nil && slices.ContainsFunc(sinks, func(s sink) bool { return strings.HasPrefix(s.Name, prefix) }) {
			return sinks
		}
		if time.Now().After(deadline) {
			return sinks
		}
		select {
		case <-ctx.Done():
			return sinks
		case <-time.After(m.sinkPoll):
		}
	}
}

// Released reports whether the device's audio is currently released.
func (m *Manager) Released(address string) bool {
	addr, err := bluez.NormalizeAddress(address)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[addr]
	return ok
}

// Recover restores every release recorded in the state directory, such as
// those left behind by a crash.
func (m *Manager) Recover(ctx context.Context) error {
	paths, err := filepath.Glob(filepath.Join(m.dir, statePattern))
	if err != nil {
		return err
	}
	m.ops.Lock()
	defer m.ops.Unlock()
	var errs []error
	for _, path := range paths {
		st, err := readState(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		addr, err := bluez.NormalizeAddress(st.Address)
		if err != nil || stateFile(m.dir, underscored(addr)) != path {
			errs = append(errs, fmt.Errorf("audio: %s: address %q does not match file", path, st.Address))
			continue
		}
		m.mu.Lock()
		_, held := m.held[addr]
		m.mu.Unlock()
		if held {
			continue
		}
		_, err = m.restore(ctx, st)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close restores every device that is still released, most recent first,
// and refuses further releases.
func (m *Manager) Close(ctx context.Context) error {
	m.ops.Lock()
	m.mu.Lock()
	m.closed = true
	type entry struct {
		addr string
		h    *heldRelease
	}
	var entries []entry
	for addr, h := range m.held {
		h.timer.Stop()
		entries = append(entries, entry{addr, h})
	}
	m.mu.Unlock()
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(b.h.seq, a.h.seq) })

	var errs []error
	var restored []string
	for _, e := range entries {
		ok, err := m.finishRestore(ctx, e.addr, e.h)
		errs = append(errs, err)
		if ok {
			restored = append(restored, e.addr)
		}
	}
	m.ops.Unlock()
	for _, addr := range restored {
		m.notify(addr, false)
	}
	return errors.Join(errs...)
}
