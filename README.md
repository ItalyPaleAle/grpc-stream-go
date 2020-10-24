# gRPC stream test

This is a sample of a gRPC client and server that communicate via a bi-directional gRPC stream, written in Go.

Features graceful shutdowns, ability to restart the server, and more.

To test it:

- Start the server with: `cd server && go run .`
- Start the client with: `cd client && go run .`
