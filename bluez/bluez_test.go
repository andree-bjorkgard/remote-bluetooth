package bluez

import (
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestNormalizeAddress(t *testing.T) {
	got, err := NormalizeAddress(" aa:bb:cc:dd:ee:0f ")
	if err != nil || got != "AA:BB:CC:DD:EE:0F" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "AA:BB:CC:DD:EE", "AA:BB:CC:DD:EE:FF/../x", "GG:BB:CC:DD:EE:FF"} {
		if _, err := NormalizeAddress(bad); !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("%q: want ErrInvalidAddress, got %v", bad, err)
		}
	}
}

func TestIsDevicePath(t *testing.T) {
	a := &Adapter{path: "/org/bluez/hci0"}
	cases := map[dbus.ObjectPath]bool{
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF":             true,
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service0001": false,
		"/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF":             false,
		"/org/bluez/hci0":                                   false,
	}
	for p, want := range cases {
		if got := a.isDevicePath(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}

const devPath = dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")

func sig(name string, path dbus.ObjectPath, body ...any) *dbus.Signal {
	return &dbus.Signal{Name: name, Path: path, Body: body}
}

func added(ifaces map[string]map[string]dbus.Variant) *dbus.Signal {
	return sig(objectManager+".InterfacesAdded", "/", devPath, ifaces)
}

func propsChanged(iface string, props map[string]dbus.Variant) *dbus.Signal {
	return sig(propertiesIface+".PropertiesChanged", devPath, iface, props, []string{})
}

func TestWatcher(t *testing.T) {
	w := newWatcher("/org/bluez/hci0")

	// Battery for an unknown device is ignored.
	if got := w.handle(added(map[string]map[string]dbus.Variant{
		batteryIface: {"Percentage": dbus.MakeVariant(byte(50))},
	})); len(got) != 0 {
		t.Fatalf("battery before device: %+v", got)
	}

	got := w.handle(added(map[string]map[string]dbus.Variant{
		deviceIface: {
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF"),
			"Alias":   dbus.MakeVariant("Headset"),
			"Trusted": dbus.MakeVariant(true),
		},
	}))
	if len(got) != 1 || got[0].Device.Alias != "Headset" || !got[0].Device.Trusted {
		t.Fatalf("device added: %+v", got)
	}

	// RSSI noise does not produce a change.
	if got := w.handle(propsChanged(deviceIface, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-40))})); len(got) != 0 {
		t.Fatalf("RSSI: %+v", got)
	}

	got = w.handle(propsChanged(deviceIface, map[string]dbus.Variant{"Connected": dbus.MakeVariant(true)}))
	if len(got) != 1 || !got[0].Device.Connected || got[0].Device.Alias != "Headset" {
		t.Fatalf("connected: %+v", got)
	}

	got = w.handle(added(map[string]map[string]dbus.Variant{
		batteryIface: {"Percentage": dbus.MakeVariant(byte(80))},
	}))
	if len(got) != 1 || !got[0].Device.HasBattery || got[0].Device.BatteryPercent != 80 {
		t.Fatalf("battery added: %+v", got)
	}

	got = w.handle(propsChanged(batteryIface, map[string]dbus.Variant{"Percentage": dbus.MakeVariant(byte(79))}))
	if len(got) != 1 || got[0].Device.BatteryPercent != 79 {
		t.Fatalf("battery changed: %+v", got)
	}

	got = w.handle(sig(objectManager+".InterfacesRemoved", "/", devPath, []string{batteryIface}))
	if len(got) != 1 || got[0].Device.HasBattery {
		t.Fatalf("battery removed: %+v", got)
	}

	got = w.handle(sig(objectManager+".InterfacesRemoved", "/", devPath, []string{deviceIface}))
	if len(got) != 1 || !got[0].Removed || got[0].Device.Address != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("device removed: %+v", got)
	}

	// Signals for child objects are ignored.
	if got := w.handle(sig(objectManager+".InterfacesAdded", "/", devPath+"/service0001", map[string]map[string]dbus.Variant{
		deviceIface: {"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF")},
	})); len(got) != 0 {
		t.Fatalf("child object: %+v", got)
	}
}
