// Package buildinfo exposes the binary's VCS revision for observability
// annotations. It reads runtime/debug.ReadBuildInfo() once and caches the
// result so callers can call Version() freely in hot paths.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

var (
	once    sync.Once
	version string
)

// Version returns a short human-readable string identifying the binary's VCS
// revision. Preference order:
//
//  1. BuildInfo.Main.Version if not "(devel)" — set by `go install` / module proxy.
//  2. The `vcs.revision` debug setting, trimmed to 12 characters (short SHA).
//  3. "unknown" if neither is available (e.g., built outside a VCS checkout).
//
// The result is cached after the first call.
func Version() string {
	once.Do(func() { version = resolve() })
	return version
}

func resolve() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	// Prefer the module version stamp (set by go install / go build -ldflags).
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	// Fall back to the VCS revision from build settings.
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}

	return "unknown"
}
