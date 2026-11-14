package client

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/auth"
	"github.com/andree-bjorkgard/remote-bluetooth/v2/internal/fakebt"
)

func fastPlayHere(t *testing.T) {
	oh, ow, os, op := playHereHold, playHereWait, playHereSettle, playHerePoll
	playHereHold, playHereWait, playHereSettle, playHerePoll = 10*time.Second, 300*time.Millisecond, 30*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { playHereHold, playHereWait, playHereSettle, playHerePoll = oh, ow, os, op })
}

func idleHeadset() bluez.Device {
	d := headset
	d.Audio = bluez.AudioIdle
	return d
}

func sendingHeadset() bluez.Device { return headset }

// twoServers starts two servers and a client that sees both.
func twoServers(t *testing.T, d1, d2 bluez.Device, a1, a2 *fakebt.Audio) (*Client, *testServer, *testServer) {
	secret := auth.GenerateSecret()
	s1 := startServer(t, secret, "", fakebt.New(d1), a1)
	s2 := startServer(t, secret, "", fakebt.New(d2), a2)
	c := startClient(t, secret, s1.addr, s2.addr)
	next(t, c, ServerUp)
	next(t, c, ServerUp)
	return c, s1, s2
}

func TestPlayHere(t *testing.T) {
	fastPlayHere(t)
	a1, a2 := fakebt.NewAudio(), fakebt.NewAudio()
	c, s1, _ := twoServers(t, sendingHeadset(), idleHeadset(), a1, a2)

	res, err := c.PlayHere(context.Background(), headset.Address, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Released, []string{s1.addr}) || len(res.Failed) != 0 {
		t.Fatalf("result: %+v", res)
	}
	want := []string{"release " + headset.Address + " 10s", "restore " + headset.Address}
	if got := a1.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("server 1 calls: %v, want %v", got, want)
	}
	if got := a2.Calls(); len(got) != 0 {
		t.Errorf("server 2 touched: %v", got)
	}
}

func TestPlayHereNotHeld(t *testing.T) {
	fastPlayHere(t)
	a1, a2 := fakebt.NewAudio(), fakebt.NewAudio()
	c, _, _ := twoServers(t, idleHeadset(), idleHeadset(), a1, a2)

	_, err := c.PlayHere(context.Background(), headset.Address, func(context.Context) bool { return true })
	if !errors.Is(err, ErrNotHeldElsewhere) {
		t.Fatalf("err = %v", err)
	}
	if len(a1.Calls())+len(a2.Calls()) != 0 {
		t.Errorf("calls: %v %v", a1.Calls(), a2.Calls())
	}
}

func TestPlayHereNothingHere(t *testing.T) {
	fastPlayHere(t)
	a1, a2 := fakebt.NewAudio(), fakebt.NewAudio()
	c, _, _ := twoServers(t, sendingHeadset(), idleHeadset(), a1, a2)

	_, err := c.PlayHere(context.Background(), headset.Address, func(context.Context) bool { return false })
	if !errors.Is(err, ErrNothingPlayingHere) {
		t.Fatalf("err = %v", err)
	}
	want := []string{"release " + headset.Address + " 10s"}
	if got := a1.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls: %v, want %v", got, want)
	}
}

func TestPlayHereOneHolderUnreachable(t *testing.T) {
	fastPlayHere(t)
	a1, a2 := fakebt.NewAudio(), fakebt.NewAudio()
	a2.FailRelease = errors.New("boom")
	c, s1, s2 := twoServers(t, sendingHeadset(), sendingHeadset(), a1, a2)

	res, err := c.PlayHere(context.Background(), headset.Address, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Released, []string{s1.addr}) {
		t.Errorf("Released: %v", res.Released)
	}
	if res.Failed[s2.addr] == nil || len(res.Failed) != 1 {
		t.Errorf("Failed: %v", res.Failed)
	}
	if got := a1.Calls(); len(got) != 2 || got[1] != "restore "+headset.Address {
		t.Errorf("server 1 calls: %v", got)
	}
}

func TestPlayHereCancelledDoesNotRestore(t *testing.T) {
	fastPlayHere(t)
	a1 := fakebt.NewAudio()
	c, _, _ := twoServers(t, sendingHeadset(), idleHeadset(), a1, fakebt.NewAudio())

	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.PlayHere(ctx, headset.Address, func(context.Context) bool { cancel(); return false })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := a1.Calls(); len(got) != 1 {
		t.Errorf("calls: %v", got)
	}
}
