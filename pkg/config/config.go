package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

const (
	DefaultAddress    = "/run/containerd-blockoverlayfs-grpc/containerd-blockoverlayfs-grpc.sock"
	DefaultConfigPath = "/etc/containerd-blockoverlayfs-grpc/config.toml"
	// DefaultRoot is where containerd keeps its own snapshotters. That puts the snapshots on the filesystem
	// holding containerd's data, and it is the path CRI falls back to for a snapshotter registered as
	// blockoverlayfs. CRI reports it to kubelet as the image filesystem.
	DefaultRoot              = "/var/lib/containerd/io.containerd.snapshotter.v1.blockoverlayfs"
	DefaultLogLevel          = "info"
	DefaultContainerdAddress = "/run/containerd/containerd.sock"
	DefaultLookupTimeout     = Duration(3 * time.Second)
)

// Sources of a writable layer's size, as on the containerd branch this daemon replaces.
const (
	SourceOff              = "off"
	SourceEphemeralStorage = "ephemeral-storage"
)

// MaxSize bounds the sizes accepted in writable_layer.
const MaxSize = int64(1) << 60

var maxQuantity = resource.NewQuantity(MaxSize, resource.BinarySI)

type Config struct {
	Root          string              `toml:"root"`
	Address       string              `toml:"address"`
	LogLevel      string              `toml:"log_level"`
	Containerd    ContainerdConfig    `toml:"containerd"`
	Overlay       OverlayConfig       `toml:"overlay"`
	WritableLayer WritableLayerConfig `toml:"writable_layer"`
}

type ContainerdConfig struct {
	Address string `toml:"address"`
	// LookupTimeout bounds reading a container record, which happens while containerd waits for Mounts.
	LookupTimeout Duration `toml:"lookup_timeout"`
}

// OverlayConfig mirrors the options of containerd's built-in overlayfs plugin.
type OverlayConfig struct {
	SyncRemove    bool     `toml:"sync_remove"`
	UpperdirLabel bool     `toml:"upperdir_label"`
	MountOptions  []string `toml:"mount_options"`
}

// WritableLayerConfig uses the keys of the containerd branch's writable_layer table. Sizes are Kubernetes
// quantities such as "256Mi", the notation Pod ephemeral-storage declarations use.
type WritableLayerConfig struct {
	Source         string `toml:"source"`
	LimitOffset    string `toml:"limit_offset"`
	RequestPercent int64  `toml:"request_percent"`
	DefaultSize    string `toml:"default_size"`
}

// Duration is written as a Go duration string, such as "3s".
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func Default() Config {
	return Config{
		Root:       DefaultRoot,
		Address:    DefaultAddress,
		LogLevel:   DefaultLogLevel,
		Containerd: ContainerdConfig{Address: DefaultContainerdAddress, LookupTimeout: DefaultLookupTimeout},
	}
}

// Decoding on top of Default lets a config file set only the keys it changes.
func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	if err := toml.NewDecoder(f).DisallowUnknownFields().Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks what decoding alone cannot.
func (c Config) Validate() error {
	if !filepath.IsAbs(c.Root) {
		return fmt.Errorf("root %q must be an absolute path", c.Root)
	}
	if c.Address == "" {
		return errors.New("address must not be empty")
	}
	if c.Containerd.Address == "" {
		return errors.New("containerd.address must not be empty")
	}
	if c.Containerd.LookupTimeout <= 0 {
		return errors.New("containerd.lookup_timeout must be positive")
	}
	_, err := c.WritableLayer.Policy()
	return err
}

// Policy validates the table and converts it; validation follows ValidateWritableLayer on the containerd branch.
func (c WritableLayerConfig) Policy() (sizing.Policy, error) {
	source := c.Source
	if source == "" {
		source = SourceOff
	}
	switch source {
	case SourceOff:
		if c.LimitOffset != "" || c.RequestPercent != 0 || c.DefaultSize != "" {
			return sizing.Policy{}, fmt.Errorf("invalid writable_layer: source is %q, so limit_offset, request_percent and default_size would never be used", SourceOff)
		}
		return sizing.Policy{}, nil
	case SourceEphemeralStorage:
	default:
		return sizing.Policy{}, fmt.Errorf("invalid writable_layer.source %q: not one of %q or %q", c.Source, SourceOff, SourceEphemeralStorage)
	}

	p := sizing.Policy{Enabled: true, RequestPercent: sizing.DefaultRequestPercent}
	if c.LimitOffset != "" {
		v, err := parseSize("writable_layer.limit_offset", c.LimitOffset)
		if err != nil {
			return sizing.Policy{}, err
		}
		p.LimitOffset = v
	}
	if c.RequestPercent != 0 {
		if c.RequestPercent < 100 {
			return sizing.Policy{}, fmt.Errorf("invalid writable_layer.request_percent %d: smaller than 100; use at least 100, or 0 for the default of %d", c.RequestPercent, sizing.DefaultRequestPercent)
		}
		p.RequestPercent = c.RequestPercent
	}
	if c.DefaultSize != "" {
		v, err := parseSize("writable_layer.default_size", c.DefaultSize)
		if err != nil {
			return sizing.Policy{}, err
		}
		if v < sizing.MinSize {
			return sizing.Policy{}, fmt.Errorf("invalid writable_layer.default_size %q: smaller than the smallest writable layer of %d bytes", c.DefaultSize, sizing.MinSize)
		}
		p.DefaultSize = v
	}
	return p, nil
}

func parseSize(name, value string) (int64, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	if q.Sign() <= 0 || q.Cmp(*maxQuantity) > 0 {
		return 0, fmt.Errorf("invalid %s %q: outside the range of 1 byte to 1Ei", name, value)
	}
	// Value rounds a fractional byte count up.
	return q.Value(), nil
}
