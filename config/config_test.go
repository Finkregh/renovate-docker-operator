package config

import (
	"errors"
	"testing"
	"time"
)

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
