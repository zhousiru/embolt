package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/zhousiru/embolt/internal/cache"
	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
	"github.com/zhousiru/embolt/internal/profile"
	"github.com/zhousiru/embolt/internal/proxy"
	"github.com/zhousiru/embolt/internal/webapi"
	"github.com/zhousiru/embolt/web"
)

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "/config/config.yaml", "config file")
	debug := fs.Bool("debug", false, "log at debug level")
	fs.Parse(args)

	journal := webapi.NewJournal(500)
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(journal.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))))

	store, err := config.Open(*path)
	if err != nil {
		return err
	}
	cfg := store.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := nodes.UseDNS(cfg.DNS); err != nil { // once: mihomo's resolver is a global
		return err
	}

	pool := nodes.NewPool(store)
	stats := measure.NewStats(store)
	ctrl := control.New(store, pool, stats)
	prof, err := profile.Open(filepath.Join(cfg.DataDir, "profile.json"))
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	px := proxy.New(store, ctrl, stats, cache.Open(filepath.Join(cfg.DataDir, "cache"), cfg.Cache.SizeMB<<20), prof)
	pane := webapi.New(webapi.Deps{
		Cfg: store, Ctrl: ctrl, Journal: journal, UI: web.UI(),
		Version: version, Started: time.Now(), Items: px,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { store.Watch(ctx); return nil })
	g.Go(func() error { pool.Run(ctx); return nil })
	g.Go(func() error { ctrl.Run(ctx); return nil })
	g.Go(func() error { stats.Persist(ctx, filepath.Join(cfg.DataDir, "estimates.json")); return nil })
	g.Go(func() error { prof.Persist(ctx); return nil })

	listen(ctx, g, "ingress", cfg.Listen, px, "", "")
	if cfg.TLS.Listen != "" {
		listen(ctx, g, "ingress-tls", cfg.TLS.Listen, px, cfg.TLS.Cert, cfg.TLS.Key)
	}
	listen(ctx, g, "pane", cfg.Web.Listen, pane, "", "")
	slog.Info("embolt started", "version", version, "upstream", cfg.Upstream.Base().Host)
	err = g.Wait()
	px.Close()
	return err
}

// listen serves h on addr until ctx ends, then drains for up to 10 s.
// No write timeout: media responses stream for hours.
func listen(ctx context.Context, g *errgroup.Group, name, addr string, h http.Handler, cert, key string) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	g.Go(func() error {
		var err error
		if cert != "" {
			err = srv.ListenAndServeTLS(cert, key)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	g.Go(func() error {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		slog.Debug("shutting down", "listener", name)
		return srv.Shutdown(shut)
	})
}
