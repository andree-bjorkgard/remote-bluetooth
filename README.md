# remote-bluetooth

Control the Bluetooth devices of your other Linux machines. Each machine runs
a small server that exposes its **trusted** devices; clients find the servers
on the local network by themselves and get a live view of every device:
connected or not, battery level, and changes as they happen.

```
$ remote-bluetooth devices
SERVER   ADDRESS            NAME          CONNECTED  BATTERY
desktop  00:0A:45:19:F3:A6  ATH-M50xBT2   true       40%
desktop  DD:57:25:0A:B2:F7  MX Master 3S  false      -
laptop   E6:8D:20:F2:DA:2B  Glove80       true       39%

$ remote-bluetooth connect laptop DD:57:25:0A:B2:F7
```

## How it works

- **Discovery:** servers announce `_remote-bt._tcp` with mDNS/DNS-SD
  (the same thing printers use; it coexists with avahi). Clients keep
  browsing, so servers that start later are picked up. For machines mDNS
  can't reach (other subnet, VPN) list them with `-servers` /
  `$REMOTE_BLUETOOTH_SERVERS`.
- **Transport:** gRPC over TLS 1.3 with mutual authentication. Every machine
  derives the same private CA from a shared secret, so only machines with the
  secret can talk to each other, and there are no certificate files to manage.
- **Live state:** `WatchDevices` streams the device list and every change,
  driven by BlueZ's D-Bus signals. Clients reconnect with backoff and resync
  when a server goes away.
- **BlueZ:** spoken to directly over D-Bus (`bluez` package).

## Setup

On one machine:

```sh
make install                  # binary to ~/go/bin, user service installed
remote-bluetooth gen-secret   # writes ~/.config/remote-bluetooth/secret, prints it
systemctl --user enable --now remote-bluetooth
```

On every other machine, `make install`, put the same secret in
`~/.config/remote-bluetooth/secret` (mode 600), and enable the service.

## Commands

| Command | |
|---|---|
| `serve` | run the server (what the systemd unit runs) |
| `devices [-json]` | list devices on all servers |
| `watch` | stream device and server changes |
| `connect SERVER ADDR` | connect a device on a server |
| `disconnect SERVER ADDR` | disconnect a device on a server |
| `play-here ADDR` | move the sound for a device to this machine; servers sending to it let go and are restored |
| `gen-secret` | create the shared secret |

Client commands skip the server on the local machine unless given `-local`.

## Configuration

| Variable | Default | |
|---|---|---|
| `REMOTE_BLUETOOTH_SECRET` | `~/.config/remote-bluetooth/secret` | shared secret |
| `REMOTE_BLUETOOTH_PORT` | `8829` | server TCP port |
| `REMOTE_BLUETOOTH_ADAPTER` | first adapter | e.g. `hci1` |
| `REMOTE_BLUETOOTH_NAME` | hostname | server name clients see |
| `REMOTE_BLUETOOTH_SERVERS` | | extra `host[:port]` servers, comma separated |

The service also reads `~/.config/remote-bluetooth/env` for these.

## Library

```go
import "github.com/andree-bjorkgard/remote-bluetooth/v2/client"

secret, _ := client.LoadSecret()
c, err := client.New(client.Config{Secret: secret})
go c.Run(ctx)
for ev := range c.Events() {
	switch ev.Type {
	case client.DeviceUpdated: // ev.Device.Alias, .Connected, .BatteryPercent...
	case client.DeviceRemoved:
	}
}
c.Connect(ctx, server, address)
```

`client.Device` embeds `bluez.Device`, and package `bluez` can also read and
control the local adapter, so local and remote devices share one type.

## Development

```sh
make test       # vet + race tests (no Bluetooth hardware needed)
make generate   # regenerate gRPC code after editing proto/
```
