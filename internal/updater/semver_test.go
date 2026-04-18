package updater

import "testing"

func TestParseSemver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in           string
		wantOK       bool
		wantMajor    int
		wantMinor    int
		wantPatch    int
		wantPre      string
	}{
		{"v0.1.0", true, 0, 1, 0, ""},
		{"0.1.0", true, 0, 1, 0, ""},
		{"  v1.2.3  ", true, 1, 2, 3, ""},
		{"v1.2.3-rc1", true, 1, 2, 3, "rc1"},
		{"v1.2.3+build.7", true, 1, 2, 3, ""},
		{"v1.2.3-rc1+build.7", true, 1, 2, 3, "rc1"},
		{"v10.20.30", true, 10, 20, 30, ""},

		// Invalid inputs — every one of these should return an error so
		// the updater silently disables itself rather than make a
		// nonsense comparison.
		{"", false, 0, 0, 0, ""},
		{"v1.2", false, 0, 0, 0, ""},
		{"v1.2.3.4", false, 0, 0, 0, ""},
		{"v1.2.x", false, 0, 0, 0, ""},
		{"vX.Y.Z", false, 0, 0, 0, ""},
		{"dev", false, 0, 0, 0, ""},
		{"v1.2.3-", false, 0, 0, 0, ""},
		{"v-1.2.3", false, 0, 0, 0, ""},
	}

	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseSemver(c.in)
			if c.wantOK && err != nil {
				t.Fatalf("parseSemver(%q): unexpected error: %v", c.in, err)
			}
			if !c.wantOK {
				if err == nil {
					t.Fatalf("parseSemver(%q): want error, got %+v", c.in, got)
				}
				return
			}
			if got.Major != c.wantMajor || got.Minor != c.wantMinor ||
				got.Patch != c.wantPatch || got.Pre != c.wantPre {
				t.Fatalf("parseSemver(%q): got %+v want {%d %d %d %q}",
					c.in, got, c.wantMajor, c.wantMinor, c.wantPatch, c.wantPre)
			}
		})
	}
}

func TestIsNewer(t *testing.T) {
	t.Parallel()

	mk := func(s string) semver {
		v, err := parseSemver(s)
		if err != nil {
			t.Fatalf("setup parseSemver(%q): %v", s, err)
		}
		return v
	}

	cases := []struct {
		current, latest string
		want            bool
		why             string
	}{
		{"v0.1.0", "v0.1.0", false, "equal"},
		{"v0.1.0", "v0.1.1", true, "patch bump"},
		{"v0.1.5", "v0.2.0", true, "minor bump"},
		{"v0.9.9", "v1.0.0", true, "major bump"},
		{"v0.1.0", "v0.0.9", false, "older minor"},
		{"v1.0.0", "v0.9.9", false, "older major"},

		// Pre-release rules.
		{"v1.0.0-rc1", "v1.0.0", true, "stable beats matching pre-release"},
		{"v1.0.0", "v1.0.0-rc1", false, "pre-release does NOT beat stable"},
		{"v1.0.0-rc1", "v1.0.0-rc2", true, "later pre-release wins lexicographically"},
		{"v1.0.0-rc2", "v1.0.0-rc1", false, "earlier pre-release loses"},
		{"v1.0.0-rc1", "v1.0.0-rc1", false, "identical pre-release"},

		// Build metadata is stripped (MUST NOT affect ordering per SemVer).
		{"v1.0.0+a", "v1.0.0+b", false, "build metadata ignored"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.why, func(t *testing.T) {
			t.Parallel()
			if got := isNewer(mk(c.current), mk(c.latest)); got != c.want {
				t.Fatalf("isNewer(%s, %s) = %v, want %v (%s)",
					c.current, c.latest, got, c.want, c.why)
			}
		})
	}
}
