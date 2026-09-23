package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

// version is set at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

// newUpstream builds the pull-through fetcher, or nil when no upstream
// repositories are configured (pull-through off).
func newUpstream(cfg *config.Config) *upstream.Upstream {
	if len(cfg.Upstream) == 0 {
		return nil
	}
	return upstream.New(cfg.Upstream, cfg.MaxPullBytes)
}

func main() {
	cfgPath := flag.String("config", os.Getenv("PIER_CONFIG"), "path to the YAML config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pier", version)
		return
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "usage: pier -config <file.yaml>")
		os.Exit(2)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	st, err := store.NewS3(ctx, cfg.S3)
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	pol, err := policy.New(cfg.Policy.Rules, *cfg.Policy.DefaultDeny)
	if err != nil {
		log.Error("policy", "err", err)
		os.Exit(1)
	}
	verifier := auth.NewVerifier(cfg.Auth.Issuers)

	if cfg.Auth.Disabled {
		log.Warn("authentication and authorization are DISABLED (auth.disabled=true) — local development only")
	}

	up := newUpstream(cfg)
	srv := api.New(cfg, st, verifier, pol, up, log)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Info("listening",
			"addr", cfg.Listen,
			"bucket", cfg.S3.Bucket,
			"prefix", cfg.S3.Prefix,
			"immutable_releases", *cfg.ImmutableReleases,
			"auth_disabled", cfg.Auth.Disabled,
			"upstreams", len(cfg.Upstream),
			"reserved_groups", len(cfg.ReservedGroups),
			"metadata_ttl", cfg.MetadataTTLOrDefault().String(),
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		for range hup {
			newCfg, err := reload(*cfgPath, cfg.Listen, srv)
			if err != nil {
				log.Error("config reload failed, keeping current configuration", "err", err)
				continue
			}
			log.Info("config reloaded",
				"bucket", newCfg.S3.Bucket,
				"prefix", newCfg.S3.Prefix,
				"rules", len(newCfg.Policy.Rules),
				"issuers", len(newCfg.Auth.Issuers),
				"immutable_releases", *newCfg.ImmutableReleases,
				"auth_disabled", newCfg.Auth.Disabled,
				"upstreams", len(newCfg.Upstream),
				"reserved_groups", len(newCfg.ReservedGroups),
			)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// reload loads and validates the config at path, checks the storage
// backend, and atomically swaps it into the server. The listen address
// cannot change on reload; on any failure the current configuration stays
// in place.
func reload(path, listen string, srv *api.Server) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg.Listen != listen {
		return nil, fmt.Errorf("listen address cannot change on reload (%q -> %q); restart instead", listen, cfg.Listen)
	}
	ctx := context.Background()
	st, err := store.NewS3(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}
	if err := st.Ping(ctx); err != nil {
		return nil, err
	}
	pol, err := policy.New(cfg.Policy.Rules, *cfg.Policy.DefaultDeny)
	if err != nil {
		return nil, err
	}
	srv.Update(cfg, st, auth.NewVerifier(cfg.Auth.Issuers), pol, newUpstream(cfg))
	return cfg, nil
}
