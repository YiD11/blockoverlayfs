package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/log"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	"google.golang.org/grpc"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/config"
	"github.com/YiD11/blockoverlayfs/pkg/containermeta"
	"github.com/YiD11/blockoverlayfs/pkg/snapshot"
	"github.com/YiD11/blockoverlayfs/version"
)

const binaryName = "containerd-blockoverlayfs-grpc"

var (
	address      = flag.String("address", config.DefaultAddress, "address for the snapshotter's gRPC server")
	configPath   = flag.String("config", config.DefaultConfigPath, "path to the configuration file")
	logLevel     = flag.String("log-level", config.DefaultLogLevel, "logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir      = flag.String("root", config.DefaultRoot, "path to the root directory for this snapshotter")
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
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := log.SetLevel(cfg.LogLevel); err != nil {
		return err
	}
	policy, err := cfg.WritableLayer.Policy()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.G(ctx).WithFields(log.Fields{"version": version.Version, "revision": version.Revision}).Info("starting " + binaryName)

	innerRoot := filepath.Join(cfg.Root, "overlay")
	inner, err := snapshot.NewOverlay(innerRoot, snapshot.OverlayOptions{SyncRemove: cfg.Overlay.SyncRemove, MountOptions: cfg.Overlay.MountOptions})
	if err != nil {
		return err
	}
	// Opened even with block layers off: layers recorded while they were on keep being served.
	layers, err := block.Open(ctx, filepath.Join(cfg.Root, "blocks"))
	if err != nil {
		_ = inner.Close()
		return err
	}
	lookup, err := containermeta.Dial(cfg.Containerd.Address, time.Duration(cfg.Containerd.LookupTimeout))
	if err != nil {
		_ = layers.Close()
		_ = inner.Close()
		return err
	}
	defer lookup.Close()

	opts := snapshot.Options{Containers: lookup, UpperdirLabel: cfg.Overlay.UpperdirLabel}
	if policy.Enabled {
		if err := block.Preflight(ctx, layers.Root(), innerRoot); err != nil {
			_ = layers.Close()
			_ = inner.Close()
			return err
		}
		opts.Decider = &snapshot.CRIDecider{Lookup: lookup, Policy: policy}
	}
	log.G(ctx).WithFields(log.Fields{
		"enabled":         policy.Enabled,
		"limit_offset":    policy.LimitOffset,
		"request_percent": policy.RequestPercent,
		"default_size":    policy.DefaultSize,
	}).Info("writable layer policy")

	sn := snapshot.New(inner, layers, opts)
	defer sn.Close()
	return serve(ctx, cfg.Address, sn)
}

func loadConfig() (config.Config, error) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := config.Load(*configPath)
	// Only the default path may be absent; a config file named on the command line must exist.
	if errors.Is(err, fs.ErrNotExist) && !set["config"] {
		cfg, err = config.Default(), nil
	}
	if err != nil {
		return config.Config{}, err
	}
	if set["address"] {
		cfg.Address = *address
	}
	if set["root"] {
		cfg.Root = *rootDir
	}
	if set["log-level"] {
		cfg.LogLevel = *logLevel
	}
	return cfg, nil
}

func serve(ctx context.Context, address string, sn snapshots.Snapshotter) error {
	if err := os.MkdirAll(filepath.Dir(address), 0o700); err != nil {
		return err
	}
	// A socket left behind by a previous run makes Listen fail with EADDRINUSE.
	if err := os.RemoveAll(address); err != nil {
		return err
	}
	l, err := net.Listen("unix", address)
	if err != nil {
		return err
	}

	rpc := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(rpc, snapshotservice.FromSnapshotter(sn))
	errCh := make(chan error, 1)
	go func() { errCh <- rpc.Serve(l) }()
	notify(ctx, sddaemon.SdNotifyReady)
	log.G(ctx).WithField("address", address).Info("serving snapshots API")

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.G(ctx).Info("shutting down")
	notify(ctx, sddaemon.SdNotifyStopping)
	// Writable layers must stay mounted across restarts because running containers keep using them.
	rpc.GracefulStop()
	return nil
}

func notify(ctx context.Context, state string) {
	if _, err := sddaemon.SdNotify(false, state); err != nil {
		log.G(ctx).WithError(err).Warn("sd_notify failed")
	}
}
