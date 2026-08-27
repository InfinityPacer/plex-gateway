package pathmap

import (
	"errors"
	"testing"
)

func TestResolveUsesCleanLongestBoundaryMatch(t *testing.T) {
	mapper, err := New([]Mapping{
		{PlexPrefix: "/media/cloud", LocalPrefix: "/mnt/one"},
		{PlexPrefix: "/media/cloud/TV", LocalPrefix: "/mnt/tv"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "longest child mapping", in: "/media/cloud/TV//Show/./Episode.strm", want: "/mnt/tv/Show/Episode.strm"},
		{name: "parent mapping", in: "/media/cloud/Movie.strm", want: "/mnt/one/Movie.strm"},
		{name: "exact prefix", in: "/media/cloud", want: "/mnt/one"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mapper.Resolve(test.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Resolve(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestResolveHonorsComponentBoundary(t *testing.T) {
	mapper, err := New([]Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: "/mnt/strm"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.Resolve("/media/cloud2/file.strm"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Resolve() error = %v, want ErrNoMatch", err)
	}
}

func TestNewRejectsUnsafeMappings(t *testing.T) {
	tests := []struct {
		name string
		maps []Mapping
	}{
		{name: "relative plex prefix", maps: []Mapping{{PlexPrefix: "media/cloud", LocalPrefix: "/mnt/strm"}}},
		{name: "relative local prefix", maps: []Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: "mnt/strm"}}},
		{name: "parent plex prefix", maps: []Mapping{{PlexPrefix: "/media/../secret", LocalPrefix: "/mnt/strm"}}},
		{name: "parent local prefix", maps: []Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: "/mnt/../secret"}}},
		{name: "NUL", maps: []Mapping{{PlexPrefix: "/media/cloud\x00", LocalPrefix: "/mnt/strm"}}},
		{name: "duplicate plex prefix", maps: []Mapping{
			{PlexPrefix: "/media/cloud", LocalPrefix: "/mnt/one"},
			{PlexPrefix: "/media/cloud/", LocalPrefix: "/mnt/two"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.maps); err == nil {
				t.Fatal("New() accepted unsafe mapping")
			}
		})
	}
}

func TestResolveRejectsUnsafeInput(t *testing.T) {
	mapper, err := New([]Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: "/mnt/strm"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"relative/file.strm", "/media/cloud/../secret.strm", "/media/cloud/file\x00.strm"} {
		if _, err := mapper.Resolve(input); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) error = %v, want ErrInvalidPath", input, err)
		}
	}
}

func TestResolveCannotEscapeLocalPrefix(t *testing.T) {
	mapper, err := New([]Mapping{{PlexPrefix: "/", LocalPrefix: "/mnt/strm"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mapper.Resolve("/safe/file.strm")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/mnt/strm/safe/file.strm" {
		t.Fatalf("Resolve() = %q", got)
	}
}
