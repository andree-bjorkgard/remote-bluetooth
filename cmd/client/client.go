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
		if event.Device.Address == "00:0A:45:19:F3:A6" {
			if event.Device.Connected {
				err := c.DisconnectFromDevice(event.Server, event.Device.Address)
				if err != nil {
					log.Printf("Failed to disconnect device: %s\n", err)
				}
			} else {
				err := c.ConnectToDevice(event.Server, event.Device.Address)
				if err != nil {
					log.Printf("Failed to connect device: %s\n", err)
				}
			}
		}
	}
}
