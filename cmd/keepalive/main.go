package main

import (
	"os"

	keepalive "github.com/bbengfort/keepalive/pkg"
	"github.com/bbengfort/keepalive/pkg/config"
	"github.com/joho/godotenv"
	cli "github.com/urfave/cli/v2"
)

func main() {
	// Load the dotenv file if it exists
	godotenv.Load()

	app := cli.NewApp()
	app.Name = "keepalive"
	app.Version = keepalive.Version()
	app.Usage = "an experiment to see what it takes to keep a gRPC stream alive indefinitely"
	app.Flags = []cli.Flag{}
	app.Commands = []*cli.Command{
		{
			Name:     "serve",
			Usage:    "run the keepalive server",
			Category: "server",
			Action:   serve,
			Flags:    []cli.Flag{},
		},
		{
			Name:     "keep",
			Usage:    "attempt to keep a client connection open for as long as possible",
			Category: "client",
			Action:   keep,
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:    "endpoint",
					Aliases: []string{"e"},
					Usage:   "the url to connect the directory service client",
					Value:   "localhost:8318",
					EnvVars: []string{"KEEPALIVE_GRPC_ENDPOINT"},
				},
				&cli.BoolFlag{
					Name:    "no-secure",
					Aliases: []string{"S"},
					Usage:   "do not connect via TLS (e.g. for development)",
					EnvVars: []string{"KEEPALIVE_NO_SECURE"},
				},
			},
		},
	}

	app.Run(os.Args)
}

func serve(c *cli.Context) (err error) {
	var conf config.Config
	if conf, err = config.New(); err != nil {
		return cli.Exit(err, 1)
	}

	var srv *keepalive.Server
	if srv, err = keepalive.New(conf); err != nil {
		return cli.Exit(err, 1)
	}

	if err = srv.Serve(); err != nil {
		return cli.Exit(err, 1)
	}
	return nil
}

func keep(c *cli.Context) (err error) {
	return nil
}
