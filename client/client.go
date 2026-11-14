// Package client finds remote-bluetooth servers and keeps a live view of
// their trusted devices.
//
//	c, err := client.New(client.Config{Secret: secret})
//	go c.Run(ctx)
//	for ev := range c.Events() {
//		...
//	}
//
// Servers are found with mDNS and, optionally, from a static list. Each
// server gets one long-lived stream; when it breaks the client reconnects
// with backoff and resyncs, so Devices and Events always reflect the
// servers that are reachable right now.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/auth"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/discovery"
)

var (
	// ErrUnknownServer is returned for a server name the client has not seen.
	ErrUnknownServer = errors.New("client: unknown server")
	// ErrServerUnavailable is returned when the server is not connected.
	ErrServerUnavailable = errors.New("client: server unavailable")
	// ErrDeviceNotFound is returned when the server has no such trusted device.
	ErrDeviceNotFound = errors.New("client: device not found")
)

// Config configures a Client.
type Config struct {
	// Secret is the shared secret every server uses. Required.
	Secret string
	// Servers are host:port addresses to use in addition to the ones found
	// with mDNS, e.g. for servers on another subnet or over a VPN. Each is
	// named by its address.
	Servers []string
	// DisableDiscovery turns off mDNS; only Servers are used.
	DisableDiscovery bool
	// IncludeLocal also uses a server running on this machine. It is
	// skipped by default because local devices are usually handled
	// directly (see package bluez).
	IncludeLocal bool
	// Logger receives diagnostics. Defaults to discarding them.
	Logger *slog.Logger
}

// Device is a trusted device on a server.
type Device struct {
	// Server is the name of the server the device belongs to.
	Server string
	bluez.Device
}

// EventType says what an Event is about.
type EventType int

const (
	// DeviceUpdated: a device appeared or changed. Event.Device is set.
	DeviceUpdated EventType = iota
	// DeviceRemoved: a device is gone, because the server forgot it or
	// became unreachable. Event.Device has Server and Address set.
	DeviceRemoved
	// ServerUp: a server connected and its devices have been sent.
	ServerUp
	// ServerDown: a server became unreachable. Event.Err says why.
	ServerDown
)

func (t EventType) String() string {
	switch t {
	case DeviceUpdated:
		return "device-updated"
	case DeviceRemoved:
		return "device-removed"
	case ServerUp:
		return "server-up"
	case ServerDown:
		return "server-down"
	}
	return fmt.Sprintf("EventType(%d)", int(t))
}

// Event is one change seen by the client.
type Event struct {
	Type   EventType
	Server string
	Device Device
	Err    error
}

// Client tracks servers and their devices.
type Client struct {
	cfg    Config
	id     *auth.Identity
	log    *slog.Logger
	events chan Event

	mu      sync.Mutex
	servers map[string]*serverConn
	running bool
}

// New validates cfg and returns a client. Call Run to start it.
func New(cfg Config) (*Client, error) {
	id, err := auth.New(cfg.Secret)
	if err != nil {
		return nil, err
	}
	if cfg.DisableDiscovery && len(cfg.Servers) == 0 {
		return nil, errors.New("client: discovery disabled and no servers configured")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		cfg:     cfg,
		id:      id,
		log:     log,
		events:  make(chan Event, 64),
		servers: map[string]*serverConn{},
	}, nil
}

// Events returns the event channel. It must be read for the client to make
// progress, and it is closed when Run returns.
func (c *Client) Events() <-chan Event { return c.events }

