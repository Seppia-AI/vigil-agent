package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is the tiny subset of SemVer 2.0.0 the updater needs to make a
// "is the upstream tag newer than ours?" decision. Full library would be
// ~600 LoC + a dep; we cover X.Y.Z[-pre] and stop there.
//
// Decisions:
//
//   - A leading "v" is tolerated and stripped. GitHub release tags use it
//     ("v0.1.3"); the build-time -ldflags often don't.
//   - Pre-release identifiers are compared as a single string with the
//     standard rule: a version WITH a pre-release is LOWER than the same
//     version WITHOUT one ("1.0.0-rc1" < "1.0.0"). That's enough for the
//     update-check to do the right thing for our release cadence — we
//     don't ship channels (alpha/beta) and we never want a pre-release
//     tag to alert as "newer than" the matching stable.
//   - Build metadata ("+sha.abc") is stripped before comparison, matching
//     the SemVer spec (build metadata MUST NOT affect ordering).
//
// Anything we can't parse is reported as an error; the updater treats
// parse errors like network errors — silently skip this round and try
// again tomorrow. Logging a "your version string is malformed" warning
// once per day would be louder than the actual problem warrants.
type semver struct {
	Major, Minor, Patch int
	Pre                 string // "" means stable / no pre-release tag
}

// parseSemver parses "v1.2.3", "1.2.3", "v1.2.3-rc1", "1.2.3+build.7".
// Returns an error for anything else (missing parts, non-numeric segments).
func parseSemver(s string) (semver, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return semver{}, fmt.Errorf("empty version string")
	}
	raw = strings.TrimPrefix(raw, "v")

	if i := strings.IndexByte(raw, '+'); i >= 0 {
		raw = raw[:i]
	}

	var pre string
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		pre = raw[i+1:]
		raw = raw[:i]
		if pre == "" {
			return semver{}, fmt.Errorf("empty pre-release tag in %q", s)
		}
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("expected MAJOR.MINOR.PATCH, got %q", s)
	}

	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("non-numeric segment %q in %q", p, s)
		}
		nums[i] = n
	}

	return semver{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// isNewer reports whether `latest` strictly compares greater than `current`
// under the rules described on the semver type. Returns false on equality.
//
// Pre-release rule recap:
//
//	1.0.0-rc1   <   1.0.0
//	1.0.0       <   1.0.1
//	1.0.0-rc1   <   1.0.0-rc2   (lexicographic — good enough for "rc1" vs "rc2",
//	                              good enough for "alpha" vs "beta"; it'll mis-
//	                              order "rc.10" vs "rc.2" but we don't ship
//	                              double-digit pre-releases)
//
// That last edge case is documented rather than fixed because the update
// check's failure mode under it is "operator sees an extra log line" — not
// "operator misses a critical security update".
func isNewer(current, latest semver) bool {
	if latest.Major != current.Major {
		return latest.Major > current.Major
	}
	if latest.Minor != current.Minor {
		return latest.Minor > current.Minor
	}
	if latest.Patch != current.Patch {
		return latest.Patch > current.Patch
	}

	switch {
	case current.Pre == "" && latest.Pre == "":
		return false
	case current.Pre == "" && latest.Pre != "":
		return false
	case current.Pre != "" && latest.Pre == "":
		return true
	default:
		return latest.Pre > current.Pre
	}
}
