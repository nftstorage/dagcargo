package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"time"

	filactors "github.com/filecoin-project/specs-actors/actors/builtin"
	fslock "github.com/ipfs/go-fs-lock"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	prometheuspush "github.com/prometheus/client_golang/prometheus/push"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
	"golang.org/x/sys/unix"
)

var currentCmd string
var currentCmdLock io.Closer

const filDefaultLookback = 10

var globalFlags = []cli.Flag{
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:  "ipfs-api",
		Value: "http://localhost:5001",
	}),
	&cli.UintFlag{
		Name:  "ipfs-api-timeout",
		Usage: "HTTP API timeout in seconds",
		Value: 240,
	},
	&cli.UintFlag{
		Name:  "ipfs-api-max-workers",
		Usage: "Amount of concurrent IPFS API operations",
		Value: 128,
	},
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:  "lotus-api",
		Value: "http://localhost:1234",
	}),
	&cli.UintFlag{
		Name:  "lotus-lookback-epochs",
		Value: filDefaultLookback,
		DefaultText: fmt.Sprintf("%d epochs / %ds",
			filDefaultLookback,
			filactors.EpochDurationSeconds*filDefaultLookback,
		),
	},
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:  "cargo-pg-connstring",
		Value: "postgres:///postgres?user=cargo&password=&host=/var/run/postgresql",
	}),
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:        "cargo-pg-stats-connstring",
		DefaultText: "defaults to cargo-pg-connstring",
	}),
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:        "aggregate-location-template",
		DefaultText: "  {{ private, read from config file }}  ",
		Hidden:      true,
	}),
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:        "prometheus_push_url",
		DefaultText: "  {{ private, read from config file }}  ",
		Hidden:      true,
		Destination: &promURL,
	}),
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:        "prometheus_push_user",
		DefaultText: "  {{ private, read from config file }}  ",
		Hidden:      true,
		Destination: &promUser,
	}),
	altsrc.NewStringFlag(&cli.StringFlag{
		Name:        "prometheus_push_pass",
		DefaultText: "  {{ private, read from config file }}  ",
		Hidden:      true,
		Destination: &promPass,
	}),
}

var promURL, promUser, promPass string
var nonAlpha = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func main() {

	ctx, cancel := context.WithCancel(context.Background())

	cleanup := func() {
		cancel()
		if cargoDb != nil {
			cargoDb.Close()
		}
		time.Sleep(250 * time.Millisecond) // give a bit of time for various parts to close
	}
	defer cleanup()

	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, unix.SIGINT, unix.SIGTERM)
		<-sigs
		log.Warn("early termination signal received, cleaning up...")
		cancel()
	}()

	// wrap in a defer to always capture endstate/send a metric, even under panic()s
	var (
		t0  time.Time
		err error
	)
	defer func() {

		// shared log/metric emitter
		// ( lock-contention does not count, see invocation below )
		emitEndLogs := func(logSuccess bool) {

			took := time.Since(t0).Truncate(time.Millisecond)
			cmdFqName := nonAlpha.ReplaceAllString("dagcargo_"+currentCmd, `_`)
			logHdr := fmt.Sprintf("=== FINISH '%s' run", currentCmd)
			logArgs := []interface{}{
				"success", logSuccess,
				"took", took.String(),
			}

			tookGauge := prometheus.NewGauge(prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_run_time", cmdFqName),
				Help: "How long did the job take (in milliseconds)",
			})
			tookGauge.Set(float64(took.Milliseconds()))
			successGauge := prometheus.NewGauge(prometheus.GaugeOpts{
				Name: fmt.Sprintf("%s_success", cmdFqName),
				Help: "Whether the job completed with success(1) or failure(0)",
			})

			if logSuccess {
				log.Infow(logHdr, logArgs...)
				successGauge.Set(1)
			} else {
				log.Warnw(logHdr, logArgs...)
				successGauge.Set(0)
			}

			if promErr := prometheuspush.New(promURL, nonAlpha.ReplaceAllString(currentCmd, `_`)).
				Grouping("instance", promInstance).
				BasicAuth(promUser, promPass).
				Collector(tookGauge).
				Collector(successGauge).
				Push(); promErr != nil {
				log.Warnf("push of prometheus metrics to %s failed: %s", promURL, promErr)
			}
		}

		// a panic condition takes precedence
		if r := recover(); r != nil {
			if err == nil {
				err = fmt.Errorf("panic encountered: %s", r)
			} else {
				err = fmt.Errorf("panic encountered (in addition to error '%s'): %s", err, r)
			}
		}

		if err != nil {
			// if we are not interactive - be quiet on a failed lock
			if !isTerm && errors.As(err, new(fslock.LockedError)) {
				cleanup()
				os.Exit(1)
			}

			log.Errorf("%+v", err)
			if currentCmdLock != nil || currentCmd == "get-new-dags" {
				emitEndLogs(false)
			}
			cleanup()
			os.Exit(1)
		} else if currentCmdLock != nil || currentCmd == "get-new-dags" {
			emitEndLogs(true)
		}
	}()

	t0 = time.Now()
	err = (&cli.App{
		Name:   "cargo-cron",
		Usage:  "Misc background processes for dagcargo",
		Before: beforeCliSetup, // obtains locks and emits the proper init loglines
		Flags:  globalFlags,
		Commands: []*cli.Command{
			getNewDags,
			analyzeDags,
			aggregateDags,
			trackDeals,
			pushMetrics,
			pushHeavyMetrics,
		},
	}).RunContext(ctx, os.Args)

}

var beforeCliSetup = func(cctx *cli.Context) error {

	//
	// urfave/cli-specific add config file
	if err := altsrc.InitInputSourceWithContext(
		globalFlags,
		func(context *cli.Context) (altsrc.InputSourceContext, error) {
			return altsrc.NewTomlSourceFromFile("dagcargo.toml")
		},
	)(cctx); err != nil {
		return err
	}
	// urfave/cli-specific end
	//

	// figure out what is the command that was invoked
	if len(os.Args) > 1 {

		cmdNames := make(map[string]string)
		for _, c := range cctx.App.Commands {
			cmdNames[c.Name] = c.Name
			for _, a := range c.Aliases {
				cmdNames[a] = c.Name
			}
		}

		var firstCmdOccurrence string
		for i := 1; i < len(os.Args); i++ {

			// if we are in help context - no locks and no start/stop timers
			if os.Args[i] == `-h` || os.Args[i] == `--help` {
				return nil
			}

			if firstCmdOccurrence != "" {
				continue
			}
			firstCmdOccurrence = cmdNames[os.Args[i]]
		}

		// wrong cmd or something
		if firstCmdOccurrence == "" {
			return nil
		}

		currentCmd = firstCmdOccurrence

		// get-new-dags does its own locking for now
		if currentCmd != "get-new-dags" {
			var err error
			if currentCmdLock, err = fslock.Lock(os.TempDir(), "cargocron-"+currentCmd); err != nil {
				return err
			}
			log.Infow(fmt.Sprintf("=== BEGIN '%s' run", currentCmd))
		}

		// init the shared DB connection: do it here, since now we know the config *AND*
		// we want the maxConn counter shared, singleton-style
		dbConnCfg, err := pgxpool.ParseConfig(cctx.String("cargo-pg-connstring"))
		if err != nil {
			return err
		}
		cargoDb, err = pgxpool.ConnectConfig(cctx.Context, dbConnCfg)
		if err != nil {
			return err
		}
	}

	return nil
}
