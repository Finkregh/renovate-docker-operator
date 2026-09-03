package imageupdate

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Reference
		wantErr bool
	}{
		// Plan-specified fixtures.
		{
			name:  "docker_hub_namespaced_with_tag",
			input: "renovate/renovate:latest",
			want:  Reference{Registry: "index.docker.io", Repository: "renovate/renovate", Tag: "latest"},
		},
		{
			name:  "docker_hub_namespaced_default_tag",
			input: "renovate/renovate",
			want:  Reference{Registry: "index.docker.io", Repository: "renovate/renovate", Tag: "latest"},
		},
		{
			name:  "docker_hub_bare_expands_library",
			input: "nginx",
			want:  Reference{Registry: "index.docker.io", Repository: "library/nginx", Tag: "latest"},
		},
		{
			name:  "ghcr_with_tag",
			input: "ghcr.io/renovatebot/renovate:44.11.3",
			want:  Reference{Registry: "ghcr.io", Repository: "renovatebot/renovate", Tag: "44.11.3"},
		},
		{
			name:  "private_registry_with_port_and_tag",
			input: "registry.example.com:5000/team/img:v1",
			want:  Reference{Registry: "registry.example.com:5000", Repository: "team/img", Tag: "v1"},
		},
		{
			name:  "digest_pinned",
			input: "registry.example.com/team/img@sha256:abc123",
			want:  Reference{Registry: "registry.example.com", Repository: "team/img", Digest: "sha256:abc123"},
		},
		{
			name:  "localhost_host",
			input: "localhost/img",
			want:  Reference{Registry: "localhost", Repository: "img", Tag: "latest"},
		},

		// Additional coverage.
		{
			name:  "localhost_with_port_and_tag",
			input: "localhost:5000/img:v2",
			want:  Reference{Registry: "localhost:5000", Repository: "img", Tag: "v2"},
		},
		{
			name:  "docker_hub_bare_with_tag",
			input: "nginx:1.25",
			want:  Reference{Registry: "index.docker.io", Repository: "library/nginx", Tag: "1.25"},
		},
		{
			name:  "docker_hub_namespaced_with_digest",
			input: "renovate/renovate@sha256:deadbeef",
			want:  Reference{Registry: "index.docker.io", Repository: "renovate/renovate", Digest: "sha256:deadbeef"},
		},
		{
			name:  "docker_hub_namespaced_tag_and_digest",
			input: "renovate/renovate:44.11.3@sha256:cafebabe",
			want:  Reference{Registry: "index.docker.io", Repository: "renovate/renovate", Tag: "44.11.3", Digest: "sha256:cafebabe"},
		},
		{
			name:  "ghcr_bare_default_tag",
			input: "ghcr.io/renovatebot/renovate",
			want:  Reference{Registry: "ghcr.io", Repository: "renovatebot/renovate", Tag: "latest"},
		},
		{
			name:  "family_suffix_tag",
			input: "renovate/renovate:44.11.3-slim",
			want:  Reference{Registry: "index.docker.io", Repository: "renovate/renovate", Tag: "44.11.3-slim"},
		},

		// Errors.
		{name: "empty_string", input: "", wantErr: true},
		{name: "trailing_at_no_digest", input: "renovate/renovate@", wantErr: true},
		{name: "trailing_colon_no_tag", input: "renovate/renovate:", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, nil; want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q)\n  got  = %+v\n  want = %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParse_EmptyIsSentinel(t *testing.T) {
	_, err := Parse("")
	if !errors.Is(err, ErrEmptyRef) {
		t.Fatalf("Parse(\"\") = %v, want ErrEmptyRef", err)
	}
}
