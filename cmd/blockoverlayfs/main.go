// Command blockoverlayfs is an NRI plugin for containerd that gives each Kubernetes container's writable layer a
// size-limited block device of its own.
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
	"sync"
	"syscall"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/nri/pkg/stub"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// reconcileInterval paces reconciliation, and reconcileRetry the first run after one that left a layer unsettled.
	reconcileInterval = 10 * time.Minute
	reconcileRetry    = 30 * time.Second
	// connectRetry paces registering again while containerd is not there. It is short because a container created
	// before the plugin is back never gets a block layer.
	connectRetry = 100 * time.Millisecond
	// maxRetry bounds the backoff, which starts at connectRetry, when containerd is there but drops the plugin.
	maxRetry = 30 * time.Second
	// settled is how long a connection has to last for the backoff to start over.
	settled = time.Second
)

var (
	// errConnectionClosed is containerd closing the plugin's connection: it is restarting, or it dropped the plugin
	// for missing the deadline of a request.
	errConnectionClosed = errors.New("connection closed")
	// errConnectionRefused is containerd closing the connection as soon as the plugin had registered.
	errConnectionRefused = errors.New("connection closed right after registering")
)

var (
	configPath   = flag.String("config", config.DefaultConfigPath, "path to the configuration file")
	logLevel     = flag.String("log-level", "", "logging level [trace, debug, info, warn, error, fatal, panic], overriding log_level")
	readyFile    = flag.String("ready-file", "", "file to keep while the plugin is registered with NRI, for a readiness probe")
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
	podSelector, err := metav1.LabelSelectorAsSelector(&cfg.PodSelector)
	if err != nil {
		return fmt.Errorf("pod_selector: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.G(ctx).WithFields(log.Fields{"version": version.Version, "revision": version.Revision}).Info("starting " + binaryName)

	ready := readiness(*readyFile)
	// A run that was killed leaves the file behind.
	ready.set(ctx, false)
	if err := block.Preflight(cfg.Containerd.SnapshotterRoot); err != nil {
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
	log.G(ctx).WithFields(log.Fields{
		"included_namespaces": cfg.IncludedNamespaces,
		"excluded_namespaces": cfg.ExcludedNamespaces,
		"pod_selector":        podSelector.String(),
	}).Info("pod filters")

	p := plugin.New(runtime, layers, policy, cfg.IncludedNamespaces, cfg.ExcludedNamespaces, podSelector)
	var reconciler sync.WaitGroup
	reconciler.Go(func() { p.RunReconciler(ctx, reconcileInterval, reconcileRetry) })
	// Clear away what a node restart or an earlier run left behind before the first container asks for a layer.
	p.Kick()
	err = serve(ctx, p, cfg.NRI, ready)
	// Layers stay mounted when the plugin stops: running containers keep using them.
	reconciler.Wait()
	p.Wait()
	return err
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
// what happened meanwhile. ready says whether the plugin is registered.
func serve(ctx context.Context, p *plugin.Plugin, cfg config.NRIConfig, ready readiness) error {
	var (
		mu sync.Mutex
		s  stub.Stub
	)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// The request being handled finishes its layer before the connection goes.
		p.Drain()
		mu.Lock()
		defer mu.Unlock()
		if s != nil {
			s.Stop()
		}
	}()
	// Requests outlive the signal that stops the plugin: containerd's own deadline ends them.
	serving := context.WithoutCancel(ctx)

	var (
		retry = connectRetry
		// since is when the plugin found itself not registered, zero while it is.
		since time.Time
	)
	for ctx.Err() == nil {
		conn, err := net.Dial("unix", cfg.SocketPath)
		if err == nil {
			var next stub.Stub
			next, err = stub.New(p,
				stub.WithPluginName(pluginName),
				stub.WithPluginIdx(cfg.PluginIndex),
				stub.WithConnection(conn),
			)
			if err != nil {
				_ = conn.Close()
				return fmt.Errorf("create the NRI plugin: %w", err)
			}
			mu.Lock()
			s = next
			mu.Unlock()
			// A stub that fails to start closes the connection.
			err = next.Start(serving)
		}
		if err == nil {
			if ctx.Err() != nil {
				// Stopped before it had started: nothing else will stop it.
				s.Stop()
				break
			}
			fields := log.Fields{"socket": cfg.SocketPath}
			if !since.IsZero() {
				fields["after"] = time.Since(since).Round(time.Millisecond).String()
			}
			log.G(ctx).WithFields(fields).Info("registered with NRI")
			since = time.Time{}
			ready.set(ctx, true)
			registered := time.Now()
			s.Wait()
			ready.set(ctx, false)
			if ctx.Err() != nil {
				break
			}
			if time.Since(registered) < settled {
				err = errConnectionRefused
			} else {
				err = errConnectionClosed
				retry = connectRetry
			}
		}
		var delay time.Duration
		delay, retry = retryDelay(err, retry)
		logger := log.G(ctx).WithError(err).WithField("retry_in", delay.String())
		if since.IsZero() || delay != connectRetry {
			logger.Warn("not registered with NRI")
		} else {
			logger.Debug("not registered with NRI")
		}
		if since.IsZero() {
			since = time.Now()
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	<-stopped
	log.G(ctx).Info("shutting down")
	return nil
}

// retryDelay says how long to wait before registering again after err, and the backoff to use after the next
// failure. A closed connection and a socket that is missing or refuses connections are containerd restarting, which
// is retried every connectRetry. Anything else is containerd refusing the plugin, and backs off.
func retryDelay(err error, backoff time.Duration) (delay, next time.Duration) {
	if errors.Is(err, errConnectionClosed) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return connectRetry, backoff
	}
	return backoff, min(2*backoff, maxRetry)
}

// readiness is the path of a file that exists while the plugin is registered with NRI, which is what a readiness
// probe looks for. An empty path keeps no file.
type readiness string

func (r readiness) set(ctx context.Context, ready bool) {
	if r == "" {
		return
	}
	path := string(r)
	var err error
	if ready {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = os.WriteFile(path, nil, 0o600)
		}
	} else if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		log.G(ctx).WithError(err).WithField("ready", ready).Warn("failed to update the readiness file")
	}
}
