package imageupdate

import "testing"

func TestLatestSemverTag(t *testing.T) {
	tests := []struct {
		name   string
		tags   []string
		family string
		want   string
		wantOK bool
	}{
		{
			name:   "pure_semver_picks_highest",
			tags:   []string{"44.11.1", "44.11.3", "44.10.9", "latest"},
			family: "",
			want:   "44.11.3",
			wantOK: true,
		},
		{
			name:   "slim_family_isolated",
			tags:   []string{"44.11.3", "44.11.3-slim", "44.12.0-slim", "44.12.0"},
			family: "-slim",
			want:   "44.12.0-slim",
			wantOK: true,
		},
		{
			name:   "pure_ignores_family_variants",
			tags:   []string{"44.11.3", "44.11.3-slim", "44.12.0-slim"},
			family: "",
			want:   "44.11.3",
			wantOK: true,
		},
		{
			name:   "family_not_found_returns_false",
			tags:   []string{"44.11.3", "44.11.3-slim"},
			family: "-full",
			wantOK: false,
		},
		{
			name:   "ignores_non_semver_noise",
			tags:   []string{"latest", "stable", "44", "44.11", "nightly-2026-01-01", "44.11.3"},
			family: "",
			want:   "44.11.3",
			wantOK: true,
		},
		{
			name:   "empty_input",
			tags:   nil,
			family: "",
			wantOK: false,
		},
		{
			name:   "major_dominates",
			tags:   []string{"44.99.99", "45.0.0"},
			family: "",
			want:   "45.0.0",
			wantOK: true,
		},
		{
			name:   "patch_dominates_when_major_minor_equal",
			tags:   []string{"1.2.3", "1.2.10"},
			family: "",
			want:   "1.2.10",
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := LatestSemverTag(tt.tags, tt.family)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSemverPrefix(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantMajor  int
		wantMinor  int
		wantPatch  int
		wantSuffix string
		wantOK     bool
	}{
		{name: "pure", in: "1.2.3", wantMajor: 1, wantMinor: 2, wantPatch: 3, wantOK: true},
		{name: "with_suffix", in: "44.11.3-slim", wantMajor: 44, wantMinor: 11, wantPatch: 3, wantSuffix: "-slim", wantOK: true},
		{name: "with_complex_suffix", in: "44.11.3-bookworm-slim", wantMajor: 44, wantMinor: 11, wantPatch: 3, wantSuffix: "-bookworm-slim", wantOK: true},
		{name: "reject_partial_two_component", in: "44.11", wantOK: false},
		{name: "reject_partial_one_component", in: "44", wantOK: false},
		{name: "reject_four_components", in: "1.2.3.4", wantOK: false},
		{name: "reject_non_numeric", in: "latest", wantOK: false},
		{name: "reject_trailing_dot", in: "1.2.", wantOK: false},
		{name: "reject_empty", in: "", wantOK: false},
		{name: "reject_digit_suffix", in: "1.2.34", wantMajor: 1, wantMinor: 2, wantPatch: 34, wantOK: true}, // this is 1.2.34, not 1.2.3-4
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maj, mi, pa, suffix, ok := parseSemverPrefix(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if maj != tt.wantMajor || mi != tt.wantMinor || pa != tt.wantPatch {
				t.Errorf("got (%d, %d, %d), want (%d, %d, %d)", maj, mi, pa, tt.wantMajor, tt.wantMinor, tt.wantPatch)
			}
			if suffix != tt.wantSuffix {
				t.Errorf("suffix = %q, want %q", suffix, tt.wantSuffix)
			}
		})
	}
}

func TestParseNextLink(t *testing.T) {
	tests := []struct {
		name string
		link string
		base string
		want string
	}{
		{
			name: "absolute_next",
			link: `<https://registry.example.com/v2/foo/tags/list?last=z>; rel="next"`,
			base: "https://registry.example.com/v2/foo/tags/list",
			want: "https://registry.example.com/v2/foo/tags/list?last=z",
		},
		{
			name: "relative_next",
			link: `</v2/foo/tags/list?last=z>; rel="next"`,
			base: "https://registry.example.com/v2/foo/tags/list",
			want: "https://registry.example.com/v2/foo/tags/list?last=z",
		},
		{
			name: "multiple_rels_picks_next",
			link: `</v2/prev>; rel="prev", </v2/next>; rel="next"`,
			base: "https://registry.example.com/",
			want: "https://registry.example.com/v2/next",
		},
		{
			name: "no_next",
			link: `</v2/prev>; rel="prev"`,
			base: "https://registry.example.com/",
			want: "",
		},
		{
			name: "empty_header",
			link: "",
			base: "https://registry.example.com/",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNextLink(tt.link, tt.base)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
