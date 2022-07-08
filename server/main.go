package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	pb "streamtest/service"
)

func main() {
	// Start the server
	srv := &RPCServer{}
	srv.Init()
	go srv.Start()

	// Handle graceful shutdown on SIGINT, SIGTERM and SIGQUIT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)

	// Wait for the shutdown signal then stop the servers
	<-sigCh
	fmt.Println("Received signal to terminate the app")
	srv.Stop()
}

// Auth token for RPC calls
const authToken = "hello world"

// RPCServer manages the gRPC server
type RPCServer struct {
	pb.UnimplementedControllerServer

	logger        *log.Logger
	stopCh        chan int
	restartCh     chan int
	doneCh        chan int
	runningCtx    context.Context
	runningCancel context.CancelFunc
	running       bool
	grpcServer    *grpc.Server
}

// Init the gRPC server
func (s *RPCServer) Init() {
	s.running = false

	// Initialize the logger
	s.logger = log.New(os.Stdout, "grpc: ", log.Ldate|log.Ltime|log.LUTC)

	// Channels used to stop and restart the server
	s.stopCh = make(chan int)
	s.restartCh = make(chan int)
	s.doneCh = make(chan int)
}

// Start the gRPC server
func (s *RPCServer) Start() {
	for {
		// Create the context
		s.runningCtx, s.runningCancel = context.WithCancel(context.Background())

		// TLS
		creds, err := credentials.NewServerTLSFromFile("../cert.pem", "../key.pem")
		if err != nil {
			panic(err)
		}

		// Create the server
		s.grpcServer = grpc.NewServer(
			grpc.Creds(creds),
			grpc.UnaryInterceptor(authUnaryInterceptor),
			grpc.StreamInterceptor(authStreamInterceptor),
		)
		pb.RegisterControllerServer(s.grpcServer, s)

		// Start the server in another channel
		go func() {
			// Listen
			port := 2400
			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				s.runningCancel()
				panic(err)
			}
			s.logger.Printf("Starting gRPC server on port %d\n", port)
			s.running = true
			s.grpcServer.Serve(listener)
		}()

		select {
		case <-s.stopCh:
			// We received an interrupt signal, shut down for good
			s.logger.Println("Shutting down the gRCP server")
			s.gracefulStop()
			s.running = false
			s.doneCh <- 1
			return
		case <-s.restartCh:
			// We received a signal to restart the server
			s.logger.Println("Restarting the gRCP server")
			s.gracefulStop()
			s.doneCh <- 1
			// Do not return, let the for loop repeat
		}
	}
}

// Restart the server
func (s *RPCServer) Restart() {
	if s.running {
		s.restartCh <- 1
		<-s.doneCh
	}
}

// Stop the server
func (s *RPCServer) Stop() {
	if s.running {
		s.stopCh <- 1
		<-s.doneCh
	}
}

// Internal function that gracefully stops the gRPC server, with a timeout
func (s *RPCServer) gracefulStop() {
	const shutdownTimeout = 15
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(shutdownTimeout)*time.Second)
	defer cancel()

	// Cancel the context
	s.runningCancel()

	// Try gracefulling closing the gRPC server
	closed := make(chan int)
	go func() {
		s.grpcServer.GracefulStop()
		if closed != nil {
			// Use a select just in case the channel was closed
			select {
			case closed <- 1:
			default:
			}
		}
	}()

	select {
	// Closed - all good
	case <-closed:
		close(closed)
	// Timeout
	case <-ctx.Done():
		// Force close
		s.logger.Printf("Shutdown timeout of %d seconds reached - force shutdown\n", shutdownTimeout)
		s.grpcServer.Stop()
		close(closed)
		closed = nil
	}
	s.logger.Println("gRPC server shut down")
}

// Channel is the handler for the Channel gRPC
func (s *RPCServer) Channel(stream pb.Controller_ChannelServer) error {
	s.logger.Println("Client connected")

	// Send a ping message every 2 seconds
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()

	// Goroutine that takes care of receiving messages
	go func() {
		// Receive messages in background
		for {
			// This call is blocking
			in, err := stream.Recv()
			if err == io.EOF {
				s.logger.Println("Stream reached EOF")
				break
			} else if err != nil {
				s.logger.Println("Error while reading message:", err)
				break
			}

			s.logger.Println("Received Pong message:", in.Pong)
		}
	}()

	// Send messages when needed
	for {
		select {
		// Exit if context is done
		case <-stream.Context().Done():
			fmt.Println("stream.Context().Done()")
			return nil

		// The server is shutting down
		case <-s.runningCtx.Done():
			fmt.Println("runningCtx.Done()")
			return nil

		// Send a ping
		case <-timer.C:
			stream.Send(&pb.ChannelServerStream{
				Ping: true,
			})
		}
	}
}

// Interceptor for unary ("simple RPC") requests that checks the authorization metadata
func authUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	// Check if the call is authorized
	err = checkAuth(ctx)
	if err != nil {
		return
	}

	// Call is authorized, so continue the execution
	return handler(ctx, req)
}

// Interceptor for stream requests that checks the authorization metadata
func authStreamInterceptor(srv interface{}, srvStream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	// Check if the call is authorized
	err = checkAuth(srvStream.Context())
	if err != nil {
		return
	}

	// Call is authorized, so continue the execution
	return handler(srv, srvStream)
}

// Used by the interceptors, this checks the authorization metadata
func checkAuth(ctx context.Context) error {
	// Ensure we have an authorization metadata
	// Note that the keys in the metadata object are always lowercased
	m, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return errors.New("missing metadata")
	}
	if len(m["authorization"]) != 1 {
		return errors.New("invalid authorization")
	}

	// Remove the optional "Bearer " prefix
	if strings.TrimPrefix(m["authorization"][0], "Bearer ") != "hello world" {
		return errors.New("invalid authorization")
	}

	// All good
	return nil
}
