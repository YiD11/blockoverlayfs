// Command blockoverlayfs is an NRI plugin for containerd that gives each Kubernetes container's writable layer a
// size-limited block device of its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/nri/pkg/stub"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/config"
	"github.com/YiD11/blockoverlayfs/pkg/containerd"
	"github.com/YiD11/blockoverlayfs/pkg/plugin"
	"github.com/YiD11/blockoverlayfs/version"
)

const (
	binaryName = "blockoverlayfs"
	// pluginName is the name the plugin registers with NRI.
	pluginName = "blockoverlayfs"
	// reconcileInterval paces the reconciliation that catches what a lost NRI event left behind.
	reconcileInterval = 10 * time.Minute
	minRetry          = time.Second
	maxRetry          = 30 * time.Second
)

var (
	configPath   = flag.String("config", config.DefaultConfigPath, "path to the configuration file")
	logLevel     = flag.String("log-level", "", "logging level [trace, debug, info, warn, error, fatal, panic], overriding log_level")
	printVersion = flag.Bool("version", false, "print the version and exit")
)

func main() {
	flag.Parse()
	if *printVersion {
		fmt.Println(binaryName, version.Version, version.Revision)
		return
	}
	if err := run(); err != nil {
		log.L.WithError(err).Fatal(binaryName + " failed")
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := log.SetLevel(cfg.LogLevel); err != nil {
		return err
	}
	policy, err := cfg.Size.Policy()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.G(ctx).WithFields(log.Fields{"version": version.Version, "revision": version.Revision}).Info("starting " + binaryName)

	if err := block.Preflight(); err != nil {
		return err
	}
	layers, err := block.Open(cfg.Root)
	if err != nil {
		return err
	}
	runtime, err := containerd.Dial(containerd.Options{
		Address:         cfg.Containerd.Address,
		Namespace:       cfg.Containerd.Namespace,
		Snapshotter:     cfg.Containerd.Snapshotter,
		SnapshotterRoot: cfg.Containerd.SnapshotterRoot,
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
	log.G(ctx).WithFields(log.Fields{
		"limit_offset":    policy.LimitOffset,
		"request_percent": policy.RequestPercent,
		"default_size":    policy.DefaultSize,
	}).Info("size policy")

	p := plugin.New(runtime, layers, policy)
	// Layers stay mounted when the plugin stops: running containers keep using them.
	defer p.Wait()
	go p.RunReconciler(ctx, reconcileInterval)
	return serve(ctx, p, cfg.NRI)
}

// loadConfig reads the configuration file. Only the default one may be missing.
func loadConfig() (config.Config, error) {
	named := false
	flag.Visit(func(f *flag.Flag) { named = named || f.Name == "config" })
	cfg, err := config.Load(*configPath)
	if errors.Is(err, fs.ErrNotExist) && !named {
		return config.Default(), nil
	}
	return cfg, err
}

// serve keeps the plugin registered with NRI until ctx is done. containerd drops the connection when it restarts,
// and when the plugin misses the deadline of a request; the plugin then registers again, and Synchronize reconciles
// what happened meanwhile. systemd hears that the plugin is ready once it has first registered.
func serve(ctx context.Context, p *plugin.Plugin, cfg config.NRIConfig) error {
	s, err := stub.New(p,
		stub.WithPluginName(pluginName),
		stub.WithPluginIdx(cfg.PluginIndex),
		stub.WithSocketPath(cfg.SocketPath),
	)
	if err != nil {
		return fmt.Errorf("create the NRI plugin: %w", err)
	}
	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	ready := false
	retry := minRetry
	for ctx.Err() == nil {
		err := s.Start(ctx)
		if err == nil {
			log.G(ctx).WithField("socket", cfg.SocketPath).Info("registered with NRI")
			if !ready {
				notify(ctx, sddaemon.SdNotifyReady)
				ready = true
			}
			registered := time.Now()
			s.Wait()
			if time.Since(registered) > maxRetry {
				retry = minRetry
			}
			err = errors.New("connection closed")
		}
		if ctx.Err() != nil {
			break
		}
		log.G(ctx).WithError(err).WithField("retry_in", retry.String()).Warn("not registered with NRI")
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
		retry = min(2*retry, maxRetry)
	}
	log.G(ctx).Info("shutting down")
	notify(ctx, sddaemon.SdNotifyStopping)
	return nil
}

func notify(ctx context.Context, state string) {
	if _, err := sddaemon.SdNotify(false, state); err != nil {
		log.G(ctx).WithError(err).Warn("sd_notify failed")
	}
}
