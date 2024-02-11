package discovery

import (
	"bytes"
	"fmt"
	"log"
	"net"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/util"
)

type DiscoveryService struct {
	BroadcastPort int

	BroadcastMessage        []byte
	BroadcastServerResponse []byte
}

func NewDiscoveryService(broadcastPort int, broadcastMessage, broadcastServerResponse []byte) DiscoveryService {
	return DiscoveryService{
		BroadcastPort: broadcastPort,

		BroadcastMessage:        broadcastMessage,
		BroadcastServerResponse: broadcastServerResponse,
	}
}

// Client
func (s *DiscoveryService) listenForServers() (addr <-chan string, port int) {
	ch := make(chan string, 10)
	port, err := util.GetFreePort()
	if err != nil {
		log.Fatalf("DiscoveryService.listenForServers: %s", err)
	}

	go func(chan string) {
		for {
			msg, addr, err := s.listen(port)
			if err != nil {
				log.Printf("DiscoveryService.listenForServers: %s", err)
				continue
			}

			if bytes.Equal(msg.getMessageBytes(), s.BroadcastServerResponse) {
				addr.Port = msg.getPort()
				ch <- addr.String()
			}
		}
	}(ch)

	return ch, port
}

// client
func (s *DiscoveryService) askForServers(broadcastIP net.IP, listenerPort int) error {
	port, err := util.GetFreePort()
	if err != nil {
		return fmt.Errorf("DiscoveryService.askForServers: %s", err)
	}

	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("DiscoveryService.askForServers: error while listening for packet: %w", err)
	}
	defer pc.Close()

	addr := fmt.Sprintf("%s:%d", broadcastIP, s.BroadcastPort)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("DiscoveryService.askForServers: error while resolving udp address: %w", err)
	}

	msg := message(s.BroadcastMessage)
	msg.setPort(listenerPort)

	_, err = pc.WriteTo(msg, udpAddr)
	if err != nil {
		return fmt.Errorf("DiscoveryService.askForServers: error while writing packet: %w", err)
	}

	return nil
}

// client
func (s *DiscoveryService) Discover(broadcastIPs []net.IP) (addr <-chan string) {
	ch, port := s.listenForServers()

	for _, ip := range broadcastIPs {
		err := s.askForServers(ip, port)
		if err != nil {
			log.Printf("DiscoveryService.Discover: %s", err)
		}
	}

	return ch
}

// Server
func (s *DiscoveryService) StartServerAnnouncer(serverPort int) {
	for {
		msg, addr, err := s.listen(s.BroadcastPort)
		if err != nil {
			log.Panicf("DiscoveryService.StartServerAnnouncer: %s", err)
			continue
		}

		log.Printf("DiscoveryService.StartServerAnnouncer: received message from %s", addr.String())

		if bytes.Equal(msg.getMessageBytes(), s.BroadcastMessage) {
			addr.Port = msg.getPort()
			go s.announce(serverPort, addr)
		}
	}
}

// Server
func (s *DiscoveryService) announce(serverPort int, addr net.Addr) {
	pc, err := net.ListenPacket("udp4", "")
	if err != nil {
		log.Fatalf("DiscoveryService.Announce: error while listening for packet: %s", err)
	}
	defer pc.Close()

	msg := message(s.BroadcastServerResponse)
	msg.setPort(serverPort)

	_, err = pc.WriteTo(msg, addr)
	if err != nil {
		log.Printf("DiscoveryService.Announce: error while writing packet: %s", err)
	}
}

func (s *DiscoveryService) listen(port int) (message, *net.UDPAddr, error) {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, nil, fmt.Errorf("DiscoveryService.Listen: error while listening for packet: %w", err)
	}
	defer pc.Close()

	log.Printf("DiscoveryService.Listen: listening on port %d", port)

	buf := make([]byte, 1024)
	n, addr, err := pc.ReadFrom(buf)
	if err != nil {
		return nil, nil, fmt.Errorf("DiscoveryService.Listen: error while reading packet: %w", err)
	}

	ad, ok := addr.(*net.UDPAddr)
	if !ok {
		return nil, nil, fmt.Errorf("DiscoveryService.Listen: Not a udp address")
	}

	return buf[:n], ad, nil
}

func padLeftWithZeros(bytes []byte, maxByteLen int) []byte {
	for {
		if len(bytes) <= maxByteLen {
			return bytes
		}
		bytes = append([]byte("0"), bytes...)
	}
}
