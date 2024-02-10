package bluetooth

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/bluez/profile/adapter"
	"github.com/muka/go-bluetooth/bluez/profile/device"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	btgrpc "github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
	"github.com/andree-bjorkgard/remote-bluetooth/internal/config"
)

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

func (s *BluetoothServer) ConnectToDevice(ctx context.Context, device *btgrpc.Device) error {
	dev, err := s.adapter.GetDeviceByAddress(device.Address)
	if err != nil {
		return err
	}
	err = dev.Connect()
	if err != nil {
		return err
	}
	return nil
}

func (s *BluetoothServer) DisconnectFromDevice(ctx context.Context, device *btgrpc.Device) error {
	dev, err := s.adapter.GetDeviceByAddress(device.Address)
	if err != nil {
		return err
	}
	err = dev.Disconnect()
	if err != nil {
		return err
	}
	return nil
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
		BatteryStatus: "",
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
