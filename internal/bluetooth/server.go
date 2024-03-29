package bluetooth

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/bluez/profile/adapter"
	"github.com/muka/go-bluetooth/bluez/profile/battery"
	"github.com/muka/go-bluetooth/bluez/profile/device"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	btgrpc "github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
	"github.com/andree-bjorkgard/remote-bluetooth/pkg/config"
)

const BATTERY_UUID = "0000180f-0000-1000-8000-00805f9b34fb"

type BluetoothServer struct {
	btgrpc.UnimplementedBluetoothServer

	port    int
	adapter *adapter.Adapter1
}

var _ btgrpc.BluetoothServer = (*BluetoothServer)(nil)

func NewBluetoothServer(port int, adapterID string) *BluetoothServer {
	var adapter *adapter.Adapter1
	var err error

	if adapterID == "" {
		adapter, err = api.GetDefaultAdapter()

	} else {
		adapter, err = api.GetAdapter(adapterID)
	}

	if err != nil {
		panic(err)
	}

	return &BluetoothServer{port: port, adapter: adapter}
}

func (s *BluetoothServer) Start() error {
	var opts []grpc.ServerOption = []grpc.ServerOption{
		grpc.UnaryInterceptor(unaryServerInterceptor(config.NewConfig())),
	}
	grpcServer := grpc.NewServer(opts...)
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", s.port))

	if err != nil {
		return fmt.Errorf("Server.Start: %w", err)
	}

	log.Printf("Server.Start: Starting server on: %s", listener.Addr())
	btgrpc.RegisterBluetoothServer(grpcServer, s)

	return grpcServer.Serve(listener)
}

func (s *BluetoothServer) GetTrustedDevices(ctx context.Context, _ *btgrpc.Empty) (*btgrpc.Devices, error) {
	var devs *btgrpc.Devices

	rawDevs, err := s.adapter.GetDevices()
	if err != nil {
		return devs, err
	}
	devs = &btgrpc.Devices{}
	for _, rd := range rawDevs {
		if rd != nil && rd.Properties.Trusted {
			dev := deviceToGrpcDevice(rd)
			devs.Devices = append(devs.Devices, dev)
		}
	}
	return devs, nil
}

func (s *BluetoothServer) ConnectToDevice(ctx context.Context, request *btgrpc.ConnectRequest) (*btgrpc.Response, error) {
	resp := &btgrpc.Response{Success: false}
	dev, err := s.adapter.GetDeviceByAddress(request.Address)
	if err != nil {
		return resp, err
	}
	err = dev.Connect()
	if err != nil {
		return resp, err
	}

	resp.Success = true
	return resp, nil
}

func (s *BluetoothServer) DisconnectFromDevice(ctx context.Context, request *btgrpc.DisconnectRequest) (*btgrpc.Response, error) {
	resp := &btgrpc.Response{Success: false}
	dev, err := s.adapter.GetDeviceByAddress(request.Address)
	if err != nil {
		return resp, err
	}
	err = dev.Disconnect()
	if err != nil {
		return resp, err
	}

	resp.Success = true
	return resp, err
}

func getBatteryStatus(dev *device.Device1) string {
	if dev.Properties.Connected {
		for _, uuid := range dev.Properties.UUIDs {
			if uuid == BATTERY_UUID {
				b, err := battery.NewBattery1(dev.Path())
				if err != nil {
					log.Println("Error getting battery service:", err)
					return ""
				}
				p, err := b.GetPercentage()
				if err != nil {
					log.Println("Error getting battery percentage:", err)
					return ""
				}

				return fmt.Sprintf("%d", p)
			}
		}
	}

	return ""
}

func deviceToGrpcDevice(d *device.Device1) *btgrpc.Device {
	addr, _ := d.GetAddress()
	name, _ := d.GetName()
	trusted, _ := d.GetTrusted()
	paired, _ := d.GetPaired()
	connected, _ := d.GetConnected()
	icon, _ := d.GetIcon()

	return &btgrpc.Device{
		Address:       addr,
		Name:          name,
		Trusted:       trusted,
		Paired:        paired,
		Connected:     connected,
		Icon:          icon,
		BatteryStatus: getBatteryStatus(d),
	}
}

func unaryServerInterceptor(cfg config.Config) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		}
		secret := md.Get("Authorization")
		if len(secret) != 1 || secret[0] != cfg.AuthenticationSecret {
			return nil, fmt.Errorf("UnaryServerInterceptor: invalid secret")
		}

		return handler(ctx, req)
	}
}
