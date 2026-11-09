package sizing

import (
	"math"
	"strings"
	"testing"
)

func ptr(v int64) *int64 { return &v }

func TestSize(t *testing.T) {
	const gi = int64(1) << 30
	policy := Policy{LimitOffset: 256 << 20, RequestPercent: 110, DefaultSize: 20 * gi}

	for _, tc := range []struct {
		name   string
		policy Policy
		res    Resources
		size   int64
		reason string
	}{
		{
			name:   "a limit wins over a request",
			policy: policy,
			res:    Resources{Request: ptr(5 * gi), Limit: ptr(10 * gi)},
			size:   10*gi - 256<<20,
			reason: "limit",
		},
		{
			name:   "a request is scaled and rounded up",
			policy: policy,
			res:    Resources{Request: ptr(100<<20 + 1)},
			size:   (int64(100<<20+1)*110 + 99) / 100,
			reason: "request",
		},
		{
			name:   "a zero request percentage means the default",
			policy: Policy{},
			res:    Resources{Request: ptr(100 << 20)},
			size:   100 << 20 * DefaultRequestPercent / 100,
			reason: "scaled by 105%",
		},
		{
			name:   "nothing declared uses the default size",
			policy: policy,
			size:   20 * gi,
			reason: "using default_size",
		},
		{
			name:   "zero and negative quantities count as undeclared",
			policy: policy,
			res:    Resources{Request: ptr(0), Limit: ptr(-1)},
			size:   20 * gi,
			reason: "no ephemeral-storage limit or request",
		},
		{
			name:   "a limit that leaves less than the minimum uses the default size",
			policy: policy,
			res:    Resources{Limit: ptr(256<<20 + MinSize - 1)},
			size:   20 * gi,
			reason: "below the minimum",
		},
		{
			name:   "without a default size an undeclared container keeps a directory",
			policy: Policy{},
			reason: "default_size is unset",
		},
		{
			name:   "without a default size a tiny request keeps a directory",
			policy: Policy{},
			res:    Resources{Request: ptr(1)},
			reason: "below the minimum",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.policy.Size(tc.res)
			if err != nil {
				t.Fatal(err)
			}
			if got.Size != tc.size {
				t.Errorf("size = %d, want %d", got.Size, tc.size)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestSizeRejectsARequestThatWouldOverflow(t *testing.T) {
	if _, err := (Policy{RequestPercent: 200}).Size(Resources{Request: ptr(math.MaxInt64 / 2)}); err == nil {
		t.Fatal("expected an error")
	}
}
