package keepalive

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"github.com/bbengfort/keepalive/pkg/config"
	"github.com/bbengfort/keepalive/pkg/logger"
	"github.com/bbengfort/keepalive/pkg/pb"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() {
	// Initialize zerolog with GCP logging requirements
	zerolog.TimeFieldFormat = time.RFC3339
	zerolog.TimestampFieldName = logger.GCPFieldKeyTime
	zerolog.MessageFieldName = logger.GCPFieldKeyMsg

	// Add the severity hook for GCP logging
	var gcpHook logger.SeverityHook
	log.Logger = zerolog.New(os.Stdout).Hook(gcpHook).With().Timestamp().Logger()
}

func New(conf config.Config) (s *Server, err error) {
	if conf.IsZero() {
		if conf, err = config.New(); err != nil {
			return nil, err
		}
	}

	// Set the global level
	zerolog.SetGlobalLevel(conf.GetLogLevel())

	// Set human readable logging if specified
	if conf.ConsoleLog {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	// Create the server
	s = &Server{conf: conf, echan: make(chan error, 1)}

	// Initialize the gRPC server
	s.srv = grpc.NewServer()
	pb.RegisterKeepAliveServer(s.srv, s)
	return s, nil
}

type Server struct {
	pb.UnimplementedKeepAliveServer
	srv   *grpc.Server
	conf  config.Config
	echan chan error
}

func (s *Server) Serve() (err error) {
	// Catch OS signals for graceful shutdowns
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	go func() {
		<-quit
		s.echan <- s.Shutdown()
	}()

	// Listen for TCP requests on the bind addr
	var sock net.Listener
	if sock, err = net.Listen("tcp", s.conf.BindAddr); err != nil {
		return fmt.Errorf("could not listen on %q", s.conf.BindAddr)
	}

	// Run the server
	go func() {
		defer sock.Close()
		log.Info().Str("listen", s.conf.BindAddr).Str("version", Version()).Msg("keepalive server started")
		if err := s.srv.Serve(sock); err != nil {
			s.echan <- err
		}
	}()

	if err = <-s.echan; err != nil {
		return err
	}
	return nil
}

func (s *Server) Shutdown() (err error) {
	log.Info().Msg("gracefully shutting down")
	s.srv.GracefulStop()
	return nil
}

func (s *Server) Echo(stream pb.KeepAlive_EchoServer) (err error) {
	// Get the context of the connection
	log.Debug().Msg("preparing incoming echo stream")
	ctx := stream.Context()
	done := make(chan error, 2)
	initMsg := make(chan *pb.Packet)
	closed := uint32(0)

	// Special handler for the first message in the stream
	go func() {
		msg, err := stream.Recv()
		switch {
		case err == io.EOF:
			log.Warn().Msg("stream closed before first message")
			done <- nil
		case err != nil:
			log.Error().Err(err).Msg("could not recv first message")
			done <- err
		default:
			initMsg <- msg
		}
	}()

	// Wait for first message special handling
	var msg *pb.Packet
	select {
	case <-ctx.Done():
		log.Warn().Msg("stream closed before first message")
		if err = ctx.Err(); err != nil {
			return status.Error(codes.Aborted, err.Error())
		}
		return nil
	case err = <-done:
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		return nil
	case msg = <-initMsg:
		close(initMsg)
		break
	}

	// Handle first message specially
	originator := msg.Originator
	if originator == "" || originator == "unknown" {
		return status.Error(codes.InvalidArgument, "must specify originator in first message")
	}

	// Create a sublogger to report all future events
	events := log.With().Str("originator", originator).Str("stream", uuid.NewString()).Logger()
	events.Info().Msg("echo stream connection established")

	// Continue to listen for all remaining messages
	messages := make(chan *pb.Packet, 10)
	go func(messages chan<- *pb.Packet) {
		for {
			// Receive a message from the client, blocking until message receipt
			in, err := stream.Recv()

			// Check to ensure the channels haven't been closed by the outer routine
			if atomic.LoadUint32(&closed) > 0 {
				return
			}

			// Error handling
			if err != nil {
				if err == io.EOF {
					events.Info().Msg("echo stream closed by client")
					done <- nil
				} else {
					events.Error().Err(err).Msg("could not recv packet")
					done <- err
				}
				return
			}

			// Send message to the outer routine
			messages <- in
		}
	}(messages)

	// Handle all incoming messages and send responses
	go func(messages <-chan *pb.Packet) {
		for msg := range messages {
			// Log the receipt of the message
			events.Info().Int64("message", msg.Msgid).Msg("packet received")

			// Attempt to echo the packet back to the client
			if err := stream.Send(msg); err != nil {
				// Check to ensure the channels haven't been closed by the outer routine
				if atomic.LoadUint32(&closed) > 0 {
					return
				}

				// Error handling
				if err == io.EOF {
					events.Info().Msg("echo stream closed by client")
					done <- nil
				} else {
					events.Error().Err(err).Msg("could not send packet")
					done <- err
				}
				return
			}
		}
	}(messages)

	// Outer routine waits for an error or done, then cleans up and exits
	select {
	case <-ctx.Done():
		if err = ctx.Err(); err != nil {
			events.Error().Err(err).Msg("context error")
		}
		break
	case err = <-done:
		break
	}

	// Clean up and close channels
	atomic.AddUint32(&closed, 1)
	close(messages)
	close(done)

	// Error handling
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
