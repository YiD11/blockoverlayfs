// Package sizing turns a container's ephemeral-storage declaration into the size of its block writable layer.
package sizing

import (
	"fmt"
	"math"
)

// MinSize is the smallest block writable layer. mkfs.ext4 fails on much smaller images, and a container could not
// start in one much smaller either, so a computed size below it falls back to the default size.
const MinSize = int64(16) << 20

// DefaultRequestPercent scales a request so that a container can write about what it requested once ext4 has taken
// its metadata. It is a heuristic, not a guarantee of usable capacity.
const DefaultRequestPercent = 105

// Policy sizes block writable layers. The zero value sizes a container by its limit or request and leaves a
// container that declares neither on a plain overlay directory.
type Policy struct {
	// LimitOffset is subtracted from a limit, leaving room under it for the container's logs and emptyDir volumes,
	// which kubelet counts against the same limit.
	LimitOffset int64
	// RequestPercent scales a request; zero means DefaultRequestPercent.
	RequestPercent int64
	// DefaultSize is used when a container declares nothing or its computed size is below MinSize. Zero leaves
	// such a container on a plain overlay directory.
	DefaultSize int64
}

// Resources is what a container declares. Nil, zero and negative all mean undeclared, as kubelet treats an unset
// ephemeral-storage quantity.
type Resources struct {
	Request *int64
	Limit   *int64
}

// Result is a sizing decision. Size zero leaves the container on a plain overlay directory.
type Result struct {
	Size   int64
	Reason string
}

// Size picks the size of a container's block writable layer. A positive limit wins even when a request is declared
// too: limit - LimitOffset. Otherwise a positive request gives ceil(request * RequestPercent / 100). A result below
// MinSize, or no declaration at all, falls back to DefaultSize.
func (p Policy) Size(res Resources) (Result, error) {
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
