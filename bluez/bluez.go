// Package bluez talks to the local BlueZ daemon over the system D-Bus. It
// covers what remote-bluetooth needs: listing devices, connecting,
// disconnecting and watching for changes, including battery levels.
package bluez

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	service         = "org.bluez"
	adapterIface    = "org.bluez.Adapter1"
	deviceIface     = "org.bluez.Device1"
	batteryIface    = "org.bluez.Battery1"
	objectManager   = "org.freedesktop.DBus.ObjectManager"
	propertiesIface = "org.freedesktop.DBus.Properties"

	errUnknownObject    = "org.freedesktop.DBus.Error.UnknownObject"
	errUnknownMethod    = "org.freedesktop.DBus.Error.UnknownMethod"
	errDoesNotExist     = "org.bluez.Error.DoesNotExist"
	errAlreadyConnected = "org.bluez.Error.AlreadyConnected"
	errNotConnected     = "org.bluez.Error.NotConnected"
)

var (
	// ErrNotFound is returned when no device or adapter matches.
	ErrNotFound = errors.New("bluez: not found")
	// ErrInvalidAddress is returned for a malformed Bluetooth address.
	ErrInvalidAddress = errors.New("bluez: invalid bluetooth address")
)

var addressRe = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)

// NormalizeAddress upper-cases a Bluetooth address and checks its format.
func NormalizeAddress(addr string) (string, error) {
	a := strings.ToUpper(strings.TrimSpace(addr))
	if !addressRe.MatchString(a) {
		return "", fmt.Errorf("%w: %q", ErrInvalidAddress, addr)
	}
	return a, nil
}

// Device is a snapshot of one BlueZ device.
type Device struct {
	Address   string
	Name      string
	Alias     string
	Icon      string
	Paired    bool
	Trusted   bool
	Connected bool

	// HasBattery reports whether the device exposes a battery level.
	HasBattery bool
	// BatteryPercent is the battery level 0-100, valid when HasBattery.
	BatteryPercent uint8
}

// Adapter is one local Bluetooth adapter.
type Adapter struct {
	conn *dbus.Conn
	path dbus.ObjectPath
}

// Open connects to the system bus and opens the adapter with the given ID
// ("hci0"). An empty ID picks the first adapter BlueZ knows about.
func Open(id string) (*Adapter, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("bluez: connect system bus: %w", err)
	}
	a, err := openOn(context.Background(), conn, id)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return a, nil
}

func openOn(ctx context.Context, conn *dbus.Conn, id string) (*Adapter, error) {
	objs, err := managedObjects(ctx, conn)
	if err != nil {
		return nil, err
	}
	var adapters []string
	for path, ifaces := range objs {
		if _, ok := ifaces[adapterIface]; ok {
			adapters = append(adapters, string(path))
		}
	}
	sort.Strings(adapters)
	for _, p := range adapters {
		if id == "" || p == "/org/bluez/"+id {
			return &Adapter{conn: conn, path: dbus.ObjectPath(p)}, nil
		}
	}
	if id == "" {
		return nil, fmt.Errorf("%w: no bluetooth adapter", ErrNotFound)
	}
	return nil, fmt.Errorf("%w: adapter %q", ErrNotFound, id)
}

// Close closes the D-Bus connection.
func (a *Adapter) Close() error { return a.conn.Close() }

// ID returns the adapter ID, e.g. "hci0".
func (a *Adapter) ID() string { return strings.TrimPrefix(string(a.path), "/org/bluez/") }

// Devices returns every device known to the adapter, sorted by address.
func (a *Adapter) Devices(ctx context.Context) ([]Device, error) {
	objs, err := managedObjects(ctx, a.conn)
	if err != nil {
		return nil, err
	}
	var devs []Device
	for path, ifaces := range objs {
		if !a.isDevicePath(path) {
			continue
		}
		props, ok := ifaces[deviceIface]
		if !ok {
			continue
		}
		d := deviceFromProps(props)
		applyBattery(&d, ifaces[batteryIface])
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Address < devs[j].Address })
	return devs, nil
}

