package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/config"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/discovery"
)

func main() {
	cfg := config.NewConfig()

	ifs, err := net.Interfaces()
	if err != nil {
		panic(err)
	}

	discoveryService := discovery.NewDiscoveryService(cfg.BroadcastPort, cfg.BroadcastMessage, cfg.BroadcastServerResponse)

	if err != nil {
		panic(err)
	}

	var broadcastIPs []net.IP
	for _, i := range ifs {
		addr, err := i.Addrs()
		if err != nil {
			panic(err)
		}
		for _, a := range addr {
			ipAddr, ok := a.(*net.IPNet)
			// Ignore loopback and ipv6 addresses
			if ok && !ipAddr.IP.IsLoopback() && strings.Count(ipAddr.IP.String(), ":") < 2 {
				broadcastIPs = append(broadcastIPs, subnetBroadcastIP(*ipAddr))
			}
		}
	}

	ch := discoveryService.Discover(broadcastIPs)

	for {
		addr := <-ch
		fmt.Println(addr)
	}
}

func subnetBroadcastIP(ipnet net.IPNet) net.IP {
	byteIp := []byte(ipnet.IP)
	byteMask := []byte(ipnet.Mask)
	// Using bytemask for length instead because IP is padded with zeros
	byteBroadCastIP := make([]byte, len(byteMask))
	byteIp = byteIp[len(byteIp)-len(byteMask):]

	for k := range byteIp {
		// mask will give us all fixed bits of the subnet (for the given byte)
		// inverted mask will give us all moving bits of the subnet (for the given byte)
		invertedMask := byteMask[k] ^ 0xff // inverted mask byte
		// broadcastIP = networkIP added to the inverted mask
		byteBroadCastIP[k] = byteIp[k]&byteMask[k] | invertedMask
	}

	return net.IP(byteBroadCastIP)
}
