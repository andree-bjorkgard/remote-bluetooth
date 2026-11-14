package bluez

import (
	"context"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Change is one device update from Watch.
type Change struct {
	// Device is the device's state after the change. On removal only
	// Address is set.
	Device Device
	// Removed is true when BlueZ forgot the device.
	Removed bool
}

// Watch returns every device the adapter knows and a channel of changes
// after that snapshot. The channel closes when ctx is done or the D-Bus
// connection closes.
func (a *Adapter) Watch(ctx context.Context) ([]Device, <-chan Change, error) {
	rules := [][]dbus.MatchOption{
		{dbus.WithMatchSender(service), dbus.WithMatchInterface(objectManager), dbus.WithMatchObjectPath("/")},
		{dbus.WithMatchSender(service), dbus.WithMatchInterface(propertiesIface), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchPathNamespace(a.path)},
	}
	for i, r := range rules {
		if err := a.conn.AddMatchSignalContext(ctx, r...); err != nil {
			for _, added := range rules[:i] {
				_ = a.conn.RemoveMatchSignal(added...)
			}
			return nil, nil, fmt.Errorf("bluez: watch: %w", err)
		}
	}
	sigs := make(chan *dbus.Signal, 64)
	a.conn.Signal(sigs)

	cleanup := func() {
		a.conn.RemoveSignal(sigs)
		for _, r := range rules {
			_ = a.conn.RemoveMatchSignal(r...)
		}
	}

	// Subscribe before taking the snapshot so no change falls in between.
	initial, err := a.Devices(ctx)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	w := newWatcher(a.path)
	for _, d := range initial {
		w.devices[a.path+"/dev_"+dbus.ObjectPath(strings.ReplaceAll(d.Address, ":", "_"))] = d
	}
	out := make(chan Change, 16)
	go func() {
		defer close(out)
		defer cleanup()
		send := func(c Change) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigs:
				if !ok {
					return
				}
				for _, c := range w.handle(sig) {
					if !send(c) {
						return
					}
				}
			}
		}
	}()
	return initial, out, nil
}

// watcher keeps the device state for Watch and turns BlueZ signals into
// changes. It has no D-Bus connection so it can be tested on its own.
type watcher struct {
	adapter *Adapter // only for isDevicePath
	devices map[dbus.ObjectPath]Device
}

func newWatcher(adapterPath dbus.ObjectPath) *watcher {
	return &watcher{adapter: &Adapter{path: adapterPath}, devices: map[dbus.ObjectPath]Device{}}
}

func (w *watcher) handle(sig *dbus.Signal) []Change {
	switch sig.Name {
	case objectManager + ".InterfacesAdded":
		var path dbus.ObjectPath
		var ifaces map[string]map[string]dbus.Variant
		if dbus.Store(sig.Body, &path, &ifaces) != nil || !w.adapter.isDevicePath(path) {
			return nil
		}
		d, known := w.devices[path]
		changed := false
		if props, ok := ifaces[deviceIface]; ok {
			changed = applyDeviceProps(&d, props) || !known
			known = true
		}
		if !known {
			return nil // battery for a device we have not seen yet
		}
		if props, ok := ifaces[batteryIface]; ok {
			changed = applyBattery(&d, props) || changed
		}
		w.devices[path] = d
		if changed {
			return []Change{{Device: d}}
		}

	case objectManager + ".InterfacesRemoved":
		var path dbus.ObjectPath
		var ifaces []string
		if dbus.Store(sig.Body, &path, &ifaces) != nil {
			return nil
		}
		d, known := w.devices[path]
		if !known {
			return nil
		}
		for _, iface := range ifaces {
			switch iface {
			case deviceIface:
				delete(w.devices, path)
				return []Change{{Device: Device{Address: d.Address}, Removed: true}}
			case batteryIface:
				if d.HasBattery {
					d.HasBattery, d.BatteryPercent = false, 0
					w.devices[path] = d
					return []Change{{Device: d}}
				}
			}
		}

	case propertiesIface + ".PropertiesChanged":
		var iface string
		var changed map[string]dbus.Variant
		var invalidated []string
		if dbus.Store(sig.Body, &iface, &changed, &invalidated) != nil {
			return nil
		}
		d, known := w.devices[sig.Path]
		if !known {
			return nil
		}
		var diff bool
		switch iface {
		case deviceIface:
			diff = applyDeviceProps(&d, changed)
		case batteryIface:
			diff = applyBattery(&d, changed)
		}
		if diff {
			w.devices[sig.Path] = d
			return []Change{{Device: d}}
		}
	}
	return nil
}
