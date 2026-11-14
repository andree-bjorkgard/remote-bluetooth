package client

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/auth"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/fakebt"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/server"
)

var (
	headset  = bluez.Device{Address: "00:0A:45:19:F3:A6", Alias: "Headset", Trusted: true, HasBattery: true, BatteryPercent: 55, Audio: bluez.AudioSending}
	keyboard = bluez.Device{Address: "DD:57:25:0A:B2:F7", Alias: "Keyboard", Trusted: true}
)

// testServer is a real server with mutual TLS on a localhost port.
type testServer struct {
	addr    string
	backend *fakebt.Backend
	stop    func()
}

func startServer(t *testing.T, secret, addr string, backend *fakebt.Backend) *testServer {
	t.Helper()
	id, err := auth.New(secret)
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var lis net.Listener
	for i := 0; i < 50; i++ { // a restarted server may need to wait for the port
		if lis, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(backend, slog.New(slog.DiscardHandler))
	go srv.Run(ctx)
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(id.ServerConfig())))
	srv.Register(g)
	go g.Serve(lis)
	ts := &testServer{addr: lis.Addr().String(), backend: backend, stop: func() { cancel(); g.Stop() }}
	t.Cleanup(ts.stop)
	return ts
}

func startClient(t *testing.T, secret string, servers ...string) *Client {
	t.Helper()
	c, err := New(Config{Secret: secret, Servers: servers, DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		for range c.Events() {
		}
		<-done
	})
	return c
}

// next returns the next event matching want, failing after a timeout.
func next(t *testing.T, c *Client, want EventType) Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.Type == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	secret := auth.GenerateSecret()
	srv := startServer(t, secret, "", fakebt.New(headset, keyboard))
	c := startClient(t, secret, srv.addr)
	ctx := context.Background()

	if ev := next(t, c, ServerUp); ev.Server != srv.addr {
		t.Fatalf("ServerUp for %q", ev.Server)
	}
	devs := c.Devices()
	if len(devs) != 2 || devs[0].Alias != "Headset" || devs[0].BatteryPercent != 55 || devs[0].Server != srv.addr {
		t.Fatalf("Devices: %+v", devs)
	}
	if devs[0].Audio != bluez.AudioSending {
		t.Errorf("headset audio: %v", devs[0].Audio)
	}

	if err := c.Connect(ctx, srv.addr, headset.Address); err != nil {
		t.Fatal(err)
	}
	for {
		ev := next(t, c, DeviceUpdated)
		if ev.Device.Address == headset.Address && ev.Device.Connected {
			break
		}
	}

	err := c.Connect(ctx, srv.addr, "11:22:33:44:55:66")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("unknown device: want ErrDeviceNotFound, got %v", err)
	}
	if err := c.Connect(ctx, "nope:1", headset.Address); !errors.Is(err, ErrUnknownServer) {
		t.Errorf("unknown server: want ErrUnknownServer, got %v", err)
	}
}

func TestServerRestart(t *testing.T) {
	secret := auth.GenerateSecret()
	srv := startServer(t, secret, "", fakebt.New(headset, keyboard))
	c := startClient(t, secret, srv.addr)
	next(t, c, ServerUp)

	srv.stop()
	if ev := next(t, c, ServerDown); ev.Err == nil {
		t.Error("ServerDown without error")
	}
	if len(c.Devices()) != 0 {
		t.Errorf("devices of a down server must be gone: %+v", c.Devices())
	}
	if err := c.Connect(context.Background(), srv.addr, headset.Address); !errors.Is(err, ErrServerUnavailable) {
		t.Errorf("want ErrServerUnavailable, got %v", err)
	}

	// The server comes back on the same address with one device fewer.
	startServer(t, secret, srv.addr, fakebt.New(keyboard))
	next(t, c, ServerUp)
	if devs := c.Devices(); len(devs) != 1 || devs[0].Address != keyboard.Address {
		t.Fatalf("after restart: %+v", devs)
	}
}

func TestWrongSecretNeverConnects(t *testing.T) {
	srv := startServer(t, auth.GenerateSecret(), "", fakebt.New(headset))
	c := startClient(t, auth.GenerateSecret(), srv.addr)

	select {
	case ev := <-c.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(1500 * time.Millisecond):
	}
	if len(c.Servers()) != 0 || len(c.Devices()) != 0 {
		t.Fatalf("connected despite wrong secret: %v %v", c.Servers(), c.Devices())
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Secret: "short", Servers: []string{"x:1"}}); !errors.Is(err, auth.ErrWeakSecret) {
		t.Errorf("weak secret: %v", err)
	}
	if _, err := New(Config{Secret: auth.GenerateSecret(), DisableDiscovery: true}); err == nil {
		t.Error("no servers and no discovery should fail")
	}
}

func TestWithoutLocalIPs(t *testing.T) {
	got := withoutLocalIPs([]string{"127.0.0.1:8829", "192.0.2.10:8829", "garbage"})
	if len(got) != 1 || got[0] != "192.0.2.10:8829" {
		t.Errorf("got %v", got)
	}
}

func TestParseServers(t *testing.T) {
	got := ParseServers(" desktop, 10.0.0.2:9000,,[fd00::1], [fd00::2]:7 ")
	want := []string{"desktop:8829", "10.0.0.2:9000", "[fd00::1]:8829", "[fd00::2]:7"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v want %v", got, want)
		}
	}
}
