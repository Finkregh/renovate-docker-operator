package imageupdate

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "docker_hub_realm_service_scope",
			in:   `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`,
			want: map[string]string{
				"realm":   "https://auth.docker.io/token",
				"service": "registry.docker.io",
				"scope":   "repository:library/nginx:pull",
			},
		},
		{
			name: "ghcr_realm_service_no_scope",
			in:   `Bearer realm="https://ghcr.io/token",service="ghcr.io"`,
			want: map[string]string{
				"realm":   "https://ghcr.io/token",
				"service": "ghcr.io",
			},
		},
		{
			name: "unquoted_values",
			in:   `Bearer realm=https://auth.example.com/token,service=example`,
			want: map[string]string{
				"realm":   "https://auth.example.com/token",
				"service": "example",
			},
		},
		{
			name: "no_bearer_prefix_accepted",
			in:   `realm="https://auth.example.com/token"`,
			want: map[string]string{"realm": "https://auth.example.com/token"},
		},
		{
			name: "scope_with_commas_inside_quotes",
			in:   `Bearer realm="https://x/y",service="s",scope="repository:foo,bar:pull"`,
			want: map[string]string{
				"realm":   "https://x/y",
				"service": "s",
				"scope":   "repository:foo,bar:pull",
			},
		},
		{name: "missing_realm", in: `Bearer service="s"`, wantErr: true},
		{name: "empty", in: ``, wantErr: true},
		{name: "empty_after_bearer", in: `Bearer `, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChallenge(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSplitOutsideQuotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "no_quotes", in: `a,b,c`, want: []string{"a", "b", "c"}},
		{name: "quoted_comma_preserved", in: `a,"b,c",d`, want: []string{"a", `"b,c"`, "d"}},
		{name: "trailing_empty", in: `a,`, want: []string{"a", ""}},
		{name: "single", in: `a`, want: []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitOutsideQuotes(tt.in, ',')
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClassifyErr_UnwrapsExistingClass(t *testing.T) {
	orig := &ClassifiedError{Class: ClassAuth, Cause: errors.New("boom")}
	wrapped := classifyErr(ClassNetwork, orig)
	var got *ClassifiedError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As failed: %v", wrapped)
	}
	if got.Class != ClassAuth {
		t.Errorf("class = %q, want %q", got.Class, ClassAuth)
	}
}

func TestClassifyErr_NilReturnsNil(t *testing.T) {
	if err := classifyErr(ClassNetwork, nil); err != nil {
		t.Errorf("classifyErr(_, nil) = %v, want nil", err)
	}
}
