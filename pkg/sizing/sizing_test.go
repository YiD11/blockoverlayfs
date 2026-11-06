package sizing

import "testing"

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

func ptr(v int64) *int64 { return &v }

// The cases follow TestResolveWritableLayerSize on the containerd branch this daemon replaces.
func TestPolicySize(t *testing.T) {
	const (
		defaultSize = 512 * mi
		twoGiAt105  = int64(2254857831) // 2Gi at 105%, rounded up
	)
	on := func(limitOffset, defaultSize int64) Policy {
		return Policy{Enabled: true, LimitOffset: limitOffset, RequestPercent: 105, DefaultSize: defaultSize}
	}
	tests := []struct {
		name    string
		policy  Policy
		res     Resources
		want    int64
		wantErr bool
	}{
		{"off creates no block layer even with a limit", Policy{}, Resources{Limit: ptr(10 * gi)}, 0, false},
		{"limit subtracts the offset", on(256*mi, 0), Resources{Limit: ptr(10 * gi)}, 10*gi - 256*mi, false},
		{"limit wins over request", on(256*mi, 0), Resources{Limit: ptr(10 * gi), Request: ptr(2 * gi)}, 10*gi - 256*mi, false},
		{"limit without an offset uses the whole limit", on(0, 0), Resources{Limit: ptr(10 * gi)}, 10 * gi, false},
		{"limit equal to request still uses the limit minus offset", on(256*mi, defaultSize), Resources{Limit: ptr(gi), Request: ptr(gi)}, gi - 256*mi, false},
		{"limit below the minimum falls back to default size", on(17*mi, defaultSize), Resources{Limit: ptr(32 * mi)}, defaultSize, false},
		{"limit below the minimum without a default keeps a directory", on(17*mi, 0), Resources{Limit: ptr(32 * mi)}, 0, false},
		{"limit exactly at the minimum is kept", on(16*mi, 0), Resources{Limit: ptr(32 * mi)}, MinSize, false},
		{"limit smaller than the offset falls back to default size", on(gi, defaultSize), Resources{Limit: ptr(256 * mi)}, defaultSize, false},
		{"request is scaled up and rounded up", on(0, 0), Resources{Request: ptr(2 * gi)}, twoGiAt105, false},
		{"request rounding is exact", on(0, 0), Resources{Request: ptr(1001 * mi)}, 1102105805, false},
		{"zero request percent uses the default", Policy{Enabled: true}, Resources{Request: ptr(2 * gi)}, twoGiAt105, false},
		{"request below the minimum falls back to default size", on(0, defaultSize), Resources{Request: ptr(mi)}, defaultSize, false},
		{"request below the minimum without a default keeps a directory", on(0, 0), Resources{Request: ptr(mi)}, 0, false},
		{"no declaration uses default size", on(0, defaultSize), Resources{}, defaultSize, false},
		{"no declaration without default size keeps a directory", on(0, 0), Resources{}, 0, false},
		{"a zero limit counts as undeclared and the request is used", on(0, 0), Resources{Limit: ptr(0), Request: ptr(2 * gi)}, twoGiAt105, false},
		{"a zero request counts as undeclared and default size is used", on(0, defaultSize), Resources{Request: ptr(0)}, defaultSize, false},
		{"a negative limit counts as undeclared", on(0, defaultSize), Resources{Limit: ptr(-1)}, defaultSize, false},
		{"request scaling overflow is rejected", on(0, 0), Resources{Request: ptr(7 << 60)}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.policy.Size(tt.res)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Size() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Size != tt.want {
				t.Fatalf("Size() = %d (%s), want %d", got.Size, got.Reason, tt.want)
			}
			if got.Reason == "" {
				t.Fatal("Size() gave no reason")
			}
		})
	}
}
