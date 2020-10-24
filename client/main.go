package main

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"

	pb "streamtest/service"
)

func main() {
	// Start the client
	client := &RPCClient{}
	client.Init()
	err := client.Connect()
	if err != nil {
		panic(err)
	}

	// Continue indefinitely
	select {}
}

// Timeout for all requests, in seconds
const requestTimeout = 15

// Interval between keepalive requests, in seconds
const keepaliveInterval = 600

// RPCClient is the gRPC client for communicating with the cluster manager
type RPCClient struct {
	client     pb.ControllerClient
	connection *grpc.ClientConn
	logger     *log.Logger
}

// Init the gRPC client
func (c *RPCClient) Init() {
	// Initialize the logger
	c.logger = log.New(os.Stdout, "grpc: ", log.Ldate|log.Ltime|log.LUTC)
}

// Connect starts the connection to the gRPC server and starts all background streams
func (c *RPCClient) Connect() (err error) {
	// Underlying connection
	connOpts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithInsecure(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.DefaultConfig,
			MinConnectTimeout: time.Duration(requestTimeout) * time.Second,
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    time.Duration(keepaliveInterval) * time.Second,
			Timeout: time.Duration(requestTimeout) * time.Second,
		}),
	}
	c.connection, err = grpc.Dial("localhost:2400", connOpts...)
	if err != nil {
		return err
	}

	// Client
	c.client = pb.NewControllerClient(c.connection)

	// Start the background stream in another goroutine
	go c.startStream()

	return nil
}

// Disconnect closes the connection with the gRPC server
func (c *RPCClient) Disconnect() error {
	err := c.connection.Close()
	c.connection = nil
	return err
}

// Reconnect re-connects to the gRPC server
func (c *RPCClient) Reconnect() error {
	if c.connection != nil {
		// Ignore errors here
		_ = c.Disconnect()
	}
	return c.Connect()
}

// startStream starts the stream with the server
func (c *RPCClient) startStream() {
	// Connect to the stream RPC
	stream, err := c.client.Channel(context.Background(), grpc.WaitForReady(true))
	if err != nil {
		c.logger.Println("Error while connecting to the Channel stream:", err)
		return
	}
	defer stream.CloseSend()
	c.logger.Println("Channel connected")

	// Watch for incoming messages in a background goroutine
	go func() {
		for {
			// This call is blocking
			in, err := stream.Recv()
			if err == io.EOF {
				c.logger.Println("Stream reached EOF")
				break
			}
			if err != nil {
				c.logger.Println("Error while reading message:", err)
				break
			}

			c.logger.Println("Received Ping message:", in.Ping)
		}
	}()

	// Send pongs every 3 seconds
	timer := time.NewTicker(3 * time.Second)
	for range timer.C {
		stream.Send(&pb.ChannelClientStream{
			Pong: true,
		})
	}
}
