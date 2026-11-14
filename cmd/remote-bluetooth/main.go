// Command remote-bluetooth shares a machine's trusted Bluetooth devices with
// other machines on the network, and controls the devices of others.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/client"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/audio"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/auth"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/discovery"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/server"
)

const defaultPort = client.DefaultPort

const usage = `remote-bluetooth shares trusted Bluetooth devices between machines.

Usage:
  remote-bluetooth serve       [flags]              run the server (see the systemd unit)
  remote-bluetooth devices     [flags]              list devices on all servers
  remote-bluetooth watch       [flags]              stream device and server changes
  remote-bluetooth connect     [flags] SERVER ADDR  connect a device on a server
  remote-bluetooth disconnect  [flags] SERVER ADDR  disconnect a device on a server
  remote-bluetooth play-here   [flags] ADDR         move the sound for a device to this machine
  remote-bluetooth play-on     [flags] SERVER ADDR  move the sound for a device to a server
  remote-bluetooth gen-secret  [-force]             create the shared secret

Every machine needs the same secret, in $REMOTE_BLUETOOTH_SECRET or
~/.config/remote-bluetooth/secret. Run "remote-bluetooth COMMAND -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmds := map[string]func(context.Context, []string) error{
		"serve":      serve,
		"devices":    devices,
		"watch":      watch,
		"connect":    func(ctx context.Context, a []string) error { return control(ctx, "connect", a) },
		"disconnect": func(ctx context.Context, a []string) error { return control(ctx, "disconnect", a) },
		"play-here":  playHere,
		"play-on":    playOn,
		"gen-secret": genSecret,
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		fmt.Print(usage)
		return
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	if err := cmd(ctx, os.Args[2:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "remote-bluetooth:", err)
		os.Exit(1)
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func serve(ctx context.Context, args []string) error {
	hostname, _ := os.Hostname()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", envInt("REMOTE_BLUETOOTH_PORT", defaultPort), "TCP port to listen on [$REMOTE_BLUETOOTH_PORT]")
	adapterID := fs.String("adapter", os.Getenv("REMOTE_BLUETOOTH_ADAPTER"), "Bluetooth adapter, e.g. hci0; default is the first one [$REMOTE_BLUETOOTH_ADAPTER]")
	name := fs.String("name", envOr("REMOTE_BLUETOOTH_NAME", hostname), "name clients see for this server [$REMOTE_BLUETOOTH_NAME]")
	noAnnounce := fs.Bool("no-announce", false, "do not announce the server with mDNS")
	verbose := fs.Bool("v", false, "debug logging")
	_ = fs.Parse(args)
	log := newLogger(*verbose)

	secret, err := client.LoadSecret()
	if err != nil {
		return err
	}
	id, err := auth.New(secret)
	if err != nil {
		return err
	}

	adapter, err := bluez.Open(*adapterID)
	if err != nil {
		return err
	}
	defer adapter.Close()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return err
	}

	// The manager reports released changes to the server, which needs the
	// manager: close the loop with a forwarding closure.
	var srv *server.Server
	mgr := audio.New(audio.ExecRunner{}, stateDir(), func(address string, released bool) {
		srv.SetReleased(address, released)
	}, audio.WithLogger(log))
	srv = server.New(adapter, mgr, log)
	g := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(id.ServerConfig())),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 20 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: time.Minute, Timeout: 20 * time.Second}),
	)
	srv.Register(g)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Put back any device a previous run left released.
	if err := mgr.Recover(ctx); err != nil {
		log.Warn("restoring released audio failed", "err", err)
	}
	errc := make(chan error, 3)
	go func() { errc <- srv.Run(ctx) }()
	go func() { errc <- g.Serve(lis) }()
	if !*noAnnounce {
		go func() {
			if err := discovery.Announce(ctx, *name, *port); err != nil {
				// Static client configuration still works without mDNS.
				log.Warn("mDNS announce failed", "err", err)
			}
		}()
	}
	log.Info("serving", "name", *name, "addr", lis.Addr().String(), "adapter", adapter.ID())

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	cancel()
	g.Stop()
	// Never leave a device released when the server goes away.
	if cerr := mgr.Close(context.Background()); cerr != nil {
		log.Warn("restoring released audio failed", "err", cerr)
	}
	return err
}

// stateDir is where the audio manager keeps its release records.
func stateDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "remote-bluetooth")
}

// clientFlags are shared by the commands that talk to servers.
type clientFlags struct {
	servers      *string
	noDiscovery  *bool
	includeLocal *bool
	verbose      *bool
}

func addClientFlags(fs *flag.FlagSet) clientFlags {
	return clientFlags{
		servers:      fs.String("servers", os.Getenv(client.ServersEnv), "comma separated host:port servers besides discovered ones [$REMOTE_BLUETOOTH_SERVERS]"),
		noDiscovery:  fs.Bool("no-discovery", false, "do not look for servers with mDNS"),
		includeLocal: fs.Bool("local", false, "include the server on this machine"),
		verbose:      fs.Bool("v", false, "log diagnostics to stderr"),
	}
}

func (f clientFlags) newClient() (*client.Client, error) {
	secret, err := client.LoadSecret()
	if err != nil {
		return nil, err
	}
	servers := client.ParseServers(*f.servers)
	var log *slog.Logger
	if *f.verbose {
		log = newLogger(true)
	}
	return client.New(client.Config{
		Secret:           secret,
		Servers:          servers,
		DisableDiscovery: *f.noDiscovery,
		IncludeLocal:     *f.includeLocal,
		Logger:           log,
	})
}

func devices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	cf := addClientFlags(fs)
	wait := fs.Duration("wait", 2*time.Second, "how long to look for servers")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	c, err := cf.newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	for range c.Events() {
	}
	devs := c.Devices()

	if *asJSON {
		type jsonDevice struct {
			Server    string `json:"server"`
			Address   string `json:"address"`
			Name      string `json:"name"`
			Alias     string `json:"alias"`
			Icon      string `json:"icon"`
			Paired    bool   `json:"paired"`
			Connected bool   `json:"connected"`
			Audio     string `json:"audio"`
			Released  bool   `json:"released"`
			Battery   *uint8 `json:"battery,omitempty"`
		}
		out := make([]jsonDevice, 0, len(devs))
		for _, d := range devs {
			jd := jsonDevice{Server: d.Server, Address: d.Address, Name: d.Name, Alias: d.Alias, Icon: d.Icon, Paired: d.Paired, Connected: d.Connected, Audio: d.Audio.String(), Released: d.Released}
			if d.HasBattery {
				b := d.BatteryPercent
				jd.Battery = &b
			}
			out = append(out, jd)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(devs) == 0 {
		fmt.Fprintln(os.Stderr, "no devices found")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tADDRESS\tNAME\tCONNECTED\tAUDIO\tBATTERY")
	for _, d := range devs {
		battery := "-"
		if d.HasBattery {
			battery = fmt.Sprintf("%d%%", d.BatteryPercent)
		}
		audioCol := "-"
		if d.Audio != bluez.AudioNone {
			audioCol = d.Audio.String()
		}
		if d.Released {
			audioCol += ", released"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\n", d.Server, d.Address, d.Alias, d.Connected, audioCol, battery)
	}
	return tw.Flush()
}

func watch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	cf := addClientFlags(fs)
	_ = fs.Parse(args)

	c, err := cf.newClient()
	if err != nil {
		return err
	}
	go func() { _ = c.Run(ctx) }()
	for ev := range c.Events() {
		switch ev.Type {
		case client.DeviceUpdated:
			d := ev.Device
			battery := ""
			if d.HasBattery {
				battery = fmt.Sprintf(" battery=%d%%", d.BatteryPercent)
			}
			fmt.Printf("%s %s %s %q connected=%v%s\n", ev.Type, ev.Server, d.Address, d.Alias, d.Connected, battery)
		case client.DeviceRemoved:
			fmt.Printf("%s %s %s\n", ev.Type, ev.Server, ev.Device.Address)
		case client.ServerDown:
			fmt.Printf("%s %s %v\n", ev.Type, ev.Server, ev.Err)
		default:
			fmt.Printf("%s %s\n", ev.Type, ev.Server)
		}
	}
	return ctx.Err()
}

func control(ctx context.Context, op string, args []string) error {
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	cf := addClientFlags(fs)
	wait := fs.Duration("wait", 5*time.Second, "how long to wait for the server to show up")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: remote-bluetooth %s SERVER ADDRESS", op)
	}
	server, addr := fs.Arg(0), fs.Arg(1)

	c, err := cf.newClient()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.Run(runCtx) }()

	timeout := time.After(*wait)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return ctx.Err()
			}
			if ev.Type != client.ServerUp || ev.Server != server {
				continue
			}
			go func() {
				for range c.Events() { // keep the client moving
				}
			}()
			if op == "connect" {
				err = c.Connect(ctx, server, addr)
			} else {
				err = c.Disconnect(ctx, server, addr)
			}
			if err == nil {
				fmt.Printf("%sed %s on %s\n", op, addr, server)
			}
			return err
		case <-timeout:
			return fmt.Errorf("server %q not found within %s (known: %s)", server, *wait, strings.Join(c.Servers(), ", "))
		}
	}
}

// playHere moves the sound for a device to this machine: servers sending to
// it let go, this machine's own audio takes over, and they are restored.
func playHere(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("play-here", flag.ExitOnError)
	cf := addClientFlags(fs)
	adapterID := fs.String("adapter", os.Getenv("REMOTE_BLUETOOTH_ADAPTER"), "Bluetooth adapter, e.g. hci0; default is the first one [$REMOTE_BLUETOOTH_ADAPTER]")
	wait := fs.Duration("wait", 5*time.Second, "how long to wait for servers to show up")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: remote-bluetooth play-here ADDRESS")
	}
	addr, err := bluez.NormalizeAddress(fs.Arg(0))
	if err != nil {
		return err
	}

	adapter, err := bluez.Open(*adapterID)
	if err != nil {
		return err
	}
	defer adapter.Close()

	c, err := cf.newClient()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.Run(runCtx) }()

	if err := settleServers(ctx, c, *wait); err != nil {
		return err
	}

	sendingHere := func(ctx context.Context) bool {
		d, err := adapter.Device(ctx, addr)
		return err == nil && d.Audio == bluez.AudioSending
	}
	res, err := c.PlayHere(ctx, addr, sendingHere)
	for name, ferr := range res.Failed {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, ferr)
	}
	if err != nil {
		return err
	}
	fmt.Printf("sound moved here; %s let go and are back to normal\n", strings.Join(res.Released, ", "))
	return nil
}

// settleServers waits for the first server, then gives the others a moment
// so the device snapshot is complete, and keeps draining events afterwards
// so the snapshot stays live.
func settleServers(ctx context.Context, c *client.Client, wait time.Duration) error {
	const quiet = 500 * time.Millisecond
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var idle <-chan time.Time
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return ctx.Err()
			}
			if ev.Type == client.ServerUp {
				idle = time.After(quiet)
			}
			continue
		case <-idle:
		case <-deadline.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		go func() {
			for range c.Events() {
			}
		}()
		return nil
	}
}

// playOn moves the sound for a device from this machine to a server: this
// machine lets go until the server sends to it, then goes back to normal.
func playOn(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("play-on", flag.ExitOnError)
	cf := addClientFlags(fs)
	wait := fs.Duration("wait", 5*time.Second, "how long to wait for servers to show up")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: remote-bluetooth play-on SERVER ADDRESS")
	}
	server := fs.Arg(0)
	addr, err := bluez.NormalizeAddress(fs.Arg(1))
	if err != nil {
		return err
	}
	c, err := cf.newClient()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.Run(runCtx) }()
	if err := settleServers(ctx, c, *wait); err != nil {
		return err
	}
	if err := c.PlayOn(ctx, server, addr); err != nil {
		return err
	}
	fmt.Printf("sound moved to %s; this machine is back to normal\n", server)
	return nil
}

func genSecret(_ context.Context, args []string) error {
	fset := flag.NewFlagSet("gen-secret", flag.ExitOnError)
	force := fset.Bool("force", false, "overwrite an existing secret")
	_ = fset.Parse(args)

	path, err := client.SecretPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to replace it)", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	secret := auth.GenerateSecret()
	if _, err := client.SaveSecret(secret); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\nput the same secret on every other machine:\n", path)
	fmt.Println(secret)
	return nil
}