// Device returns one device by address.
func (a *Adapter) Device(ctx context.Context, address string) (Device, error) {
	path, addr, err := a.devicePath(address)
	if err != nil {
		return Device{}, err
	}
	var props map[string]dbus.Variant
	err = a.conn.Object(service, path).CallWithContext(ctx, propertiesIface+".GetAll", 0, deviceIface).Store(&props)
	if err != nil {
		return Device{}, mapErr(err, addr)
	}
	d := deviceFromProps(props)
	var bat map[string]dbus.Variant
	if a.conn.Object(service, path).CallWithContext(ctx, propertiesIface+".GetAll", 0, batteryIface).Store(&bat) == nil {
		applyBattery(&d, bat)
	}
	return d, nil
}

// Connect connects the device. Connecting an already connected device is
// not an error.
func (a *Adapter) Connect(ctx context.Context, address string) error {
	return a.call(ctx, address, "Connect", errAlreadyConnected)
}

// Disconnect disconnects the device. Disconnecting a device that is not
// connected is not an error.
func (a *Adapter) Disconnect(ctx context.Context, address string) error {
	return a.call(ctx, address, "Disconnect", errNotConnected)
}

func (a *Adapter) call(ctx context.Context, address, method, okErr string) error {
	path, addr, err := a.devicePath(address)
	if err != nil {
		return err
	}
	err = a.conn.Object(service, path).CallWithContext(ctx, deviceIface+"."+method, 0).Err
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) && dbusErr.Name == okErr {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bluez: %s %s: %w", strings.ToLower(method), addr, mapErr(err, addr))
	}
	return nil
}

func (a *Adapter) devicePath(address string) (dbus.ObjectPath, string, error) {
	addr, err := NormalizeAddress(address)
	if err != nil {
		return "", "", err
	}
	return a.path + "/dev_" + dbus.ObjectPath(strings.ReplaceAll(addr, ":", "_")), addr, nil
}

// isDevicePath reports whether path is a device directly under the adapter
// (and not one of its child objects such as GATT services).
func (a *Adapter) isDevicePath(path dbus.ObjectPath) bool {
	rest, ok := strings.CutPrefix(string(path), string(a.path)+"/dev_")
	return ok && !strings.Contains(rest, "/")
}

func managedObjects(ctx context.Context, conn *dbus.Conn) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := conn.Object(service, "/").CallWithContext(ctx, objectManager+".GetManagedObjects", 0).Store(&objs)
	if err != nil {
		return nil, fmt.Errorf("bluez: list objects (is bluetoothd running?): %w", err)
	}
	return objs, nil
}

func mapErr(err error, addr string) error {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		switch dbusErr.Name {
		case errUnknownObject, errUnknownMethod, errDoesNotExist:
			return fmt.Errorf("%w: device %s", ErrNotFound, addr)
		}
	}
	return err
}

func deviceFromProps(props map[string]dbus.Variant) Device {
	var d Device
	applyDeviceProps(&d, props)
	return d
}

// applyDeviceProps sets the Device1 properties present in props and reports
// whether any of them changed d.
func applyDeviceProps(d *Device, props map[string]dbus.Variant) bool {
	before := *d
	for k, v := range props {
		switch k {
		case "Address":
			d.Address, _ = v.Value().(string)
		case "Name":
			d.Name, _ = v.Value().(string)
		case "Alias":
			d.Alias, _ = v.Value().(string)
		case "Icon":
			d.Icon, _ = v.Value().(string)
		case "Paired":
			d.Paired, _ = v.Value().(bool)
		case "Trusted":
			d.Trusted, _ = v.Value().(bool)
		case "Connected":
			d.Connected, _ = v.Value().(bool)
		}
	}
	return *d != before
}

// applyBattery sets the battery level from Battery1 properties. A nil map
// leaves d untouched.
func applyBattery(d *Device, props map[string]dbus.Variant) bool {
	v, ok := props["Percentage"]
	if !ok {
		return false
	}
	p, ok := v.Value().(byte)
	if !ok {
		return false
	}
	changed := !d.HasBattery || d.BatteryPercent != p
	d.HasBattery, d.BatteryPercent = true, p
	return changed
}
