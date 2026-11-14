package client

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	pb "github.com/andree-bjorkgard/remote-bluetooth/v2/internal/pb/remotebtv1"
)

// callTimeout bounds a connect or disconnect when the caller's context has
// no deadline. Bluetooth connects can take a while.
const callTimeout = 40 * time.Second

var schemeSeq atomic.Uint64

// serverConn follows one server: one gRPC connection, one WatchDevices
// stream at a time, and the devices that stream reported.
type serverConn struct {
	c        *Client
	name     string
	resolver *manual.Resolver
	conn     *grpc.ClientConn
	rpc      pb.BluetoothServiceClient

	mu      sync.Mutex
	up      bool
	devices map[string]Device
}

func newServerConn(c *Client, name string, addrs []string) (*serverConn, error) {
	// A manual resolver lets gRPC try every announced address and lets us
	// swap them when the server is re-announced elsewhere.
	r := manual.NewBuilderWithScheme("rbt" + strconv.FormatUint(schemeSeq.Add(1), 10))
	r.InitialState(resolverState(addrs))
	opts := []grpc.DialOption{
		grpc.WithResolvers(r),
		grpc.WithTransportCredentials(credentials.NewTLS(c.id.ClientConfig())),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	conn, err := grpc.NewClient(r.Scheme()+":///"+name, opts...)
	if err != nil {
		return nil, err
	}
	return &serverConn{
		c:        c,
		name:     name,
		resolver: r,
		conn:     conn,
		rpc:      pb.NewBluetoothServiceClient(conn),
		devices:  map[string]Device{},
	}, nil
}

func resolverState(addrs []string) resolver.State {
	var s resolver.State
	for _, a := range addrs {
		s.Addresses = append(s.Addresses, resolver.Address{Addr: a})
	}
	return s
}

func (s *serverConn) setAddrs(addrs []string) { s.resolver.UpdateState(resolverState(addrs)) }

func (s *serverConn) isUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up
}

func (s *serverConn) snapshot() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.up {
		return nil
	}
	devs := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, d)
	}
	return devs
}

// run keeps a WatchDevices stream open until ctx is done.
func (s *serverConn) run(ctx context.Context) {
	defer s.conn.Close()
	var lastErr string
	for attempt := 0; ; attempt++ {
		synced, err := s.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		if synced {
			attempt = 0
		}
		s.down(ctx, err)

		// Log a repeating failure once, not on every retry.
		if msg := err.Error(); msg != lastErr {
			lastErr = msg
			if strings.Contains(msg, "handshake failed") || strings.Contains(msg, "certificate") {
				s.c.log.Error("server rejected us or we rejected it; do both sides use the same secret?", "server", s.name, "err", err)
			} else {
				s.c.log.Warn("server unreachable, retrying", "server", s.name, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(attempt)):
		}
	}
}

// watch runs one WatchDevices stream. It reports whether the stream got as
// far as the initial sync, and the error that ended it.
func (s *serverConn) watch(ctx context.Context) (synced bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := s.rpc.WatchDevices(ctx, &pb.WatchDevicesRequest{})
	if err != nil {
		return false, fromStatus(err)
	}
	seen := map[string]bool{} // devices reported before sync
	for {
		msg, err := stream.Recv()
		if err != nil {
			return synced, fromStatus(err)
		}
		switch ev := msg.Event.(type) {
		case *pb.WatchDevicesResponse_Updated:
			d := fromProto(s.name, ev.Updated)
			if !synced {
				seen[d.Address] = true
			}
			s.update(ctx, d, synced)
		case *pb.WatchDevicesResponse_Removed:
			s.remove(ctx, ev.Removed)
		case *pb.WatchDevicesResponse_Synced:
			if synced {
				continue
			}
			synced = true
			s.sync(ctx, seen)
		}
	}
}

// update stores a device. Before sync the events are held back; sync
// reports the whole initial state at once.
func (s *serverConn) update(ctx context.Context, d Device, live bool) {
	s.mu.Lock()
	old, known := s.devices[d.Address]
	s.devices[d.Address] = d
	s.mu.Unlock()
	if live && (!known || old != d) {
		s.c.emit(ctx, Event{Type: DeviceUpdated, Server: s.name, Device: d})
	}
}

func (s *serverConn) remove(ctx context.Context, addr string) {
	s.mu.Lock()
	_, known := s.devices[addr]
	delete(s.devices, addr)
	s.mu.Unlock()
	if known {
		s.c.emit(ctx, Event{Type: DeviceRemoved, Server: s.name, Device: Device{Server: s.name, Device: bluez.Device{Address: addr}}})
	}
}

// sync finishes the initial snapshot: devices from an earlier session that
// were not re-sent are dropped, then the server and its devices are
// announced.
func (s *serverConn) sync(ctx context.Context, seen map[string]bool) {
	s.mu.Lock()
	for addr := range s.devices {
		if !seen[addr] {
			delete(s.devices, addr)
		}
	}
	s.up = true
	devs := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, d)
	}
	s.mu.Unlock()

	s.c.log.Info("server up", "server", s.name, "devices", len(devs))
	s.c.emit(ctx, Event{Type: ServerUp, Server: s.name})
	for _, d := range devs {
		s.c.emit(ctx, Event{Type: DeviceUpdated, Server: s.name, Device: d})
	}
}

// down marks the server unreachable and removes its devices.
func (s *serverConn) down(ctx context.Context, err error) {
	s.mu.Lock()
	wasUp := s.up
	s.up = false
	addrs := make([]string, 0, len(s.devices))
	for a := range s.devices {
		addrs = append(addrs, a)
	}
	clear(s.devices)
	s.mu.Unlock()
	if !wasUp {
		return
	}
	for _, a := range addrs {
		s.c.emit(ctx, Event{Type: DeviceRemoved, Server: s.name, Device: Device{Server: s.name, Device: bluez.Device{Address: a}}})
	}
	s.c.emit(ctx, Event{Type: ServerDown, Server: s.name, Err: err})
}

func (s *serverConn) connect(ctx context.Context, address string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	_, err := s.rpc.Connect(ctx, &pb.ConnectRequest{Address: address})
	if err != nil {
		return fmt.Errorf("connect %s on %s: %w", address, s.name, fromStatus(err))
	}
	return nil
}

func (s *serverConn) disconnect(ctx context.Context, address string) error {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	_, err := s.rpc.Disconnect(ctx, &pb.DisconnectRequest{Address: address})
	if err != nil {
		return fmt.Errorf("disconnect %s on %s: %w", address, s.name, fromStatus(err))
	}
	return nil
}

func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, callTimeout)
}

func fromProto(server string, p *pb.Device) Device {
	d := Device{
		Server:   server,
		Released: p.GetReleased(),
		Device: bluez.Device{
			Address:   p.GetAddress(),
			Name:      p.GetName(),
			Alias:     p.GetAlias(),
			Icon:      p.GetIcon(),
			Paired:    p.GetPaired(),
			Trusted:   p.GetTrusted(),
			Connected: p.GetConnected(),
			Audio:     audioFromProto(p.GetAudioState()),
		},
	}
	if p.BatteryPercent != nil {
		d.HasBattery = true
		d.BatteryPercent = uint8(min(p.GetBatteryPercent(), 100))
	}
	return d
}

// audioFromProto maps unknown values (a newer server) to AudioNone.
func audioFromProto(a pb.AudioState) bluez.AudioState {
	switch a {
	case pb.AudioState_AUDIO_STATE_IDLE:
		return bluez.AudioIdle
	case pb.AudioState_AUDIO_STATE_SENDING:
		return bluez.AudioSending
	}
	return bluez.AudioNone
}
