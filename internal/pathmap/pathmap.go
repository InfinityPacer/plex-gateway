// Package pathmap maps Plex-visible absolute paths to gateway-local paths.
//
// Mappings are lexical only: they protect the path contract and do not resolve
// filesystem symlinks. Deployments that expose untrusted directories must
// enforce any additional filesystem policy at the container boundary.
package pathmap

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

var (
	// ErrInvalidPath reports a path that cannot be used by the mapper.
	ErrInvalidPath = errors.New("invalid path")
	// ErrNoMatch reports an otherwise valid path for which no mapping applies.
	ErrNoMatch = errors.New("no path mapping matched")
)

// Mapping connects a Plex metadata path with the corresponding path visible
// inside the gateway container. Both prefixes must be absolute POSIX paths.
type Mapping struct {
	PlexPrefix  string `json:"plex_prefix"`
	LocalPrefix string `json:"local_prefix"`
}

// Mapper applies validated mappings in longest Plex-prefix order.
type Mapper struct {
	mappings []Mapping
}

// New validates mappings and returns a mapper. Empty mappings are valid and
// make every Resolve call return ErrNoMatch, which supports fail-open startup
// configurations while preserving an explicit mapping contract.
func New(mappings []Mapping) (*Mapper, error) {
	validated := make([]Mapping, 0, len(mappings))
	seen := make(map[string]struct{}, len(mappings))
	for index, mapping := range mappings {
		plexPrefix, err := normalizePrefix(mapping.PlexPrefix)
		if err != nil {
			return nil, fmt.Errorf("mapping %d PlexPrefix: %w", index, err)
		}
		localPrefix, err := normalizePrefix(mapping.LocalPrefix)
		if err != nil {
			return nil, fmt.Errorf("mapping %d LocalPrefix: %w", index, err)
		}
		if _, exists := seen[plexPrefix]; exists {
			return nil, fmt.Errorf("mapping %d PlexPrefix %q: duplicate Plex prefix", index, plexPrefix)
		}
		seen[plexPrefix] = struct{}{}
		validated = append(validated, Mapping{
			PlexPrefix:  plexPrefix,
			LocalPrefix: localPrefix,
		})
	}

	// A longer prefix must win when mappings overlap, for example
	// /media/cloud/TV over /media/cloud.
	sort.SliceStable(validated, func(i, j int) bool {
		return len(validated[i].PlexPrefix) > len(validated[j].PlexPrefix)
	})
	return &Mapper{mappings: validated}, nil
}

// Resolve maps an absolute Plex path to a local path. The returned path is
// clean and remains within the selected LocalPrefix by component boundary.
func (m *Mapper) Resolve(plexPath string) (string, error) {
	cleanPlexPath, err := normalizeInput(plexPath)
	if err != nil {
		return "", err
	}
	for _, mapping := range m.mappings {
		if !hasComponentPrefix(cleanPlexPath, mapping.PlexPrefix) {
			continue
		}

		suffix := strings.TrimPrefix(cleanPlexPath, mapping.PlexPrefix)
		localPath := path.Clean(path.Join(mapping.LocalPrefix, suffix))
		if !hasComponentPrefix(localPath, mapping.LocalPrefix) {
			// This is defensive because both the suffix and prefix were
			// validated, but the invariant is security-sensitive.
			return "", fmt.Errorf("%w: mapped path escapes local prefix %q", ErrInvalidPath, mapping.LocalPrefix)
		}
		return localPath, nil
	}
	return "", ErrNoMatch
}

// Mappings returns a copy of the normalized mappings in match order.
func (m *Mapper) Mappings() []Mapping {
	return append([]Mapping(nil), m.mappings...)
}

func normalizePrefix(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("%w: path contains NUL", ErrInvalidPath)
	}
	if !path.IsAbs(raw) {
		return "", fmt.Errorf("%w: path must be absolute", ErrInvalidPath)
	}
	if containsParentComponent(raw) {
		return "", fmt.Errorf("%w: path contains ..", ErrInvalidPath)
	}
	return path.Clean(raw), nil
}

func normalizeInput(raw string) (string, error) {
	return normalizePrefix(raw)
}

func containsParentComponent(raw string) bool {
	for _, component := range strings.Split(raw, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func hasComponentPrefix(candidate, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return candidate == prefix || strings.HasPrefix(candidate, prefix+"/")
}
