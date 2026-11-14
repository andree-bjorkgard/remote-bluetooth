// Package server serves a host's trusted Bluetooth devices over gRPC.
package server

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/audio"
	pb "github.com/andree-bjorkgard/remote-bluetooth/v2/internal/pb/remotebtv1"
)

// Backend is the Bluetooth stack the server exposes. *bluez.Adapter
// implements it.
type Backend interface {
	Connect(ctx context.Context, address string) error
	Disconnect(ctx context.Context, address string) error
	Watch(ctx context.Context) ([]bluez.Device, <-chan bluez.Change, error)
}

var _ Backend = (*bluez.Adapter)(nil)

// Audio releases and restores a device's audio on this host. *audio.Manager
// implements it.
type Audio interface {
	Release(ctx context.Context, address string, hold time.Duration) error
	Restore(ctx context.Context, address string) error
	Released(address string) bool
}

var _ Audio = (*audio.Manager)(nil)

const (
	// subscriberBuffer is how many events a slow watcher may lag behind
	// before it is dropped and has to resync.
	subscriberBuffer = 128
	// maxCallTime caps how long a connect or disconnect may take.
	maxCallTime = 30 * time.Second
	// defaultHold and maxHold bound how long a release lasts before the
	// audio manager restores the device by itself.
	defaultHold = 15 * time.Second
	maxHold     = 30 * time.Second
)

// Server keeps the current set of trusted devices and fans changes out to
// every WatchDevices stream.
type Server struct {
	pb.UnimplementedBluetoothServiceServer

	backend Backend
	audio   Audio // may be nil
	log     *slog.Logger

	mu       sync.Mutex
	synced   chan struct{} // closed once the initial device list is loaded
	devices  map[string]bluez.Device
	released map[string]bool // addresses whose audio is released here
	subs     map[*subscriber]struct{}
}

type subscriber struct {
	ch      chan *pb.WatchDevicesResponse
	dropped bool
}

// New returns a server for backend. audio may be nil, in which case Release
// and Restore are unimplemented. Call Run to start tracking devices.
func New(backend Backend, audio Audio, log *slog.Logger) *Server {
	return &Server{
		backend:  backend,
		audio:    audio,
		released: map[string]bool{},
		log:      log,
		synced:   make(chan struct{}),
		devices:  map[string]bluez.Device{},
		subs:     map[*subscriber]struct{}{},
	}
}

// Register adds the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { pb.RegisterBluetoothServiceServer(g, s) }

// Run tracks the backend's devices until ctx is done. It returns an error if
// the backend's watch fails or ends early.
func (s *Server) Run(ctx context.Context) error {
	initial, changes, err := s.backend.Watch(ctx)
	if err != nil {
		return err
	}
	for _, d := range initial {
		s.apply(bluez.Change{Device: d})
	}
	s.mu.Lock()
	n := len(s.devices)
	s.mu.Unlock()
	close(s.synced)
	s.log.Info("tracking devices", "trusted", n)

	for {
		select {
		case <-ctx.Done():
			return nil
		case c, ok := <-changes:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("server: bluetooth watch ended")
			}
			s.apply(c)
		}
	}
}

// apply updates the trusted device set and notifies subscribers.
func (s *Server) apply(c bluez.Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	addr := c.Device.Address
	old, known := s.devices[addr]
	if c.Removed || !c.Device.Trusted {
		if known {
			delete(s.devices, addr)
			s.broadcast(&pb.WatchDevicesResponse{Event: &pb.WatchDevicesResponse_Removed{Removed: addr}})
		}
		return
	}
	if known && old == c.Device {
		return
	}
	s.devices[addr] = c.Device
	s.broadcast(&pb.WatchDevicesResponse{Event: &pb.WatchDevicesResponse_Updated{Updated: toProto(c.Device, s.released[addr])}})
}

// SetReleased records whether the device's audio is released on this host and
// tells watchers. It is the audio manager's change callback.
func (s *Server) SetReleased(address string, released bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if released {
		s.released[address] = true
	} else {
		delete(s.released, address)
	}
	if d, ok := s.devices[address]; ok {
		s.broadcast(&pb.WatchDevicesResponse{Event: &pb.WatchDevicesResponse_Updated{Updated: toProto(d, released)}})
	}
}

// broadcast sends ev to every subscriber; s.mu must be held. A subscriber
// whose buffer is full is dropped so one slow client cannot stall the rest.
func (s *Server) broadcast(ev *pb.WatchDevicesResponse) {
	for sub := range s.subs {
		select {
		case sub.ch <- ev:
		default:
			sub.dropped = true
			close(sub.ch)
			delete(s.subs, sub)
		}
	}
}

func (s *Server) waitSynced(ctx context.Context) error {
	select {
	case <-s.synced:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (s *Server) snapshot() []*pb.Device {
	devs := make([]*pb.Device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, toProto(d, s.released[d.Address]))
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Address < devs[j].Address })
	return devs
}

