package keepalive

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/bbengfort/keepalive/pkg/config"
	"github.com/bbengfort/keepalive/pkg/logger"
	"github.com/bbengfort/keepalive/pkg/pb"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
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
	return nil
}
