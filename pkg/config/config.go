// Package config is the plugin's configuration file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

const (
	DefaultConfigPath = "/etc/blockoverlayfs/config.toml"
	DefaultLogLevel   = "info"
	// DefaultRoot holds one sparse ext4 image per container that has a block writable layer.
	DefaultRoot = "/var/lib/containerd/blockoverlayfs"

	DefaultNRISocketPath = "/var/run/nri/nri.sock"
	DefaultPluginIndex   = "10"

	DefaultContainerdAddress = "/run/containerd/containerd.sock"
	// DefaultNamespace is the containerd namespace CRI creates Kubernetes containers in.
	DefaultNamespace       = "k8s.io"
	DefaultSnapshotter     = "overlayfs"
	DefaultSnapshotterRoot = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs"
)

// MaxSize bounds the sizes accepted in the size table.
const MaxSize = int64(1) << 60

var (
	maxQuantity = resource.NewQuantity(MaxSize, resource.BinarySI)
	// NRI orders plugins by a two-digit index.
	pluginIndex = regexp.MustCompile(`^[0-9]{2}$`)
)

type Config struct {
	LogLevel string `toml:"log_level"`
	Root     string `toml:"root"`
	// IncludedNamespaces and ExcludedNamespaces name Kubernetes namespaces of Pods. A container gets a block layer
	// only if its Pod's namespace is not excluded and, when IncludedNamespaces is not empty, is included.
	IncludedNamespaces []string         `toml:"included_namespaces"`
	ExcludedNamespaces []string         `toml:"excluded_namespaces"`
	NRI                NRIConfig        `toml:"nri"`
	Containerd         ContainerdConfig `toml:"containerd"`
	Size               SizeConfig       `toml:"size"`
}

// NRIConfig is how the plugin connects to containerd's NRI.
type NRIConfig struct {
	SocketPath string `toml:"socket_path"`
	// PluginIndex orders the plugin among other NRI plugins: two digits, lower first.
	PluginIndex string `toml:"plugin_index"`
}

// ContainerdConfig is where the plugin looks up a container's rootfs snapshot.
type ContainerdConfig struct {
	Address   string `toml:"address"`
	Namespace string `toml:"namespace"`
	// Snapshotter is the snapshotter CRI prepares container rootfs with. A container whose runtime handler uses
	// another snapshotter keeps its writable layer as it is.
	Snapshotter string `toml:"snapshotter"`
	// SnapshotterRoot is that snapshotter's root directory. The plugin mounts block layers only over directories
	// directly under <snapshotter_root>/snapshots.
	SnapshotterRoot string `toml:"snapshotter_root"`
}

// SizeConfig is the sizing policy. Sizes are Kubernetes quantities such as "256Mi", the notation of Pod
// ephemeral-storage declarations.
type SizeConfig struct {
	LimitOffset    string `toml:"limit_offset"`
	RequestPercent int64  `toml:"request_percent"`
	DefaultSize    string `toml:"default_size"`
}

func Default() Config {
	return Config{
		LogLevel: DefaultLogLevel,
		Root:     DefaultRoot,
		NRI: NRIConfig{
			SocketPath:  DefaultNRISocketPath,
			PluginIndex: DefaultPluginIndex,
		},
		Containerd: ContainerdConfig{
			Address:         DefaultContainerdAddress,
			Namespace:       DefaultNamespace,
			Snapshotter:     DefaultSnapshotter,
			SnapshotterRoot: DefaultSnapshotterRoot,
		},
	}
}

// Load decodes the file on top of Default, so the file only sets what it changes. Unknown keys fail, so a misspelt
// key does not silently keep its default.
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
	// An empty name matches no Pod, and one in included_namespaces would keep every Pod out.
	if slices.Contains(c.IncludedNamespaces, "") {
		return errors.New("included_namespaces must not hold an empty name")
	}
	if slices.Contains(c.ExcludedNamespaces, "") {
		return errors.New("excluded_namespaces must not hold an empty name")
	}
	if !filepath.IsAbs(c.NRI.SocketPath) {
		return fmt.Errorf("nri.socket_path %q must be an absolute path", c.NRI.SocketPath)
	}
	if !pluginIndex.MatchString(c.NRI.PluginIndex) {
		return fmt.Errorf("nri.plugin_index %q must be two digits", c.NRI.PluginIndex)
	}
	if c.Containerd.Address == "" {
		return errors.New("containerd.address must not be empty")
	}
	if c.Containerd.Namespace == "" {
		return errors.New("containerd.namespace must not be empty")
	}
	if c.Containerd.Snapshotter == "" {
		return errors.New("containerd.snapshotter must not be empty")
	}
	if !filepath.IsAbs(c.Containerd.SnapshotterRoot) {
		return fmt.Errorf("containerd.snapshotter_root %q must be an absolute path", c.Containerd.SnapshotterRoot)
	}
	// The snapshotter deletes what it does not know under its root.
	if within(c.Root, c.Containerd.SnapshotterRoot) {
		return fmt.Errorf("root %q must not be inside containerd.snapshotter_root %q", c.Root, c.Containerd.SnapshotterRoot)
	}
	_, err := c.Size.Policy()
	return err
}

// Policy validates the size table and converts it.
func (c SizeConfig) Policy() (sizing.Policy, error) {
	p := sizing.Policy{RequestPercent: sizing.DefaultRequestPercent}
	if c.LimitOffset != "" {
		v, err := parseSize("size.limit_offset", c.LimitOffset, 0)
		if err != nil {
			return sizing.Policy{}, err
		}
		p.LimitOffset = v
	}
	if c.RequestPercent != 0 {
		if c.RequestPercent < 100 {
			return sizing.Policy{}, fmt.Errorf("invalid size.request_percent %d: use at least 100, or 0 for the default of %d", c.RequestPercent, sizing.DefaultRequestPercent)
		}
		p.RequestPercent = c.RequestPercent
	}
	if c.DefaultSize != "" {
		v, err := parseSize("size.default_size", c.DefaultSize, sizing.MinSize)
		if err != nil {
			return sizing.Policy{}, err
		}
		p.DefaultSize = v
	}
	return p, nil
}

// parseSize reads a quantity of at least minimum bytes and at most MaxSize.
func parseSize(name, value string, minimum int64) (int64, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	if q.Sign() < 0 || q.Cmp(*maxQuantity) > 0 {
		return 0, fmt.Errorf("invalid %s %q: outside the range of 0 to 1Ei", name, value)
	}
	// Value rounds a fractional byte count up.
	v := q.Value()
	if v < minimum {
		return 0, fmt.Errorf("invalid %s %q: smaller than %d bytes", name, value, minimum)
	}
	return v, nil
}

// within reports whether path is dir or lies under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
