package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	translator.RegisterBuiltin()

	logger, err := reqlog.Open(cfg.RequestLog)
	if err != nil {
		log.Fatalf("request-log: %v", err)
	}
	defer func() {
		if err := logger.Close(); err != nil {
			log.Printf("request-log close: %v", err)
		}
	}()
	if cfg.RequestLog.Enabled {
		log.Printf("request-log enabled backend=%s retention=%s store-body=%v",
			cfg.RequestLog.Backend, cfg.RequestLog.Retention, cfg.RequestLog.StoreBody)
	}

	srv := server.New(cfg, logger)

	// Shared reload entry: SIGHUP and file watcher both call this.
	var reloadMu sync.Mutex
	reload := func(reason string) {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		next, err := config.Load(*configPath)
		if err != nil {
			config.LogReloadError(err)
			return
		}
		if err := srv.Reload(next); err != nil {
			config.LogReloadError(err)
			return
		}
		log.Printf("config reload ok (%s)", reason)
		if next.Debug {
			log.SetFlags(log.LstdFlags | log.Lshortfile)
		} else {
			log.SetFlags(log.LstdFlags)
		}
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	watcher := config.NewWatcher(*configPath, func() { reload("file change") }, time.Second, 300*time.Millisecond)
	go watcher.Run(watchCtx)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		select {
		case err := <-errCh:
			watchCancel()
			if err != nil {
				log.Fatalf("server: %v", err)
			}
			return
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				log.Printf("signal %v, reloading config", sig)
				reload("SIGHUP")
				continue
			}
			log.Printf("signal %v, shutting down", sig)
			watchCancel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := srv.Shutdown(ctx); err != nil {
				log.Printf("shutdown: %v", err)
			}
			cancel()
			return
		}
	}
}
