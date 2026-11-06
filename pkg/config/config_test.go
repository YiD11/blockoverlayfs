package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestLoadOverlaysDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[writable_layer]\nsource = \"ephemeral-storage\"\nrequest_percent = 150\n[containerd]\nlookup_timeout = \"5s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WritableLayer.RequestPercent != 150 || cfg.Containerd.LookupTimeout != Duration(5*time.Second) {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.Address != DefaultAddress || cfg.Root != DefaultRoot || cfg.Containerd.Address != DefaultContainerdAddress {
		t.Fatalf("defaults not kept: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultIsValidAndOff(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := cfg.WritableLayer.Policy()
	if err != nil || p.Enabled {
		t.Fatalf("Policy() = %+v, %v; want disabled", p, err)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	if _, err := Load(writeConfig(t, "no_such_key = 1\n")); err == nil {
		t.Fatal("Load() succeeded, want error for unknown key")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	if _, err := Load(writeConfig(t, "[containerd]\nlookup_timeout = \"three seconds\"\n")); err == nil {
		t.Fatal("Load() succeeded, want error for a bad duration")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load() error = %v, want fs.ErrNotExist", err)
	}
}

func TestExampleConfigStaysValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := cfg.WritableLayer.Policy()
	if err != nil {
		t.Fatal(err)
	}
	want := sizing.Policy{Enabled: true, LimitOffset: 256 << 20, RequestPercent: 105, DefaultSize: 20 << 30}
	if p != want {
		t.Fatalf("policy from deploy/config.toml = %+v, want %+v", p, want)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"relative root", func(c *Config) { c.Root = "var/lib/x" }, "absolute path"},
		{"empty address", func(c *Config) { c.Address = "" }, "address"},
		{"zero containerd timeout", func(c *Config) { c.Containerd.LookupTimeout = 0 }, "containerd.lookup_timeout"},
		{"negative containerd timeout", func(c *Config) { c.Containerd.LookupTimeout = -1 }, "containerd.lookup_timeout"},
		{"bad writable layer", func(c *Config) { c.WritableLayer.Source = "bogus" }, "writable_layer.source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// The cases follow TestValidateWritableLayer on the containerd branch, with Kubernetes quantities.
func TestWritableLayerPolicy(t *testing.T) {
	es := SourceEphemeralStorage
	tests := []struct {
		name    string
		cfg     WritableLayerConfig
		want    sizing.Policy
		wantErr string
	}{
		{name: "empty is off", want: sizing.Policy{}},
		{name: "explicit off", cfg: WritableLayerConfig{Source: SourceOff}, want: sizing.Policy{}},
		{
			name: "ephemeral-storage accepts all settings",
			cfg:  WritableLayerConfig{Source: es, LimitOffset: "256Mi", RequestPercent: 100, DefaultSize: "20Gi"},
			want: sizing.Policy{Enabled: true, LimitOffset: 256 << 20, RequestPercent: 100, DefaultSize: 20 << 30},
		},
		{name: "request percent zero uses the default", cfg: WritableLayerConfig{Source: es}, want: sizing.Policy{Enabled: true, RequestPercent: sizing.DefaultRequestPercent}},
		{name: "default size exactly at the smallest writable layer", cfg: WritableLayerConfig{Source: es, DefaultSize: "16Mi"}, want: sizing.Policy{Enabled: true, RequestPercent: 105, DefaultSize: sizing.MinSize}},
		{name: "default size at the largest accepted size", cfg: WritableLayerConfig{Source: es, DefaultSize: "1Ei"}, want: sizing.Policy{Enabled: true, RequestPercent: 105, DefaultSize: MaxSize}},
		{name: "decimal quantities are accepted", cfg: WritableLayerConfig{Source: es, LimitOffset: "100M"}, want: sizing.Policy{Enabled: true, LimitOffset: 100_000_000, RequestPercent: 105}},
		{name: "unknown source", cfg: WritableLayerConfig{Source: "bogus"}, wantErr: "not one of"},
		{name: "off with limit offset", cfg: WritableLayerConfig{LimitOffset: "256Mi"}, wantErr: "would never be used"},
		{name: "off with request percent", cfg: WritableLayerConfig{RequestPercent: 105}, wantErr: "would never be used"},
		{name: "off with default size", cfg: WritableLayerConfig{DefaultSize: "20Gi"}, wantErr: "would never be used"},
		{name: "invalid limit offset", cfg: WritableLayerConfig{Source: es, LimitOffset: "notasize"}, wantErr: "invalid writable_layer.limit_offset"},
		{name: "go-units spelling is rejected", cfg: WritableLayerConfig{Source: es, LimitOffset: "256MiB"}, wantErr: "invalid writable_layer.limit_offset"},
		{name: "negative limit offset", cfg: WritableLayerConfig{Source: es, LimitOffset: "-1Gi"}, wantErr: "outside the range"},
		{name: "zero limit offset", cfg: WritableLayerConfig{Source: es, LimitOffset: "0"}, wantErr: "outside the range"},
		{name: "request percent below 100", cfg: WritableLayerConfig{Source: es, RequestPercent: 99}, wantErr: "smaller than 100"},
		{name: "negative request percent", cfg: WritableLayerConfig{Source: es, RequestPercent: -5}, wantErr: "smaller than 100"},
		{name: "default size below the smallest writable layer", cfg: WritableLayerConfig{Source: es, DefaultSize: "16383Ki"}, wantErr: "smaller than the smallest writable layer"},
		{name: "invalid default size", cfg: WritableLayerConfig{Source: es, DefaultSize: "notasize"}, wantErr: "invalid writable_layer.default_size"},
		{name: "default size over 1Ei", cfg: WritableLayerConfig{Source: es, DefaultSize: "2048Pi"}, wantErr: "outside the range"},
		{name: "default size far over the int64 range", cfg: WritableLayerConfig{Source: es, DefaultSize: "100Ei"}, wantErr: "outside the range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.Policy()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Policy() = %+v, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("Policy() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
