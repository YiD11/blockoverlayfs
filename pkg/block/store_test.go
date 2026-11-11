package block

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: strings.Repeat("0123456789abcdef", 4), want: true},
		{id: "ctr-1_a.b", want: true},
		{id: strings.Repeat("a", 128), want: true},
		{id: strings.Repeat("a", 129)},
		{id: ""},
		{id: "."},
		{id: ".."},
		{id: ".hidden"},
		{id: "a/b"},
		{id: "a b"},
	} {
		if got := ValidID(tc.id); got != tc.want {
			t.Errorf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestIDsListsOnlyImages(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"abc.img", ".hidden.img", "bad id.img", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(s.Root(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(s.Root(), "dir.img"), 0o700); err != nil {
		t.Fatal(err)
	}
	ids, err := s.IDs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"abc"}) {
		t.Errorf("IDs = %v, want [abc]", ids)
	}
}