// ListDevices implements the RPC.
func (s *Server) ListDevices(ctx context.Context, _ *pb.ListDevicesRequest) (*pb.ListDevicesResponse, error) {
	if err := s.waitSynced(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.ListDevicesResponse{Devices: s.snapshot()}, nil
}

// WatchDevices implements the RPC.
func (s *Server) WatchDevices(_ *pb.WatchDevicesRequest, stream grpc.ServerStreamingServer[pb.WatchDevicesResponse]) error {
	ctx := stream.Context()
	if err := s.waitSynced(ctx); err != nil {
		return err
	}

	sub := &subscriber{ch: make(chan *pb.WatchDevicesResponse, subscriberBuffer)}
	s.mu.Lock()
	initial := s.snapshot()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if !sub.dropped {
			delete(s.subs, sub)
		}
		s.mu.Unlock()
	}()

	s.log.Info("watch started", "peer", peerAddr(ctx))
	defer s.log.Info("watch ended", "peer", peerAddr(ctx))

	for _, d := range initial {
		if err := stream.Send(&pb.WatchDevicesResponse{Event: &pb.WatchDevicesResponse_Updated{Updated: d}}); err != nil {
			return err
		}
	}
	if err := stream.Send(&pb.WatchDevicesResponse{Event: &pb.WatchDevicesResponse_Synced{Synced: &pb.Synced{}}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.ch:
			if !ok {
				return status.Error(codes.ResourceExhausted, "watcher fell behind; reconnect to resync")
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// Connect implements the RPC.
func (s *Server) Connect(ctx context.Context, req *pb.ConnectRequest) (*pb.ConnectResponse, error) {
	if err := s.call(ctx, "connect", req.GetAddress(), s.backend.Connect); err != nil {
		return nil, err
	}
	return &pb.ConnectResponse{}, nil
}

// Disconnect implements the RPC.
func (s *Server) Disconnect(ctx context.Context, req *pb.DisconnectRequest) (*pb.DisconnectResponse, error) {
	if err := s.call(ctx, "disconnect", req.GetAddress(), s.backend.Disconnect); err != nil {
		return nil, err
	}
	return &pb.DisconnectResponse{}, nil
}

// call runs a connect or disconnect for a trusted device. Untrusted devices
// are reported as not found: the server only exposes trusted ones.
func (s *Server) call(ctx context.Context, op, address string, fn func(context.Context, string) error) error {
	return s.callMapped(ctx, op, address, fn, toStatus)
}

// callMapped is call with its own mapping of fn's errors to gRPC statuses.
func (s *Server) callMapped(ctx context.Context, op, address string, fn func(context.Context, string) error, toErr func(error) error) error {
	addr, err := bluez.NormalizeAddress(address)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.waitSynced(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	_, ok := s.devices[addr]
	s.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "no trusted device %s", addr)
	}

	ctx, cancel := context.WithTimeout(ctx, maxCallTime)
	defer cancel()
	s.log.Info(op, "device", addr, "peer", peerAddr(ctx))
	if err := fn(ctx, addr); err != nil {
		s.log.Warn(op+" failed", "device", addr, "err", err)
		return toErr(err)
	}
	return nil
}

// Release implements the RPC.
func (s *Server) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseResponse, error) {
	if s.audio == nil {
		return nil, status.Error(codes.Unimplemented, "audio handoff is not available on this host")
	}
	hold := time.Duration(req.GetHoldSeconds()) * time.Second
	if hold <= 0 {
		hold = defaultHold
	}
	hold = min(hold, maxHold)
	err := s.callMapped(ctx, "release", req.GetAddress(), func(ctx context.Context, addr string) error {
		return s.audio.Release(ctx, addr, hold)
	}, audioStatus)
	if err != nil {
		return nil, err
	}
	return &pb.ReleaseResponse{}, nil
}

// Restore implements the RPC.
func (s *Server) Restore(ctx context.Context, req *pb.RestoreRequest) (*pb.RestoreResponse, error) {
	if s.audio == nil {
		return nil, status.Error(codes.Unimplemented, "audio handoff is not available on this host")
	}
	if err := s.callMapped(ctx, "restore", req.GetAddress(), s.audio.Restore, audioStatus); err != nil {
		return nil, err
	}
	return &pb.RestoreResponse{}, nil
}

// audioStatus maps an audio manager error to a gRPC status.
func audioStatus(err error) error {
	switch {
	case errors.Is(err, audio.ErrNoCard):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, audio.ErrBusy):
		return status.Errorf(codes.FailedPrecondition, "another device is released: %v", err)
	}
	return toStatus(err)
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, bluez.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

func toProto(d bluez.Device, released bool) *pb.Device {
	p := &pb.Device{
		Address:    d.Address,
		Name:       d.Name,
		Alias:      d.Alias,
		Icon:       d.Icon,
		Paired:     d.Paired,
		Trusted:    d.Trusted,
		Connected:  d.Connected,
		AudioState: audioToProto(d.Audio),
		Released:   released,
	}
	if d.HasBattery {
		b := uint32(d.BatteryPercent)
		p.BatteryPercent = &b
	}
	return p
}

func audioToProto(a bluez.AudioState) pb.AudioState {
	switch a {
	case bluez.AudioIdle:
		return pb.AudioState_AUDIO_STATE_IDLE
	case bluez.AudioSending:
		return pb.AudioState_AUDIO_STATE_SENDING
	}
	return pb.AudioState_AUDIO_STATE_NONE
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		return p.Addr.String()
	}
	return "unknown"
}
