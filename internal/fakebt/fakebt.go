// Package fakebt is an in-memory Bluetooth backend for tests.
package fakebt

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
)

// Backend is an in-memory server.Backend. Connect and Disconnect flip the
// device's Connected flag and report it as a change, like BlueZ does.
type Backend struct {
	mu      sync.Mutex
	devices map[string]bluez.Device
	changes chan bluez.Change
	calls   []string
}

// New returns a backend that knows devs.
func New(devs ...bluez.Device) *Backend {
	f := &Backend{devices: map[string]bluez.Device{}, changes: make(chan bluez.Change, 16)}
	for _, d := range devs {
		f.devices[d.Address] = d
	}
	return f
}

func (f *Backend) Watch(ctx context.Context) ([]bluez.Device, <-chan bluez.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var devs []bluez.Device
	for _, d := range f.devices {
		devs = append(devs, d)
	}
	return devs, f.changes, nil
}

func (f *Backend) set(addr string, connected bool, op string) error {
	f.mu.Lock()
	f.calls = append(f.calls, op+" "+addr)
	d, ok := f.devices[addr]
	if !ok {
		f.mu.Unlock()
		return bluez.ErrNotFound
	}
	d.Connected = connected
	f.devices[addr] = d
	f.mu.Unlock()
	f.changes <- bluez.Change{Device: d}
	return nil
}

func (f *Backend) Connect(_ context.Context, addr string) error {
	return f.set(addr, true, "connect")
}

func (f *Backend) Disconnect(_ context.Context, addr string) error {
	return f.set(addr, false, "disconnect")
}

// Calls returns the connect/disconnect calls made so far.
func (f *Backend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Changes exposes the change channel so tests can flood it.
func (f *Backend) Changes() chan<- bluez.Change { return f.changes }

// Emit pushes a change as if BlueZ reported it.
func (f *Backend) Emit(c bluez.Change) {
	f.mu.Lock()
	if c.Removed {
		delete(f.devices, c.Device.Address)
	} else {
		f.devices[c.Device.Address] = c.Device
	}
	f.mu.Unlock()
	f.changes <- c
}

// Audio is an in-memory server.Audio. It records calls as
// "release <ADDR> <hold>" and "restore <ADDR>" and reports changes through
// OnChange like the real audio manager.
type Audio struct {
	// FailRelease, when set, is returned by Release.
	FailRelease error
	// OnChange is called when a device becomes released or restored.
	OnChange func(address string, released bool)

	mu       sync.Mutex
	calls    []string
	released map[string]bool
}

// NewAudio returns an Audio with nothing released.
func NewAudio() *Audio { return &Audio{released: map[string]bool{}} }

func (a *Audio) Release(_ context.Context, addr string, hold time.Duration) error {
	a.mu.Lock()
	a.calls = append(a.calls, fmt.Sprintf("release %s %s", addr, hold))
	if a.FailRelease != nil {
		a.mu.Unlock()
		return a.FailRelease
	}
	a.released[addr] = true
	a.mu.Unlock()
	a.notify(addr, true)
	return nil
}

func (a *Audio) Restore(_ context.Context, addr string) error {
	a.mu.Lock()
	a.calls = append(a.calls, "restore "+addr)
	delete(a.released, addr)
	a.mu.Unlock()
	a.notify(addr, false)
	return nil
}

func (a *Audio) Released(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.released[addr]
}

func (a *Audio) notify(addr string, released bool) {
	if a.OnChange != nil {
		a.OnChange(addr, released)
	}
}

// Calls returns the release/restore calls made so far.
func (a *Audio) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}
