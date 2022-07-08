package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	gops "github.com/google/gops/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	pb "streamtest/service"
)

func main() {
	// Gops (for detecting goroutine leaks)
	if err := gops.Listen(gops.Options{}); err != nil {
		log.Fatal(err)
	}

	// Init the client
	client := &RPCClient{}
	client.Init()
	err := client.Connect()
	if err != nil {
		log.Fatal(err)
	}

	// Continue indefinitely
	//select {}

	// Listen to the USR1 signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh,
		syscall.SIGUSR1,
	)

	for {
		// Wait for the signal to disconnect
		<-sigCh
		err = client.Disconnect()
		if err != nil {
			panic(err)
		}

		// On the next USR1 signal, re-connect
		<-sigCh
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}
}

// Auth token for RPC calls
const authToken = "hello world"

// Timeout for all requests, in seconds
const requestTimeout = 15

// Interval between keepalive requests, in seconds
const keepaliveInterval = 600

// RPCAuth is the object implementing credentials.PerRPCCredentials that provides the auth info
type RPCAuth struct {
	PSK string
}

// GetRequestMetadata returns the metadata containing the authorization key
func (a *RPCAuth) GetRequestMetadata(ctx context.Context, in ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + a.PSK,
	}, nil
}

// RequireTransportSecurity returns true because this kind of auth requires TLS
func (a *RPCAuth) RequireTransportSecurity() bool {
	return true
}

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
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			// Enable InsecureSkipVerify because our certificate is self-signed
			// TODO: Remove this in production!
			InsecureSkipVerify: true,
		})),
		grpc.WithPerRPCCredentials(&RPCAuth{
			PSK: authToken,
		}),
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
	go func() {
		// Continue re-connecting automatically if the connection drops
		for c.connection != nil {
			c.logger.Println("Connecting to the channel")
			// Note that if the underlying connection is down, this call blocks until it comes back up
			c.startStream()
			// Wait 1 second before trying to reconnect
			time.Sleep(1 * time.Second)
		}
	}()

	return nil
}

// Disconnect closes the connection with the gRPC server
func (c *RPCClient) Disconnect() error {
	conn := c.connection
	c.connection = nil
	err := conn.Close()
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to the stream RPC
	stream, err := c.client.Channel(ctx, grpc.WaitForReady(true))
	if err != nil {
		c.logger.Println("Error while connecting to the Channel stream:", err)
		return
	}
	defer stream.CloseSend()
	c.logger.Println("Channel connected")

	// Send new pings every 3 seconds
	timer := time.NewTicker(3 * time.Second)
	defer timer.Stop()

	// Watch for incoming messages in a background goroutine
	go func() {
		for {
			// This call is blocking
			in, err := stream.Recv()
			if err == io.EOF {
				c.logger.Println("Stream reached EOF")
				cancel()
				break
			}
			if err != nil {
				c.logger.Println("Error while reading message:", err)
				break
			}

			c.logger.Println("Received Ping message:", in.Ping)
		}
	}()

	// Send pings at the interval
	for {
		select {
		// Interval
		case <-timer.C:
			err := stream.Send(&pb.ChannelClientStream{
				Pong: true,
			})
			if err == io.EOF {
				c.logger.Println("Stream reached EOF")
				return
			}
			if err != nil {
				c.logger.Println("Error while sending message:", err)
			}
		// Context for canceling the operation
		case <-ctx.Done():
			c.logger.Println("Channel closed")
			return
		}
	}
}
