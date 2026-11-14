// Package fakebt is an in-memory Bluetooth backend for tests.
package fakebt

import (
	"context"
	"sync"

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
