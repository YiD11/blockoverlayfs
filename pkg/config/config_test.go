package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadKeepsTheDefaultsAFileDoesNotSet(t *testing.T) {
	cfg, err := Load(writeConfig(t, "log_level = \"debug\"\n[size]\ndefault_size = \"20Gi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.LogLevel = "debug"
	want.Size.DefaultSize = "20Gi"
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}

func TestTheDeployedExampleIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// Outside the size table, the example spells out the defaults.
	want := Default()
	want.Size = cfg.Size
	if cfg != want {
		t.Errorf("the example sets %+v, want the defaults %+v", cfg, want)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	if _, err := Load(writeConfig(t, "[size]\nlimits = \"1Gi\"\n")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load error = %v, want fs.ErrNotExist", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{name: "relative root", modify: func(c *Config) { c.Root = "var/lib/blockoverlayfs" }, want: "root"},
		{name: "relative NRI socket", modify: func(c *Config) { c.NRI.SocketPath = "nri.sock" }, want: "nri.socket_path"},
		{name: "one-digit plugin index", modify: func(c *Config) { c.NRI.PluginIndex = "1" }, want: "nri.plugin_index"},
		{name: "no containerd address", modify: func(c *Config) { c.Containerd.Address = "" }, want: "containerd.address"},
		{name: "no namespace", modify: func(c *Config) { c.Containerd.Namespace = "" }, want: "containerd.namespace"},
		{name: "no snapshotter", modify: func(c *Config) { c.Containerd.Snapshotter = "" }, want: "containerd.snapshotter"},
		{name: "relative snapshotter root", modify: func(c *Config) { c.Containerd.SnapshotterRoot = "overlayfs" }, want: "containerd.snapshotter_root"},
		{
			name:   "root inside the snapshotter root",
			modify: func(c *Config) { c.Root = filepath.Join(c.Containerd.SnapshotterRoot, "snapshots", "blocks") },
			want:   "must not be inside",
		},
		{name: "root equal to the snapshotter root", modify: func(c *Config) { c.Root = c.Containerd.SnapshotterRoot }, want: "must not be inside"},
		{name: "invalid size table", modify: func(c *Config) { c.Size.RequestPercent = 50 }, want: "size.request_percent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.modify(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsARootNextToTheSnapshotterRoot(t *testing.T) {
	cfg := Default()
	cfg.Root = cfg.Containerd.SnapshotterRoot + "-blocks"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSizePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    SizeConfig
		want    sizing.Policy
		wantErr string
	}{
		{name: "defaults", want: sizing.Policy{RequestPercent: sizing.DefaultRequestPercent}},
		{
			name: "all set",
			size: SizeConfig{LimitOffset: "256Mi", RequestPercent: 110, DefaultSize: "20Gi"},
			want: sizing.Policy{LimitOffset: 256 << 20, RequestPercent: 110, DefaultSize: 20 << 30},
		},
		{name: "a zero limit offset", size: SizeConfig{LimitOffset: "0"}, want: sizing.Policy{RequestPercent: sizing.DefaultRequestPercent}},
		{name: "a fractional byte count rounds up", size: SizeConfig{LimitOffset: "1.5"}, want: sizing.Policy{LimitOffset: 2, RequestPercent: sizing.DefaultRequestPercent}},
		{name: "a negative limit offset", size: SizeConfig{LimitOffset: "-1Mi"}, wantErr: "size.limit_offset"},
		{name: "not a quantity", size: SizeConfig{LimitOffset: "lots"}, wantErr: "size.limit_offset"},
		{name: "a request percentage below 100", size: SizeConfig{RequestPercent: 99}, wantErr: "size.request_percent"},
		{name: "a default size below the minimum", size: SizeConfig{DefaultSize: "8Mi"}, wantErr: "size.default_size"},
		{name: "a default size above the maximum", size: SizeConfig{DefaultSize: "2Ei"}, wantErr: "size.default_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.size.Policy()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Policy error = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}
