// Package discovery finds remote-bluetooth servers on the local network with
// mDNS / DNS-SD, the same mechanism printers and Chromecasts use.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

// recheck is how often Announce looks for changed interface addresses.
var recheck = 30 * time.Second

// Announce advertises a server named instance on port until ctx is done.
// It announces on the machine's physical network interfaces only, so
// container bridges and other virtual interfaces never hand clients an
// address they cannot reach, and announces again when those interfaces'
// addresses change (a new Wi-Fi network, a new DHCP lease). Problems are
// logged and retried; Announce returns when ctx is done.
func Announce(ctx context.Context, instance string, port int, log *slog.Logger) {
	tick := time.NewTicker(recheck)
	defer tick.Stop()
	var srv *zeroconf.Server
	announced := ""
	for {
		ifaces := announceInterfaces()
		if key := addrKey(ifaces); key != announced || srv == nil {
			if srv != nil {
				srv.Shutdown()
				srv = nil
			}
			s, err := zeroconf.Register(instance, ServiceType, domain, port, []string{"v=2", "host=" + HostID()}, ifaces)
			if err != nil {
				log.Warn("mDNS announce failed, will retry", "err", err)
			} else {
				srv, announced = s, key
				log.Info("announced with mDNS", "addrs", key)
			}
		}
		select {
		case <-ctx.Done():
			if srv != nil {
				srv.Shutdown() // sends a goodbye so caches drop us right away
			}
			return
		case <-tick.C:
		}
	}
}

// announceInterfaces lists the interfaces to announce on: up, multicast,
// backed by hardware (/sys/class/net/<name>/device exists, which rules out
// docker0, br-*, veth*, virbr*, VPN tunnels...) and holding an address.
// Without any it falls back to every up multicast interface with an
// address, e.g. inside a container.
func announceInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var physical, usable []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(ifaceIPs(ifi)) == 0 {
			continue
		}
		usable = append(usable, ifi)
		if _, err := os.Stat(filepath.Join("/sys/class/net", ifi.Name, "device")); err == nil {
			physical = append(physical, ifi)
		}
	}
	if len(physical) > 0 {
		return physical
	}
	return usable
}

// ifaceIPs are an interface's addresses worth announcing: not loopback and
// not IPv6 link-local, which needs a zone clients do not know.
func ifaceIPs(ifi net.Interface) []string {
	addrs, _ := ifi.Addrs()
	var ips []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ipn.IP.String())
	}
	return ips
}

// addrKey sums up the interfaces' addresses, to notice when they change.
func addrKey(ifaces []net.Interface) string {
	var parts []string
	for _, ifi := range ifaces {
		parts = append(parts, ifi.Name+"="+strings.Join(ifaceIPs(ifi), ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
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
