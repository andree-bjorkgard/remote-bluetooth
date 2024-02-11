package main

import (
	"log"

	"github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/discovery"
	"github.com/andree-bjorkgard/remote-bluetooth/pkg/config"
	"github.com/sirupsen/logrus"
)

func main() {
	cfg := config.NewConfig()
	logrus.SetLevel(logrus.ErrorLevel)

	discoveryService := discovery.NewDiscoveryService(cfg.BroadcastPort, cfg.BroadcastMessage, cfg.BroadcastServerResponse)

	go discoveryService.StartServerAnnouncer(cfg.Port)

	if err := bluetooth.NewBluetoothServer(cfg.Port, cfg.AdapterID).Start(); err != nil {
		log.Println(err)
	}
}
