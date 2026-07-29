package config

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestValidateWebAuthnConfig(t *testing.T) {
	tests := []struct {
		name      string
		webauthn  WebAuthnConfig
		wantError string
	}{
		{
			name: "valid production origin",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API", RPID: "sub2api.example.com",
				RPOrigins: []string{"https://sub2api.example.com"}},
		},
		{
			name: "valid localhost development origin",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API Dev", RPID: "localhost",
				RPOrigins: []string{"http://localhost:5173"}},
		},
		{
			name: "missing relying party id",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API",
				RPOrigins: []string{"https://sub2api.example.com"}},
			wantError: "webauthn.rp_id",
		},
		{
			name: "relying party id contains scheme",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API", RPID: "https://sub2api.example.com",
				RPOrigins: []string{"https://sub2api.example.com"}},
			wantError: "domain without scheme",
		},
		{
			name: "non-local insecure origin",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API", RPID: "sub2api.example.com",
				RPOrigins: []string{"http://sub2api.example.com"}},
			wantError: "must use HTTPS",
		},
		{
			name: "origin outside relying party id",
			webauthn: WebAuthnConfig{Enabled: true, RPDisplayName: "Sub2API", RPID: "example.com",
				RPOrigins: []string{"https://example.net"}},
			wantError: "not within relying party ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
			cfg, err := Load()
			require.NoError(t, err)
			cfg.WebAuthn = tt.webauthn

			err = cfg.Validate()
			if tt.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantError)
			}
		})
	}
}

func TestLoadWebAuthnConfigFromEnvironment(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("WEBAUTHN_ENABLED", "true")
	t.Setenv("WEBAUTHN_RP_DISPLAY_NAME", "Nuro S2")
	t.Setenv("WEBAUTHN_RP_ID", "panel.example.com")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "https://panel.example.com,https://admin.panel.example.com")

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.WebAuthn.Enabled)
	require.Equal(t, "Nuro S2", cfg.WebAuthn.RPDisplayName)
	require.Equal(t, "panel.example.com", cfg.WebAuthn.RPID)
	require.Equal(t, []string{"https://panel.example.com", "https://admin.panel.example.com"}, cfg.WebAuthn.RPOrigins)
}
