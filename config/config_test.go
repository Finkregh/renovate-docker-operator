package config

import (
	"errors"
	"testing"
	"time"
)

func TestParseContainerExtraHosts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty", raw: "", want: nil},
		{name: "only separators", raw: " , ,, ", want: nil},
		{
			name: "single host gateway entry",
			raw:  "git.h.oluflorenzen.de:host-gateway",
			want: []string{"git.h.oluflorenzen.de:host-gateway"},
		},
		{
			name: "multiple entries with whitespace",
			raw:  " forgejo.local:host-gateway , mirror.local:10.0.0.5 ",
			want: []string{"forgejo.local:host-gateway", "mirror.local:10.0.0.5"},
		},
		{
			name: "ipv6 address preserved",
			raw:  "forgejo.local:fd00::1",
			want: []string{"forgejo.local:fd00::1"},
		},
		{name: "missing address", raw: "forgejo.local", wantErr: true},
		{name: "missing address after colon", raw: "forgejo.local:", wantErr: true},
		{name: "missing name", raw: ":host-gateway", wantErr: true},
		{name: "one malformed among valid", raw: "ok.local:host-gateway,broken", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContainerExtraHosts(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseContainerExtraHosts(%q) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseContainerExtraHosts(%q) error: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseContainerExtraHosts(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("entry %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestLoadContainerExtraHosts(t *testing.T) {
	t.Run("defaults to empty", func(t *testing.T) {
		t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
		t.Setenv("RENOVATE_TOKEN", "token")
		t.Setenv("ROP_CONTAINER_EXTRA_HOSTS", "")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if len(cfg.ContainerExtraHosts) != 0 {
			t.Errorf("ContainerExtraHosts = %v, want empty", cfg.ContainerExtraHosts)
		}
	})

	t.Run("parses comma separated entries", func(t *testing.T) {
		t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
		t.Setenv("RENOVATE_TOKEN", "token")
		t.Setenv("ROP_CONTAINER_EXTRA_HOSTS", "git.example.com:host-gateway,mirror.example.com:10.1.2.3")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		want := []string{"git.example.com:host-gateway", "mirror.example.com:10.1.2.3"}
		if len(cfg.ContainerExtraHosts) != len(want) {
			t.Fatalf("ContainerExtraHosts = %v, want %v", cfg.ContainerExtraHosts, want)
		}
		for i := range want {
			if cfg.ContainerExtraHosts[i] != want[i] {
				t.Errorf("entry %d = %q, want %q", i, cfg.ContainerExtraHosts[i], want[i])
			}
		}
		if got := GetValue("ROP_CONTAINER_EXTRA_HOSTS"); got != "git.example.com:host-gateway,mirror.example.com:10.1.2.3" {
			t.Errorf("GetValue(ROP_CONTAINER_EXTRA_HOSTS) = %q", got)
		}
	})

	t.Run("rejects malformed entry", func(t *testing.T) {
		t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
		t.Setenv("RENOVATE_TOKEN", "token")
		t.Setenv("ROP_CONTAINER_EXTRA_HOSTS", "git.example.com")

		if _, err := Load(); err == nil {
			t.Fatal("Load() accepted a malformed ROP_CONTAINER_EXTRA_HOSTS entry")
		}
	})
}

func TestValidate(t *testing.T) {
	badParse := func(_ string) error { return errors.New("nope") }
	okParse := func(_ string) error { return nil }

	tests := []struct {
		name    string
		cfg     *Config
		parse   func(string) error
		wantErr bool
	}{
		{
			name:    "disabled_skips_all_checks",
			cfg:     &Config{CheckUpdateRunnerImage: false, ImageUpdatePollInterval: 1 * time.Second, RenovateImage: "%%invalid%%"},
			parse:   badParse,
			wantErr: false,
		},
		{
			name:    "enabled_floor_violation_rejected",
			cfg:     &Config{CheckUpdateRunnerImage: true, ImageUpdatePollInterval: 5 * time.Minute, RenovateImage: "renovate/renovate:latest"},
			parse:   okParse,
			wantErr: true,
		},
		{
			name:    "enabled_zero_interval_allowed",
			cfg:     &Config{CheckUpdateRunnerImage: true, ImageUpdatePollInterval: 0, RenovateImage: "renovate/renovate:latest"},
			parse:   okParse,
			wantErr: false,
		},
		{
			name:    "enabled_exactly_at_floor_allowed",
			cfg:     &Config{CheckUpdateRunnerImage: true, ImageUpdatePollInterval: MinImageUpdatePollInterval, RenovateImage: "renovate/renovate:latest"},
			parse:   okParse,
			wantErr: false,
		},
		{
			name:    "enabled_invalid_ref_rejected",
			cfg:     &Config{CheckUpdateRunnerImage: true, ImageUpdatePollInterval: 1 * time.Hour, RenovateImage: "%%invalid%%"},
			parse:   badParse,
			wantErr: true,
		},
		{
			name:    "nil_parser_skips_ref_check",
			cfg:     &Config{CheckUpdateRunnerImage: true, ImageUpdatePollInterval: 1 * time.Hour, RenovateImage: "whatever"},
			parse:   nil,
			wantErr: false,
		},
		{
			name:    "nil_config",
			cfg:     nil,
			parse:   okParse,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg, tt.parse)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
