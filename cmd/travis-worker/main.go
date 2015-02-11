package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/Sirupsen/logrus/hooks/sentry"
	"github.com/codegangsta/cli"
	"github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/librato"
	"github.com/streadway/amqp"
	"github.com/travis-ci/worker/lib"
	"github.com/travis-ci/worker/lib/backend"
	"github.com/travis-ci/worker/lib/context"
	gocontext "golang.org/x/net/context"
)

func main() {
	app := cli.NewApp()
	app.Usage = "Travis Worker daemon"
	app.Version = lib.VersionString
	app.Action = runWorker

	app.Run(os.Args)
}

func runWorker(c *cli.Context) {
	ctx := gocontext.Background()
	logger := context.LoggerFromContext(ctx)

	config := lib.EnvToConfig()
	logger.WithField("config", fmt.Sprintf("%+v", config)).Debug("read config")

	if config.SentryDSN != "" {
		sentryHook, err := logrus_sentry.NewSentryHook(config.SentryDSN, []logrus.Level{logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel})
		if err != nil {
			context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't create sentry hook")
		}

		logrus.AddHook(sentryHook)
	}

	if config.LibratoEmail != "" && config.LibratoToken != "" && config.LibratoSource != "" {
		context.LoggerFromContext(ctx).Info("starting librato metrics reporter")
		go librato.Librato(metrics.DefaultRegistry, time.Minute, config.LibratoEmail, config.LibratoToken, config.LibratoSource, []float64{0.95}, time.Millisecond)
	} else {
		context.LoggerFromContext(ctx).Info("starting logger metrics reporter")
		go metrics.Log(metrics.DefaultRegistry, time.Minute, log.New(os.Stderr, "metrics: ", log.Lmicroseconds))
	}

	amqpConn, err := amqp.Dial(config.AmqpURI)
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't connect to AMQP")
		return
	}

	context.LoggerFromContext(ctx).Debug("connected to AMQP")

	generator := lib.NewBuildScriptGenerator(config.BuildAPIURI)
	provider, err := backend.NewProvider(config.ProviderName, config.ProviderConfig)
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't create backend provider")
		return
	}

	commandDispatcher := lib.NewCommandDispatcher(ctx, amqpConn)
	go commandDispatcher.Run()

	pool := &lib.ProcessorPool{
		Context:   ctx,
		Conn:      amqpConn,
		Provider:  provider,
		Generator: generator,
		Canceller: commandDispatcher,
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		<-signalChan
		context.LoggerFromContext(ctx).Info("SIGTERM received, starting graceful shutdown")
		pool.GracefulShutdown()
	}()

	pool.Run(config.PoolSize, config.QueueName)

	err = amqpConn.Close()
	if err != nil {
		context.LoggerFromContext(ctx).WithField("err", err).Error("couldn't close AMQP connection cleanly")
		return
	}
}