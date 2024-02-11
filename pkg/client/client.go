package client

import (
	"errors"
	"log"
	"net"
	"strings"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/discovery"
	"github.com/andree-bjorkgard/remote-bluetooth/pkg/config"
)

var (
	ErrServerNotFound = errors.New("server not found")
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
	Device *Device
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

	var ignoreList []net.IP
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
				ignoreList = append(ignoreList, ipAddr.IP)
				broadcastIPs = append(broadcastIPs, subnetBroadcastIP(*ipAddr))
			}
		}
	}

	ch := discoveryService.Discover(broadcastIPs)

main:
	for {
		addr := <-ch
		addrSplit := strings.Split(addr, ":")
		if len(addrSplit) != 2 {
			log.Println("Invalid address: ", addr)
			continue
		}

		for _, ip := range ignoreList {
			if ip.String() == addrSplit[0] {
				log.Println("Ignoring local address: ", addr)
				continue main
			}
		}

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

func (c *Client) ConnectToDevice(server, address string) error {
	bc, ok := c.connections[server]
	if !ok {
		return ErrServerNotFound
	}

	return bc.ConnectToDevice(address)
}

func (c *Client) DisconnectFromDevice(server, address string) error {
	bc, ok := c.connections[server]
	if !ok {
		return ErrServerNotFound
	}

	return bc.DisconnectFromDevice(address)
}

func (c *Client) GetDeviceEventsChannel() <-chan DeviceEvent {
	return c.channel
}

func grpcDeviceToClientDevice(d *grpc.Device) *Device {
	return &Device{
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

func (d *Device) GetAlias() (string, error) {
	return d.Name, nil
}

func (d *Device) GetAddress() (string, error) {
	return d.Address, nil
}

func (d *Device) GetTrusted() (bool, error) {
	return d.Trusted, nil
}

func (d *Device) GetPaired() (bool, error) {
	return d.Paired, nil
}

func (d *Device) GetConnected() (bool, error) {
	return d.Connected, nil
}

func (d *Device) GetBatteryStatus() (string, error) {
	return d.BatteryStatus, nil
}

func (d *Device) GetIcon() (string, error) {
	return d.Icon, nil
}
