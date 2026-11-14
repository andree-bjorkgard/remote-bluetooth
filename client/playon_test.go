package client

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/auth"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/fakebt"
)

// hereAndThere starts the server on this machine (not followed, as usual)
// and a remote one the client follows.
func hereAndThere(t *testing.T, there bluez.Device) (c *Client, here *fakebt.Audio, remote string) {
	secret := auth.GenerateSecret()
	here = fakebt.NewAudio()
	local := startServer(t, secret, "", fakebt.New(sendingHeadset()), here)
	s := startServer(t, secret, "", fakebt.New(there), fakebt.NewAudio())
	c, err := New(Config{Secret: secret, Servers: []string{s.addr}, DisableDiscovery: true, LocalServer: local.addr})
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
	next(t, c, ServerUp)
	return c, here, s.addr
}

func TestPlayOn(t *testing.T) {
	fastPlayHere(t)
	c, here, there := hereAndThere(t, connected(sendingHeadset()))
	if err := c.PlayOn(context.Background(), there, headset.Address); err != nil {
		t.Fatal(err)
	}
	want := []string{"release " + headset.Address + " 10s", "restore " + headset.Address}
	if got := here.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls here: %v, want %v", got, want)
	}
}

func TestPlayOnNothingThere(t *testing.T) {
	fastPlayHere(t)
	c, here, there := hereAndThere(t, connected(idleHeadset()))
	err := c.PlayOn(context.Background(), there, headset.Address)
	if !errors.Is(err, ErrNothingPlayingThere) {
		t.Fatalf("err = %v", err)
	}
	// This machine takes the sound back at once, not at hold expiry.
	want := []string{"release " + headset.Address + " 10s", "restore " + headset.Address}
	if got := here.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls here: %v, want %v", got, want)
	}
}

func TestPlayOnNotConnectedThere(t *testing.T) {
	fastPlayHere(t)
	c, here, there := hereAndThere(t, idleHeadset())
	if err := c.PlayOn(context.Background(), there, headset.Address); !errors.Is(err, ErrNotConnectedThere) {
		t.Fatalf("err = %v", err)
	}
	if got := here.Calls(); len(got) != 0 {
		t.Errorf("calls here: %v", got)
	}
}

func connected(d bluez.Device) bluez.Device {
	d.Connected = true
	return d
}
