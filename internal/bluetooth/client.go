package bluetooth

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	btgrpc "github.com/andree-bjorkgard/remote-bluetooth/internal/bluetooth/grpc"
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

func (c *BluetoothClient) ConnectDevice(mac string) error {
	_, err := c.client.ConnectDevice(context.Background(), &btgrpc.ConnectRequest{Address: mac})
	if err != nil {
		return err
	}

	return nil
}

func (c *BluetoothClient) DisconnectDevice(mac string) error {
	_, err := c.client.DisconnectDevice(context.Background(), &btgrpc.DisconnectRequest{Address: mac})
	if err != nil {
		return err
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
