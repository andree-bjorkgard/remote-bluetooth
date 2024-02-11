package bluetooth

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	btgrpc "github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
)

var (
	ErrConnectingFailed    = errors.New("connecting to device failed")
	ErrDisconnectingFailed = errors.New("disconnecting to device failed")
)

type BluetoothClient struct {
	client btgrpc.BluetoothClient
	conn   *grpc.ClientConn

	authorization string
}

func NewBluetoothClient(addr string, authorization string) (*BluetoothClient, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryClientInterceptor(authorization)),
	}
	conn, err := grpc.Dial(addr, opts...)
	if err != nil {
		return nil, err
	}
	client := btgrpc.NewBluetoothClient(conn)

	return &BluetoothClient{client: client, conn: conn, authorization: authorization}, nil
}

func (c *BluetoothClient) Close() error {
	return c.conn.Close()
}

func (c *BluetoothClient) GetTrustedDevices() ([]*btgrpc.Device, error) {
	devs, err := c.client.GetTrustedDevices(context.Background(), &btgrpc.Empty{})
	if err != nil {
		return nil, err
	}

	return devs.Devices, nil
}

func (c *BluetoothClient) ConnectToDevice(mac string) error {
	r, err := c.client.ConnectToDevice(context.Background(), &btgrpc.ConnectRequest{Address: mac})
	if err != nil {
		return err
	}

	if !r.Success {
		return ErrConnectingFailed
	}

	return nil
}

func (c *BluetoothClient) DisconnectFromDevice(mac string) error {
	r, err := c.client.DisconnectFromDevice(context.Background(), &btgrpc.DisconnectRequest{Address: mac})
	if err != nil {
		return err
	}

	if !r.Success {
		return ErrDisconnectingFailed
	}

	return nil
}

func unaryClientInterceptor(secret string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		}
		md.Append("Authorization", secret)
		ctx = metadata.NewOutgoingContext(ctx, md)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
