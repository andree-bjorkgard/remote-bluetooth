package server

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/fakebt"
	pb "github.com/andree-bjorkgard/remote-bluetooth/v2/internal/pb/remotebtv1"
)

var (
	headset  = bluez.Device{Address: "00:0A:45:19:F3:A6", Alias: "Headset", Trusted: true, Paired: true, HasBattery: true, BatteryPercent: 70}
	keyboard = bluez.Device{Address: "DD:57:25:0A:B2:F7", Alias: "Keyboard", Trusted: true, Paired: true}
	stranger = bluez.Device{Address: "58:18:62:63:08:B3", Alias: "Phone"}
)

// start runs a Server over an in-memory connection. Transport security is
// covered by package auth; here we test the service itself.
func start(t *testing.T, backend Backend) pb.BluetoothServiceClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := New(backend, slog.New(slog.DiscardHandler))
	go func() {
		if err := srv.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	srv.Register(g)
	go g.Serve(lis)
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewBluetoothServiceClient(conn)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestListDevicesOnlyTrusted(t *testing.T) {
	c := start(t, fakebt.New(headset, keyboard, stranger))
	resp, err := c.ListDevices(ctxT(t), &pb.ListDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) != 2 {
		t.Fatalf("want 2 trusted devices, got %v", resp.Devices)
	}
	if resp.Devices[0].Address != headset.Address || resp.Devices[0].GetBatteryPercent() != 70 {
		t.Errorf("first device: %v", resp.Devices[0])
	}
	if resp.Devices[1].BatteryPercent != nil {
		t.Errorf("keyboard should have no battery: %v", resp.Devices[1])
	}
}

func recv(t *testing.T, s grpc.ServerStreamingClient[pb.WatchDevicesResponse]) *pb.WatchDevicesResponse {
	t.Helper()
	msg, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return msg
}

func TestWatchDevices(t *testing.T) {
	fb := fakebt.New(headset, stranger)
	c := start(t, fb)
	ctx := ctxT(t)
	s, err := c.WatchDevices(ctx, &pb.WatchDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if got := recv(t, s).GetUpdated(); got.GetAddress() != headset.Address {
		t.Fatalf("snapshot: %v", got)
	}
	if recv(t, s).GetSynced() == nil {
		t.Fatal("want synced after snapshot")
	}

	// Noise: an untrusted device changing must not be sent.
	fb.Emit(bluez.Change{Device: bluez.Device{Address: stranger.Address, Connected: true}})

	// Connecting produces an update.
	if _, err := c.Connect(ctx, &pb.ConnectRequest{Address: "00:0a:45:19:f3:a6"}); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, s).GetUpdated(); got.GetAddress() != headset.Address || !got.Connected {
		t.Fatalf("after connect: %v", got)
	}

	// A newly trusted device shows up.
	nowTrusted := stranger
	nowTrusted.Trusted = true
	fb.Emit(bluez.Change{Device: nowTrusted})
	if got := recv(t, s).GetUpdated(); got.GetAddress() != stranger.Address {
		t.Fatalf("newly trusted: %v", got)
	}

	// Revoking trust removes it.
	fb.Emit(bluez.Change{Device: stranger})
	if got := recv(t, s).GetRemoved(); got != stranger.Address {
		t.Fatalf("untrusted: %q", got)
	}

	// Forgetting a device removes it.
	fb.Emit(bluez.Change{Device: bluez.Device{Address: headset.Address}, Removed: true})
	if got := recv(t, s).GetRemoved(); got != headset.Address {
		t.Fatalf("removed: %q", got)
	}
}

func TestConnectValidation(t *testing.T) {
	fb := fakebt.New(headset, stranger)
	c := start(t, fb)
	ctx := ctxT(t)

	cases := []struct {
		addr string
		code codes.Code
	}{
		{"not-an-address", codes.InvalidArgument},
		{"/org/bluez/hci0", codes.InvalidArgument},
		{stranger.Address, codes.NotFound}, // untrusted devices are not exposed
		{"11:22:33:44:55:66", codes.NotFound},
	}
	for _, tc := range cases {
		_, err := c.Connect(ctx, &pb.ConnectRequest{Address: tc.addr})
		if status.Code(err) != tc.code {
			t.Errorf("Connect(%q): want %v, got %v", tc.addr, tc.code, err)
		}
	}
	if len(fb.Calls()) != 0 {
		t.Errorf("backend should not be called for rejected requests: %v", fb.Calls())
	}

	if _, err := c.Disconnect(ctx, &pb.DisconnectRequest{Address: headset.Address}); err != nil {
		t.Fatal(err)
	}
	if len(fb.Calls()) != 1 || fb.Calls()[0] != "disconnect "+headset.Address {
		t.Errorf("calls: %v", fb.Calls())
	}
}

func TestSlowWatcherIsDropped(t *testing.T) {
	fb := fakebt.New(headset)
	c := start(t, fb)
	ctx := ctxT(t)
	s, err := c.WatchDevices(ctx, &pb.WatchDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	recv(t, s)
	recv(t, s)

	// Flood without reading. gRPC's own buffers absorb some, the server's
	// per-watcher buffer the rest, then the watcher must be cut loose
	// instead of blocking the server.
	go func() {
		for i := 0; i < 20000; i++ {
			d := headset
			d.BatteryPercent = uint8(i % 100)
			d.Connected = i%2 == 0
			select {
			case fb.Changes() <- bluez.Change{Device: d}:
			case <-ctx.Done():
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	for {
		_, err := s.Recv()
		if err != nil {
			if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("want ResourceExhausted, got %v", err)
			}
			return
		}
	}
}
