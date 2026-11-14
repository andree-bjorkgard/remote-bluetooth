package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
)

var (
	// ErrNotConnectedThere is returned by PlayOn when the device is not
	// connected on the target server.
	ErrNotConnectedThere = errors.New("client: the device is not connected there")
	// ErrNothingPlayingThere is returned by PlayOn when the target server
	// did not start sending in time. This machine has taken the sound back.
	ErrNothingPlayingThere = errors.New("client: nothing is playing there")
)

// PortEnv is the environment variable that sets the server port.
const PortEnv = "REMOTE_BLUETOOTH_PORT"

// LocalServer is the address of the server on this machine:
// localhost on $REMOTE_BLUETOOTH_PORT, or DefaultPort.
func LocalServer() string {
	port := strconv.Itoa(DefaultPort)
	if p := strings.TrimSpace(os.Getenv(PortEnv)); p != "" {
		port = p
	}
	return net.JoinHostPort("localhost", port)
}

// PlayOn moves the sound for the device from this machine to server. The
// server on this machine (Config.LocalServer) lets go, PlayOn waits until
// server sends to the device, and after a short settle this machine takes
// its audio back. If server never sends, this machine takes it back at once
// and PlayOn returns ErrNothingPlayingThere.
func (c *Client) PlayOn(ctx context.Context, server, address string) error {
	if _, err := c.server(server); err != nil {
		return err
	}
	connected := false
	for _, d := range c.Devices() {
		if d.Server == server && d.Address == address && d.Connected {
			connected = true
		}
	}
	if !connected {
		return ErrNotConnectedThere
	}

	// The local server is not followed (it is usually skipped), so it gets
	// a connection of its own for the two calls.
	local, err := newServerConn(c, "this computer", []string{c.cfg.LocalServer})
	if err != nil {
		return err
	}
	defer local.conn.Close()
	if err := local.release(ctx, address, playHereHold); err != nil {
		return err
	}

	sendingThere := func(context.Context) bool {
		for _, d := range c.Devices() {
			if d.Server == server && d.Address == address && d.Audio == bluez.AudioSending {
				return true
			}
		}
		return false
	}
	waitErr := waitFor(ctx, playHereWait, playHerePoll, sendingThere)
	if waitErr == nil {
		waitErr = sleep(ctx, playHereSettle)
	}
	if errors.Is(waitErr, ErrNothingPlayingHere) {
		waitErr = ErrNothingPlayingThere
	}

	// Take the audio back even if the caller has gone away; the hold would
	// otherwise keep this machine silent until it runs out.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	if err := local.restore(rctx, address); err != nil {
		return errors.Join(waitErr, fmt.Errorf("this computer will restore itself when the hold runs out: %w", err))
	}
	return waitErr
}
