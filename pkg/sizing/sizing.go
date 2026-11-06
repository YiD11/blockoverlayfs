package sizing

import (
	"fmt"
	"math"
)

// MinSize is the smallest writable layer. A block image below 16Mi either fails in mkfs.ext4 or leaves the
// container no room to start, so a computed size below it falls back to the default size, and a default size
// below it is rejected at startup. Like the containerd branch it is ported from, it is not calibrated yet.
const MinSize = int64(16) << 20

// DefaultRequestPercent scales a request so a container can write about what it requested once ext4
// metadata is paid for. It is a heuristic, not a guarantee of usable capacity.
const DefaultRequestPercent = 105

// Policy turns a container's ephemeral-storage declaration into a writable layer size. The zero value is
// disabled: every container keeps the directory upper of the overlay snapshotter.
type Policy struct {
	Enabled        bool
	LimitOffset    int64
	RequestPercent int64
	// DefaultSize is used when a container declares nothing or its computed size is below MinSize.
	// Zero means those containers keep a directory upper.
	DefaultSize int64
}

// Resources is what a container declares. Nil, zero and negative all mean undeclared, as kubelet treats an
// unset ephemeral-storage quantity.
type Resources struct {
	Request *int64
	Limit   *int64
}

// Result is a sizing decision. Size zero means the container keeps a directory upper.
type Result struct {
	Size   int64
	Reason string
}

// Size picks the block image size. A positive limit always wins, even when a request is declared too:
// limit - limit_offset. Otherwise a positive request gives ceil(request * request_percent / 100). A result
// below MinSize, or no declaration at all, falls back to DefaultSize.
func (p Policy) Size(res Resources) (Result, error) {
	if !p.Enabled {
		return Result{Reason: "block writable layers are off"}, nil
	}
	if limit, ok := declared(res.Limit); ok {
		size := limit - p.LimitOffset
		reason := fmt.Sprintf("limit %d minus limit_offset %d", limit, p.LimitOffset)
		if size >= MinSize {
			return Result{Size: size, Reason: reason}, nil
		}
		return p.fallback(reason + " is below the minimum"), nil
	}
	if request, ok := declared(res.Request); ok {
		percent := p.requestPercent()
		size, err := scaleUp(request, percent)
		if err != nil {
			return Result{}, err
		}
		reason := fmt.Sprintf("request %d scaled by %d%%", request, percent)
		if size >= MinSize {
			return Result{Size: size, Reason: reason}, nil
		}
		return p.fallback(reason + " is below the minimum"), nil
	}
	return p.fallback("no ephemeral-storage limit or request"), nil
}

func (p Policy) fallback(reason string) Result {
	if p.DefaultSize > 0 {
		return Result{Size: p.DefaultSize, Reason: reason + ", using default_size"}
	}
	return Result{Reason: reason + ", and default_size is unset"}
}

func (p Policy) requestPercent() int64 {
	if p.RequestPercent > 0 {
		return p.RequestPercent
	}
	return DefaultRequestPercent
}

func declared(v *int64) (int64, bool) {
	if v == nil || *v <= 0 {
		return 0, false
	}
	return *v, true
}

// scaleUp multiplies by percent/100 and rounds up. The multiplication is checked before it can overflow.
func scaleUp(bytes, percent int64) (int64, error) {
	if percent <= 0 {
		return 0, fmt.Errorf("invalid request percentage %d", percent)
	}
	if bytes > (math.MaxInt64-99)/percent {
		return 0, fmt.Errorf("request of %d bytes is too large to scale by %d%%", bytes, percent)
	}
	return (bytes*percent + 99) / 100, nil
}
