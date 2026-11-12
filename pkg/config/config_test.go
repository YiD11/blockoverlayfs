package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
	cfg, err := Load(writeConfig(t, "log_level = \"debug\"\nincluded_namespaces = [\"a\", \"b\"]\nexcluded_namespaces = [\"c\"]\n[size]\ndefault_size = \"20Gi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.LogLevel = "debug"
	want.IncludedNamespaces = []string{"a", "b"}
	want.ExcludedNamespaces = []string{"c"}
	want.Size.DefaultSize = "20Gi"
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Error(err)
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
	}{
		{name: "relative root", modify: func(c *Config) { c.Root = "var/lib/blockoverlayfs" }},
		{name: "an empty included namespace", modify: func(c *Config) { c.IncludedNamespaces = []string{"a", ""} }},
		{name: "an empty excluded namespace", modify: func(c *Config) { c.ExcludedNamespaces = []string{""} }},
		{name: "relative NRI socket", modify: func(c *Config) { c.NRI.SocketPath = "nri.sock" }},
		{name: "one-digit plugin index", modify: func(c *Config) { c.NRI.PluginIndex = "1" }},
		{name: "no containerd address", modify: func(c *Config) { c.Containerd.Address = "" }},
		{name: "no namespace", modify: func(c *Config) { c.Containerd.Namespace = "" }},
		{name: "no snapshotter", modify: func(c *Config) { c.Containerd.Snapshotter = "" }},
		{name: "relative snapshotter root", modify: func(c *Config) { c.Containerd.SnapshotterRoot = "overlayfs" }},
		{
			name:   "root inside the snapshotter root",
			modify: func(c *Config) { c.Root = filepath.Join(c.Containerd.SnapshotterRoot, "snapshots", "blocks") },
		},
		{name: "root equal to the snapshotter root", modify: func(c *Config) { c.Root = c.Containerd.SnapshotterRoot }},
		{name: "invalid size table", modify: func(c *Config) { c.Size.RequestPercent = 50 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.modify(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected an error")
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
		wantErr bool
	}{
		{name: "defaults", want: sizing.Policy{RequestPercent: sizing.DefaultRequestPercent}},
		{
			name: "all set",
			size: SizeConfig{LimitOffset: "256Mi", RequestPercent: 110, DefaultSize: "20Gi"},
			want: sizing.Policy{LimitOffset: 256 << 20, RequestPercent: 110, DefaultSize: 20 << 30},
		},
		{name: "a zero limit offset", size: SizeConfig{LimitOffset: "0"}, want: sizing.Policy{RequestPercent: sizing.DefaultRequestPercent}},
		{name: "a fractional byte count rounds up", size: SizeConfig{LimitOffset: "1.5"}, want: sizing.Policy{LimitOffset: 2, RequestPercent: sizing.DefaultRequestPercent}},
		{name: "a negative limit offset", size: SizeConfig{LimitOffset: "-1Mi"}, wantErr: true},
		{name: "not a quantity", size: SizeConfig{LimitOffset: "lots"}, wantErr: true},
		{name: "a request percentage below 100", size: SizeConfig{RequestPercent: 99}, wantErr: true},
		{name: "a default size below the minimum", size: SizeConfig{DefaultSize: "8Mi"}, wantErr: true},
		{name: "a default size above the maximum", size: SizeConfig{DefaultSize: "2Ei"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.size.Policy()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Policy error = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}
