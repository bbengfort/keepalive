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
	"github.com/bbengfort/x/stats"
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
	srv := &keepAlive{
		conf:    conf,
		stop:    make(chan bool, 2),
		done:    make(chan bool, 2),
		latency: &stats.Statistics{},
		outage:  &stats.Statistics{},
	}
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

	for {
		select {
		case err = <-errc:
			// Exit condition is an error on this channel
			return err
		case <-srv.ctx.Done():
			// Track the start of an outage
			if srv.outageStart.IsZero() {
				srv.outageStart = time.Now()
			}

			// The main routine must maintain the stream connection, not the inner routines
			// The context will remain Done until the stream is reopened, so multiple attempts
			// delays how long we wait before we retry the connection again.
			log.Warn().Err(srv.ctx.Err()).Msg("context canceled, attempting to reopen stream")
			if err = srv.open(); err != nil {
				srv.attempts++
				log.Warn().Err(err).Int64("attempts", srv.attempts).Msg("could not reconnect")
			} else {
				// Record the length of the outage if we're reconnected
				outageDuration := time.Since(srv.outageStart)
				srv.outageStart = time.Time{}
				srv.outage.Update(float64(outageDuration) / float64(time.Millisecond))
				log.Info().
					Float64("mean", srv.outage.Mean()).
					Float64("stddev", srv.outage.StdDev()).
					Float64("min", srv.outage.Minimum()).
					Float64("max", srv.outage.Maximum()).
					Uint64("count", srv.outage.N()).
					Msg("outage statistics")
			}
		}
	}
}

type keepAlive struct {
	sync.RWMutex
	conf        config.Config
	cc          *grpc.ClientConn
	ctx         context.Context
	client      pb.KeepAliveClient
	stream      pb.KeepAlive_EchoClient
	latency     *stats.Statistics
	outage      *stats.Statistics
	outageStart time.Time
	attempts    int64
	stop        chan bool
	done        chan bool
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

	// Delay longer the more attempts we've tried
	time.Sleep(time.Duration(k.attempts) * time.Second)

	// Open the stream with a background context
	if k.stream, err = k.client.Echo(context.Background()); err != nil {
		return err
	}
	log.Trace().Msg("echo stream opened")
	k.ctx = k.stream.Context()

	// Send initialization message
	if err = k.stream.Send(&pb.Packet{Timestamp: time.Now().UnixNano(), Originator: k.conf.Originator}); err != nil {
		log.Error().Err(err).Msg("could not send initialization message")
		return err
	}

	// Success! Log and reset attempts
	log.Info().Msg("echo stream initialized")
	k.attempts = 0
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
	ticker := time.NewTicker(k.conf.Interval)
	defer ticker.Stop()

	// Primary outer loop
	for {
		select {
		case <-k.stop:
			log.Debug().Str("receiver", "sendr").Msg("stopping")
			k.done <- true
			return
		case <-ticker.C:
			log.Trace().Str("receiver", "sendr").Msg("tick")
		}

		// Update the packet
		packet.Msgid++
		packet.Timestamp = time.Now().UnixNano()

		// Send a new message to the server
		if err := k.stream.Send(packet); err != nil {
			// Log the error
			log.Warn().Err(err).Int64("msg", packet.Msgid).Msg("could not send message")

			// Reset the msgid so we send it again
			packet.Msgid--
			continue
		}
		log.Debug().Int64("msg", packet.Msgid).Msg("message sent")
	}
}

func (k *keepAlive) recvr(errc chan<- error) {
	msgs := make(chan *pb.Packet, 10)
	closed := uint32(0)

	// Secondary recv loop
	go func(msgs chan<- *pb.Packet) {
		for {
			msg, err := k.stream.Recv()
			if atomic.LoadUint32(&closed) > 0 {
				return
			}
			// If the server shuts down, the context may get the error first; log the error and continue
			if err != nil {
				// Log the error and wait for a bit for the stream to be reopened.
				log.Error().Err(err).Msg("could not recv message")
				time.Sleep(k.conf.Interval)
				continue
			}

			msgs <- msg
		}

	}(msgs)

	// Primary outer loop
	for {
		select {
		case <-k.stop:
			log.Debug().Str("receiver", "recvr").Msg("stopping")
			atomic.AddUint32(&closed, 1)
			k.done <- true
			return
		case msg := <-msgs:
			latency := time.Since(time.Unix(0, msg.Timestamp))
			k.latency.Update(float64(latency) / float64(time.Millisecond))
			log.Debug().Int64("msgid", msg.Msgid).Dur("latency", latency).Msg("ping returned")

			// Log the statistics every 100 messages
			if msg.Msgid%100 == 0 {
				log.Info().
					Float64("mean", k.latency.Mean()).
					Float64("stddev", k.latency.StdDev()).
					Float64("min", k.latency.Minimum()).
					Float64("max", k.latency.Maximum()).
					Uint64("count", k.latency.N()).
					Msg("latency statistics")
			}
		}
	}
}
