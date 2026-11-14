// Package discovery finds remote-bluetooth servers on the local network with
// mDNS / DNS-SD, the same mechanism printers and Chromecasts use.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/libp2p/zeroconf/v2"
)

// ServiceType is the DNS-SD service type servers announce.
const ServiceType = "_remote-bt._tcp"

const domain = "local."

// HostID identifies this machine without revealing its machine-id. Clients
// use it to recognise the server running on their own host.
func HostID() string {
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(strings.TrimSpace(string(id))) == 0 {
		h, _ := os.Hostname()
		id = []byte(h)
	}
	sum := sha256.Sum256([]byte("remote-bluetooth host " + strings.TrimSpace(string(id))))
	return hex.EncodeToString(sum[:8])
}

// Announce advertises a server named instance on port until ctx is done.
func Announce(ctx context.Context, instance string, port int) error {
	srv, err := zeroconf.Register(instance, ServiceType, domain, port, []string{"v=2", "host=" + HostID()}, nil)
	if err != nil {
		return fmt.Errorf("discovery: announce: %w", err)
	}
	<-ctx.Done()
	srv.Shutdown() // sends a goodbye so caches drop us right away
	return nil
}

// Service is one server found on the network.
type Service struct {
	// Instance is the server's name, by default its hostname.
	Instance string
	// HostID is the announcing machine's HostID.
	HostID string
	// Addrs are host:port pairs, IPv4 first.
	Addrs []string
}

// Browse reports servers until ctx is done, then closes the channel. A
// server is reported again when its announcement is refreshed, possibly
// with new addresses. If browsing cannot start (no multicast-capable
// interface) onErr is called and the channel closes.
func Browse(ctx context.Context, onErr func(error)) <-chan Service {
	entries := make(chan *zeroconf.ServiceEntry, 16)
	out := make(chan Service, 16)
	errc := make(chan error, 1)
	go func() { errc <- zeroconf.Browse(ctx, ServiceType, domain, entries) }()

	go func() {
		defer close(out)
		for {
			select {
			case e, ok := <-entries:
				if !ok {
					return
				}
				svc := Service{Instance: unescape(e.Instance)}
				for _, kv := range e.Text {
					if v, ok := strings.CutPrefix(kv, "host="); ok {
						svc.HostID = v
					}
				}
				for _, ip := range e.AddrIPv4 {
					svc.Addrs = append(svc.Addrs, net.JoinHostPort(ip.String(), strconv.Itoa(e.Port)))
				}
				for _, ip := range e.AddrIPv6 {
					if ip.IsLinkLocalUnicast() {
						continue // would need a zone we do not know
					}
					svc.Addrs = append(svc.Addrs, net.JoinHostPort(ip.String(), strconv.Itoa(e.Port)))
				}
				if len(svc.Addrs) == 0 {
					continue
				}
				select {
				case out <- svc:
				case <-ctx.Done():
					return
				}
			case err := <-errc:
				if err != nil && ctx.Err() == nil {
					onErr(fmt.Errorf("discovery: browse: %w", err))
					return
				}
				errc = nil // finished cleanly; wait for entries to close
			}
		}
	}()
	return out
}

// unescape undoes DNS label escaping such as "my\ host".
func unescape(s string) string {
	return strings.ReplaceAll(s, `\`, "")
}
