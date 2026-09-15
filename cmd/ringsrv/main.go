package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vmkteam/ringsrv/pkg/app"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/BurntSushi/toml"
	"github.com/getsentry/sentry-go"
	"github.com/namsral/flag"
	"github.com/vmkteam/appkit"
	"github.com/vmkteam/embedlog"
)

const appName = "ringsrv"

var (
	fs           = flag.NewFlagSetWithEnvPrefix(os.Args[0], strings.ToUpper(appName), 0)
	flConfigPath = fs.String("config", "config.toml", "Path to config file")
	flVerbose    = fs.Bool("verbose", false, "enable debug output")
	flJSONLogs   = fs.Bool("json", false, "enable json output")
	flDev        = fs.Bool("dev", false, "enable dev mode")
	flCheck      = fs.Bool("check-config", false, "validate the config and the catalogue it names, then exit")
	cfg          app.Config
)

func main() {
	flag.DefaultConfigFlagname = "config.flag"
	exitOnError(fs.Parse(os.Args[1:]))

	// setup logger
	sl, ctx := embedlog.NewLogger(*flVerbose, *flJSONLogs), context.Background()
	if *flDev {
		sl = embedlog.NewDevLogger()
	}
	slog.SetDefault(sl.Log()) // set default logger

	version := appkit.Version()
	sl.Print(ctx, "starting", "app", appName, "version", version)
	md, err := toml.DecodeFile(*flConfigPath, &cfg)
	exitOnError(err)
	// The decoder ignores keys it does not know, so a typo — or a renamed Config
	// field — silently switches a whole section off. The catalogue has always
	// refused to load on one; the instance config used to log and carry on,
	// which is how a feature ships dark.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		exitOnError(fmt.Errorf("config %s: unknown keys: %s", *flConfigPath, strings.Join(keys, ", ")))
	}
	// Limits left at zero get defaults and a log line each: the operator
	// should be able to tell which values they chose and which they did not.
	applied, err := cfg.Validate()
	if err != nil {
		exitOnError(fmt.Errorf("config %s: %w", *flConfigPath, err))
	}
	for _, d := range applied {
		sl.Print(ctx, "config default applied", "setting", d, "config", *flConfigPath)
	}

	// check-config stops here, after both halves have been read: the instance
	// config above and the catalogue it names below. These two files are edited
	// on a contour, by hand, under a template, and the only other thing that
	// reads them is a start — a bad moment to find out. The tests in pkg/app
	// check the files that live in git; this checks the ones that do not.
	if *flCheck {
		cat, err := target.Load(cfg.Catalog.Path)
		exitOnError(err)
		sl.Print(ctx, "config ok",
			"config", *flConfigPath,
			"catalog", cfg.Catalog.Path,
			"env", cat.Env,
			"profiles", len(cat.Profiles),
			"databases", len(cat.Databases),
			"roles", len(cat.Roles),
			"repos", len(cat.Repos))
		return
	}

	// enable sentry
	if cfg.Sentry.DSN != "" {
		exitOnError(sentry.Init(sentry.ClientOptions{
			Dsn:         cfg.Sentry.DSN,
			Environment: cfg.Sentry.Environment,
			Release:     version,
		}))
	}

	// create & run app
	a := app.New(appName, sl, cfg)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// runErr carries a Run failure to the exit code: without it a boot error
	// walks the same path as Ctrl+C and the process exits 0, which an
	// orchestrator reads as success.
	runErr := make(chan error, 1)
	var exitErr error

	// run app and send panic to sentry
	go func() {
		defer func() {
			if err := recover(); err != nil {
				sentry.CurrentHub().Recover(err)
				sentry.Flush(time.Second * 3)
				panic(err)
			}
		}()

		er := a.Run(ctx)
		if errors.Is(er, http.ErrServerClosed) {
			er = nil
		}

		// exit after run failed
		a.PrintOrErr(ctx, "server stopped", er)
		runErr <- er
	}()

	select {
	case <-quit: // Ctrl+C / SIGTERM — the regular path, exit 0
	case exitErr = <-runErr: // Run returned on its own
	}

	if err := a.Shutdown(5 * time.Second); err != nil {
		a.Error(ctx, "shutting down service", "err", err)
	}

	if exitErr != nil {
		os.Exit(1)
	}
}

// exitOnError calls log.Fatal if err wasn't nil.
func exitOnError(err error) {
	if err != nil {
		//nolint:sloglint,gosec // the app logger does not exist yet; the text is a boot-time error, not caller input
		slog.Error(err.Error())
		os.Exit(1)
	}
}
