package main

import (
	"log"

	"github.com/andree-bjorkgard/remote-bluetooth/pkg/client"
	"github.com/andree-bjorkgard/remote-bluetooth/pkg/config"
)

func main() {
	cfg := config.NewConfig()
	c := client.NewClient(cfg)

	go c.FindServers()
	for {
		event := <-c.GetDeviceEventsChannel()
		log.Printf("Server: %s, Device: %s, Battery: %s\n", event.Server, event.Device.Name, event.Device.BatteryStatus)
	}
}
