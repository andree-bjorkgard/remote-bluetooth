package client

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
)

var (
	// ErrNotHeldElsewhere is returned by PlayHere when no other server has
	// the device connected with audio to let go of.
	ErrNotHeldElsewhere = errors.New("client: no other computer has this device connected")
	// ErrNothingPlayingHere is returned by PlayHere when this machine did not
	// start sending in time.
	ErrNothingPlayingHere = errors.New("client: nothing is playing here")
)

// Variables so tests can shorten them.
var (
	playHereHold   = 15 * time.Second       // how long each holder lets go
	playHereWait   = 15 * time.Second       // how long to wait for this machine to send
	playHereSettle = 3 * time.Second        // pause between sending here and restoring
	playHerePoll   = 200 * time.Millisecond // how often to check sendingHere
	restoreTimeout = 10 * time.Second       // bound for the final Restore calls
)

// PlayHereResult reports what PlayHere did on each server.
type PlayHereResult struct {
	// Released lists the servers that let go and were restored, sorted.
	Released []string
	// Failed maps a server to the Release or Restore error it hit.
	Failed map[string]error
}

// Release asks server to let go of the device's audio for hold.
func (c *Client) Release(ctx context.Context, server, address string, hold time.Duration) error {
	s, err := c.server(server)
	if err != nil {
		return err
	}
	return s.release(ctx, address, hold)
}

// Restore asks server to take back audio it released.
func (c *Client) Restore(ctx context.Context, server, address string) error {
	s, err := c.server(server)
	if err != nil {
		return err
	}
	return s.restore(ctx, address)
}

// PlayHere moves the sound for the device to this machine. Every server
// with the device connected lets go, sending or not: a headset can stay
// with an idle source and ignore this machine until that source lets go.
// A server with no audio card for the device has nothing to hold and is
// skipped. Then sendingHere is polled until this machine sends,
// and after a short settle the holders restore their audio. If this machine
// never sends, the holders are left to restore on their own at hold expiry.
func (c *Client) PlayHere(ctx context.Context, address string, sendingHere func(context.Context) bool) (PlayHereResult, error) {
	res := PlayHereResult{Failed: map[string]error{}}
	var holders []string
	noTransport := map[string]bool{}
	for _, d := range c.Devices() {
		if d.Address == address && d.Connected {
			holders = append(holders, d.Server)
			noTransport[d.Server] = d.Audio == bluez.AudioNone
		}
	}
	if len(holders) == 0 {
		return res, ErrNotHeldElsewhere
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.Release(ctx, name, address, playHereHold)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil && noTransport[name] && errors.Is(err, ErrNoAudio):
				// No audio card there, so nothing to let go of.
			case err != nil:
				res.Failed[name] = err
			default:
				res.Released = append(res.Released, name)
			}
		}()
	}
	wg.Wait()
	sort.Strings(res.Released)
	if len(res.Released) == 0 {
		if len(res.Failed) == 0 {
			return res, ErrNotHeldElsewhere
		}
		var errs []error
		for _, name := range holders {
			if err := res.Failed[name]; err != nil {
				errs = append(errs, err)
			}
		}
		return res, errors.Join(errs...)
	}

	if err := waitFor(ctx, playHereWait, playHerePoll, sendingHere); err != nil {
		return res, err
	}
	if err := sleep(ctx, playHereSettle); err != nil {
		return res, err
	}

	// The switch happened; restore even if the caller has gone away.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	for _, name := range res.Released {
		if err := c.Restore(rctx, name, address); err != nil {
			res.Failed[name] = err
		}
	}
	return res, nil
}

// waitFor polls cond every poll until it is true. It returns
// ErrNothingPlayingHere after timeout and ctx.Err() if ctx ends first.
func waitFor(ctx context.Context, timeout, poll time.Duration, cond func(context.Context) bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if cond(ctx) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrNothingPlayingHere
		case <-tick.C:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
