package client

import (
	"log"
	"net"
	"strings"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/discovery"
	"github.com/andree-bjorkgard/remote-bluetooth/pkg/config"
)

type Device struct {
	Name          string
	Address       string
	Trusted       bool
	Paired        bool
	Connected     bool
	BatteryStatus string
	Icon          string
}

type DeviceEvent struct {
	Server string
	Device Device
}

type Client struct {
	cfg         config.Config
	connections map[string]*bluetooth.BluetoothClient
	channel     chan DeviceEvent
}

func NewClient(cfg config.Config) *Client {
	ch := make(chan DeviceEvent, 20)

	return &Client{cfg: cfg, channel: ch, connections: make(map[string]*bluetooth.BluetoothClient)}
}

func (c *Client) FindServers() {
	ifs, err := net.Interfaces()
	if err != nil {
		panic(err)
	}

	discoveryService := discovery.NewDiscoveryService(c.cfg.BroadcastPort, c.cfg.BroadcastMessage, c.cfg.BroadcastServerResponse)

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
		bc, err := bluetooth.NewBluetoothClient(addr, c.cfg.AuthenticationSecret)
		if err != nil {
			log.Println("Error creating client: ", err)
			continue
		}

		c.connections[addr] = bc
		ds, err := bc.GetTrustedDevices()
		if err != nil {
			log.Println("Error getting trusted devices: ", err)
			continue
		}
		for _, d := range ds {
			c.channel <- DeviceEvent{Server: addr, Device: grpcDeviceToClientDevice(d)}
		}
	}
}

func (c *Client) GetDeviceEventsChannel() <-chan DeviceEvent {
	return c.channel
}

func grpcDeviceToClientDevice(d *grpc.Device) Device {
	return Device{
		Name:          d.Name,
		Address:       d.Address,
		Trusted:       d.Trusted,
		Paired:        d.Paired,
		Connected:     d.Connected,
		BatteryStatus: d.BatteryStatus,
		Icon:          d.Icon,
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
