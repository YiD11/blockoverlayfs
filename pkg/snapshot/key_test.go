package snapshot

import "testing"

func TestParseKey(t *testing.T) {
	tests := []struct {
		key        string
		want       Ref
		wantOK     bool
		wantUnpack bool
	}{
		{"k8s.io/43/0123abcd", Ref{"k8s.io", "43", "0123abcd"}, true, false},
		{"k8s.io/12/extract-123-456 sha256:abc", Ref{"k8s.io", "12", "extract-123-456 sha256:abc"}, true, true},
		{"default/5/name/with/slashes", Ref{"default", "5", "name/with/slashes"}, true, false},
		{"no-namespace", Ref{}, false, false},
		{"k8s.io//abc", Ref{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, ok := ParseKey(tt.key)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ParseKey() = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
			if got.IsUnpack() != tt.wantUnpack {
				t.Fatalf("IsUnpack() = %v, want %v", got.IsUnpack(), tt.wantUnpack)
			}
		})
	}
}
