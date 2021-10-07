package keepalive

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bbengfort/keepalive/pkg/config"
	"github.com/bbengfort/keepalive/pkg/pb"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func KeepAlive(conf config.Config) (err error) {
	if conf.IsZero() {
		if conf, err = config.New(); err != nil {
			return err
		}
	}

	// Set the global level
	zerolog.SetGlobalLevel(conf.GetLogLevel())

	// Set human readable logging if specified
	if conf.ConsoleLog {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	// Create and connect the keepAlive client
	srv := &keepAlive{conf: conf, stop: make(chan bool, 2), done: make(chan bool, 2)}
	if err = srv.connect(); err != nil {
		return err
	}

	// Open the stream
	if err = srv.open(); err != nil {
		return err
	}

	errc := make(chan error, 1)
	quit := make(chan os.Signal, 1)

	signal.Notify(quit, os.Interrupt)
	go func(quit <-chan os.Signal) {
		<-quit
		errc <- srv.shutdown()
	}(quit)

	go srv.sendr(errc)
	go srv.recvr(errc)

	return <-errc
}

type keepAlive struct {
	sync.RWMutex
	conf   config.Config
	cc     *grpc.ClientConn
	client pb.KeepAliveClient
	stream pb.KeepAlive_EchoClient
	stop   chan bool
	done   chan bool
}

func (k *keepAlive) connect() (err error) {
	var opts []grpc.DialOption
	if k.conf.NoSecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		config := &tls.Config{}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	}

	if k.cc, err = grpc.Dial(k.conf.Endpoint, opts...); err != nil {
		return err
	}
	k.client = pb.NewKeepAliveClient(k.cc)
	log.Debug().Str("endpoint", k.conf.Endpoint).Msg("connected to keepalive server")
	return nil
}

func (k *keepAlive) open() (err error) {
	k.Lock()
	defer k.Unlock()

	// Open the stream with a background context
	if k.stream, err = k.client.Echo(context.Background()); err != nil {
		return err
	}
	log.Trace().Msg("echo stream opened")

	// Send initialization message
	if err = k.stream.Send(&pb.Packet{Timestamp: time.Now().UnixNano(), Originator: k.conf.Originator}); err != nil {
		log.Error().Err(err).Msg("could not send initialization message")
		return err
	}
	log.Info().Msg("echo stream initialized")
	return nil
}

func (k *keepAlive) shutdown() (err error) {
	log.Info().Msg("gracefully shutting down")

	// TODO: add a shutdown timeout
	// send two stop signals to each of the go routines
	for i := 0; i < 2; i++ {
		k.stop <- true
		log.Trace().Msg("stop signal sent to both routines")
	}

	// wait for two done signals from each of the go routines
	for i := 0; i < 2; i++ {
		<-k.done
		log.Trace().Msg("a routine signaled done")
	}

	errs := make([]error, 0)

	// at this point the two go routines are done
	k.Lock()
	defer k.Unlock()
	if err = k.stream.CloseSend(); err != nil {
		log.Error().Err(err).Msg("could not close send stream")
		errs = append(errs, err)
	}

	if err = k.cc.Close(); err != nil {
		log.Error().Err(err).Msg("could not close connection")
		errs = append(errs, err)
	}

	close(k.stop)
	close(k.done)
	k.stream = nil
	k.client = nil
	k.cc = nil

	switch {
	case len(errs) > 1:
		return fmt.Errorf("%d errors occurred during shutdown", len(errs))
	case len(errs) == 1:
		return errs[0]
	default:
		return nil
	}
}

func (k *keepAlive) sendr(errc chan<- error) {
	// Create the template packet
	packet := &pb.Packet{Originator: k.conf.Originator}

	// Establish the ticker context
	ctx := k.stream.Context()
	ticker := time.NewTicker(k.conf.Interval)
	defer ticker.Stop()

	// Primary outer loop
	for {
		select {
		case <-k.stop:
			log.Trace().Str("receiver", "sendr").Msg("stopping")
			k.done <- true
			return
		case <-ctx.Done():
			log.Trace().Str("receiver", "sendr").Msg("context done")
			k.done <- true
			errc <- ctx.Err()
			return
		case <-ticker.C:
			log.Trace().Str("receiver", "sendr").Msg("tick")
		}

		// Update the packet
		packet.Msgid++
		packet.Timestamp = time.Now().UnixNano()

		// Send a new message to the server
		if err := k.stream.Send(packet); err != nil {
			// Log the error and attempt to reopen the stream
			log.Error().Err(err).Int64("msg", packet.Msgid).Msg("could not send message")
			go func() {
				if err := k.open(); err != nil {
					log.Error().Err(err).Str("caller", "sendr").Msg("could not reopen stream")
				}
			}()

			// Reset the msgid so we send it again
			packet.Msgid--
			continue
		}
		log.Debug().Int64("msg", packet.Msgid).Msg("message sent")
	}
}

func (k *keepAlive) recvr(errc chan<- error) {
	ctx := k.stream.Context()
	msgs := make(chan *pb.Packet, 10)
	closed := uint32(0)

	// Secondary recv loop
	go func(msgs chan<- *pb.Packet) {
		for {
			msg, err := k.stream.Recv()
			if atomic.LoadUint32(&closed) > 0 {
				return
			}

			if err != nil {
				// Log the error and attempt to reopen the stream
				log.Error().Err(err).Msg("could not recv message")
				go func() {
					if err := k.open(); err != nil {
						log.Error().Err(err).Str("caller", "recvr").Msg("could not reopen stream")
					}
				}()
				continue
			}

			msgs <- msg
		}

	}(msgs)

	// Primary outer loop
	for {
		select {
		case <-k.stop:
			log.Trace().Str("receiver", "recvr").Msg("stopping")
			atomic.AddUint32(&closed, 1)
			k.done <- true
			return
		case <-ctx.Done():
			log.Trace().Str("receiver", "recvr").Msg("context done")
			atomic.AddUint32(&closed, 1)
			k.done <- true
			errc <- ctx.Err()
			return
		case msg := <-msgs:
			latency := time.Since(time.Unix(0, msg.Timestamp))
			log.Info().Int64("msgid", msg.Msgid).Dur("latency", latency).Msg("ping returned")
		}
	}
}
