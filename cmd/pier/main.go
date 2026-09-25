package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brutasse/pier/internal/api"
	"github.com/brutasse/pier/internal/auth"
	"github.com/brutasse/pier/internal/config"
	"github.com/brutasse/pier/internal/policy"
	"github.com/brutasse/pier/internal/store"
	"github.com/brutasse/pier/internal/upstream"
)

// version is set at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

var rootCmd = &cobra.Command{
	Use:           "pier",
	Short:         "A private, stateless Maven repository on top of S3-compatible object storage",
	Version:       version,
	SilenceErrors: true,
	SilenceUsage:  true,
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the repository server",
	RunE:  serve,
}

func init() {
	// Keep the pre-cobra output: "pier <version>".
	rootCmd.SetVersionTemplate("pier {{.Version}}\n")
	serveCmd.Flags().String("config", "", "path to the YAML config file (defaults to $PIER_CONFIG)")
	rootCmd.AddCommand(serveCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func serve(cmd *cobra.Command, _ []string) error {
	cfgPath, _ := cmd.Flags().GetString("config")
	if cfgPath == "" {
		cfgPath = os.Getenv("PIER_CONFIG")
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "usage: pier serve -config <file.yaml>")
		os.Exit(2)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(cfgPath)
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

	admins := adminServers(cfg.MetricsPort, cfg.PprofPort, srv)
	for i := range admins {
		a := &admins[i]
		go func() {
			if err := a.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("admin server", "port", a.Addr, "err", err)
				os.Exit(1)
			}
		}()
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
			"metrics_port", cfg.MetricsPort,
			"pprof_port", cfg.PprofPort,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		for range hup {
			newCfg, err := reload(cfgPath, cfg.Listen, srv)
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
	for i := range admins {
		if err := admins[i].Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown", "err", err)
		}
	}
	return nil
}

// newUpstream builds the pull-through fetcher, or nil when no upstream
// repositories are configured (pull-through off).
func newUpstream(cfg *config.Config) *upstream.Upstream {
	if len(cfg.Upstream) == 0 {
		return nil
	}
	return upstream.New(cfg.Upstream, cfg.MaxPullBytes)
}

// adminServers builds the opt-in admin servers: /metrics on
// metricsPort, /debug/pprof/ on pprofPort. A port of zero disables its
// endpoint; both endpoints on the same port share one server.
func adminServers(metricsPort, pprofPort int, srv *api.Server) []http.Server {
	type spec struct {
		port int
		h    http.Handler
	}
	var specs []spec
	if metricsPort > 0 {
		specs = append(specs, spec{metricsPort, srv.AdminHandler(true, false)})
	}
	if pprofPort > 0 {
		if len(specs) == 1 && specs[0].port == pprofPort {
			specs[0].h = srv.AdminHandler(true, true)
		} else {
			specs = append(specs, spec{pprofPort, srv.AdminHandler(false, true)})
		}
	}
	srvs := make([]http.Server, 0, len(specs))
	for _, sp := range specs {
		srvs = append(srvs, http.Server{
			Addr:              fmt.Sprintf(":%d", sp.port),
			Handler:           sp.h,
			ReadHeaderTimeout: 30 * time.Second,
		})
	}
	return srvs
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
