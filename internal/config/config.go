package config

import (
	"os"
	"strconv"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/util"
	_ "github.com/joho/godotenv/autoload"
)

type Config struct {
	// Server
	Port                 int
	AuthenticationSecret string

	// Bluetooth
	AdapterID string

	// Discovery
	BroadcastPort           int
	BroadcastMessage        []byte
	BroadcastServerResponse []byte
}

const broadcastMessage = "bt-discovery"
const broadcastServerResponse = "bt-discovery-server"

func NewConfig() Config {
	var port int
	var err error

	if tmpPort := os.Getenv("REMOTE_BLUETOOTH_PORT"); tmpPort != "" {
		port, err = strconv.Atoi(tmpPort)
		if err != nil {
			panic(err)
		}
	} else {
		port, err = util.GetFreePort()
		if err != nil {
			panic(err)
		}
	}

	tmpPort := os.Getenv("REMOTE_BLUETOOTH_BROADCAST_PORT")
	if tmpPort == "" {
		tmpPort = "8829"
	}

	broadcastPort, err := strconv.Atoi(tmpPort)
	if err != nil {
		panic(err)
	}

	secret := os.Getenv("REMOTE_BLUETOOTH_SECRET")

	msg := os.Getenv("REMOTE_BLUETOOTH_BROADCAST_MESSAGE")
	if msg == "" {
		msg = broadcastMessage
	}

	serverMsg := os.Getenv("REMOTE_BLUETOOTH_BROADCAST_SERVER_RESPONSE")
	if serverMsg == "" {
		serverMsg = broadcastServerResponse
	}

	adapterID := os.Getenv("REMOTE_BLUETOOTH_ADAPTER_ID")

	return Config{
		Port:                 port,
		AuthenticationSecret: secret,
		AdapterID:            adapterID,

		BroadcastPort:           broadcastPort,
		BroadcastMessage:        []byte(msg),
		BroadcastServerResponse: []byte(serverMsg),
	}
}