// Run finds and follows servers until ctx is done. It returns ctx's error.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("client: Run called twice")
	}
	c.running = true
	c.mu.Unlock()

	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(c.events)
	}()

	for _, addr := range c.cfg.Servers {
		c.upsert(ctx, &wg, addr, []string{addr})
	}

	if c.cfg.DisableDiscovery {
		<-ctx.Done()
		return ctx.Err()
	}

	localID := discovery.HostID()
	found := discovery.Browse(ctx, func(err error) { c.log.Warn("mDNS discovery unavailable", "err", err) })
	for svc := range found {
		local := svc.HostID == localID
		if local && !c.cfg.IncludeLocal {
			c.log.Debug("skipping local server", "server", svc.Instance)
			continue
		}
		addrs := svc.Addrs
		if !local {
			addrs = withoutLocalIPs(addrs)
		}
		if len(addrs) == 0 {
			continue
		}
		c.upsert(ctx, &wg, svc.Instance, addrs)
	}
	<-ctx.Done()
	return ctx.Err()
}

// upsert starts following a server, or updates the addresses of one we
// already follow.
func (c *Client) upsert(ctx context.Context, wg *sync.WaitGroup, name string, addrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.servers[name]; ok {
		s.setAddrs(addrs)
		return
	}
	s, err := newServerConn(c, name, addrs)
	if err != nil {
		c.log.Error("cannot follow server", "server", name, "err", err)
		return
	}
	c.servers[name] = s
	c.log.Info("found server", "server", name, "addrs", addrs)
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.run(ctx)
	}()
}

// emit delivers an event unless ctx is done.
func (c *Client) emit(ctx context.Context, ev Event) {
	select {
	case c.events <- ev:
	case <-ctx.Done():
	}
}

// Devices returns the devices of all reachable servers, sorted by server
// and alias.
func (c *Client) Devices() []Device {
	c.mu.Lock()
	servers := make([]*serverConn, 0, len(c.servers))
	for _, s := range c.servers {
		servers = append(servers, s)
	}
	c.mu.Unlock()

	var devs []Device
	for _, s := range servers {
		devs = append(devs, s.snapshot()...)
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].Server != devs[j].Server {
			return devs[i].Server < devs[j].Server
		}
		if devs[i].Alias != devs[j].Alias {
			return devs[i].Alias < devs[j].Alias
		}
		return devs[i].Address < devs[j].Address
	})
	return devs
}

// Servers returns the names of the reachable servers, sorted.
func (c *Client) Servers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for name, s := range c.servers {
		if s.isUp() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Connect asks server to connect the device with the given address.
func (c *Client) Connect(ctx context.Context, server, address string) error {
	s, err := c.server(server)
	if err != nil {
		return err
	}
	return s.connect(ctx, address)
}

// Disconnect asks server to disconnect the device with the given address.
func (c *Client) Disconnect(ctx context.Context, server, address string) error {
	s, err := c.server(server)
	if err != nil {
		return err
	}
	return s.disconnect(ctx, address)
}

func (c *Client) server(name string) (*serverConn, error) {
	c.mu.Lock()
	s, ok := c.servers[name]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownServer, name)
	}
	if !s.isUp() {
		return nil, fmt.Errorf("%w: %q", ErrServerUnavailable, name)
	}
	return s, nil
}

func fromStatus(err error) error {
	switch status.Code(err) {
	case codes.OK:
		return nil
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrDeviceNotFound, status.Convert(err).Message())
	case codes.Unavailable:
		return fmt.Errorf("%w: %s", ErrServerUnavailable, status.Convert(err).Message())
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.Canceled:
		return context.Canceled
	}
	return errors.New(status.Convert(err).Message())
}

// withoutLocalIPs drops addresses that belong to this machine. A remote
// server can't be reached at our own IP, and dialing it (say a shared
// Docker bridge address) would reach our own server instead.
func withoutLocalIPs(addrs []string) []string {
	local := map[string]bool{}
	if ifAddrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifAddrs {
			if n, ok := a.(*net.IPNet); ok {
				local[n.IP.String()] = true
			}
		}
	}
	var out []string
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil || local[host] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// backoff returns the delay before retry attempt n (starting at 0).
func backoff(n int) time.Duration {
	d := time.Second << min(n, 5)
	return min(d, 30*time.Second)
}
