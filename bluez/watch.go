package bluez

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
	objs, err := managedObjects(ctx, a.conn)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	w := newWatcher(a.path)
	var initial []Device
	for path, d := range a.devicesFromObjects(objs) {
		w.devices[path] = d
		initial = append(initial, d)
	}
	sort.Slice(initial, func(i, j int) bool { return initial[i].Address < initial[j].Address })
	for path, ifaces := range objs {
		if props, ok := ifaces[transportIface]; ok {
			w.transports[path], _ = props["State"].Value().(string)
		}
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
	// transports holds the State of each MediaTransport1 by object path.
	transports map[dbus.ObjectPath]string
}

func newWatcher(adapterPath dbus.ObjectPath) *watcher {
	return &watcher{adapter: &Adapter{path: adapterPath}, devices: map[dbus.ObjectPath]Device{}, transports: map[dbus.ObjectPath]string{}}
}

// transportDevice returns the device path a transport path belongs to.
func (w *watcher) transportDevice(path dbus.ObjectPath) (dbus.ObjectPath, bool) {
	rest, ok := strings.CutPrefix(string(path), string(w.adapter.path)+"/")
	if !ok {
		return "", false
	}
	dev, _, found := strings.Cut(rest, "/")
	if !found || !strings.HasPrefix(dev, "dev_") {
		return "", false
	}
	return w.adapter.path + "/" + dbus.ObjectPath(dev), true
}

// audio computes the audio state of the device at dev from its transports.
func (w *watcher) audio(dev dbus.ObjectPath) AudioState {
	var states []string
	for p, st := range w.transports {
		if strings.HasPrefix(string(p), string(dev)+"/") {
			states = append(states, st)
		}
	}
	return audioFromStates(states)
}

// updateTransport records (or, with remove, forgets) a transport and returns
// a change if the owning device's audio state differs.
func (w *watcher) updateTransport(path dbus.ObjectPath, state string, remove bool) []Change {
	if remove {
		delete(w.transports, path)
	} else {
		w.transports[path] = state
	}
	dp, ok := w.transportDevice(path)
	if !ok {
		return nil
	}
	d, known := w.devices[dp]
	if !known {
		return nil
	}
	if a := w.audio(dp); a != d.Audio {
		d.Audio = a
		w.devices[dp] = d
		return []Change{{Device: d}}
	}
	return nil
}

func (w *watcher) handle(sig *dbus.Signal) []Change {
	switch sig.Name {
	case objectManager + ".InterfacesAdded":
		var path dbus.ObjectPath
		var ifaces map[string]map[string]dbus.Variant
		if dbus.Store(sig.Body, &path, &ifaces) != nil {
			return nil
		}
		if props, ok := ifaces[transportIface]; ok {
			st, _ := props["State"].Value().(string)
			return w.updateTransport(path, st, false)
		}
		if !w.adapter.isDevicePath(path) {
			return nil
		}
		d, known := w.devices[path]
		changed := false
		if props, ok := ifaces[deviceIface]; ok {
			changed = applyDeviceProps(&d, props) || !known
			known = true
			d.Audio = w.audio(path)
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
		if slices.Contains(ifaces, transportIface) {
			return w.updateTransport(path, "", true)
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
		if iface == transportIface {
			if st, ok := changed["State"]; ok {
				s, _ := st.Value().(string)
				return w.updateTransport(sig.Path, s, false)
			}
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
