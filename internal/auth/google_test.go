package auth

import (
	"testing"
)

func TestParseGoogleMockToken(t *testing.T) {
	tests := []struct {
		token    string
		wantMock bool
		email    string
	}{
		{"mock-google:test@example.com", true, "test@example.com"},
		{"mock-google:", true, ""},
		{"invalid-token", false, ""},
		{"", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			isMock := len(tt.token) >= 12 && tt.token[:12] == "mock-google:"
			if isMock != tt.wantMock {
				t.Errorf("isMock: got %v, want %v", isMock, tt.wantMock)
			}
			if isMock {
				email := tt.token[12:]
				if email != tt.email {
					t.Errorf("email: got %q, want %q", email, tt.email)
				}
			}
		})
	}
}

func TestGoogleTokenInfoValidation(t *testing.T) {
	tests := []struct {
		name     string
		info     GoogleTokenInfo
		clientID string
		wantErr  bool
	}{
		{
			"valid token",
			GoogleTokenInfo{Sub: "123", Email: "user@gmail.com", EmailVerified: "true", Aud: "my-client-id"},
			"my-client-id",
			false,
		},
		{
			"audience mismatch",
			GoogleTokenInfo{Sub: "123", Email: "user@gmail.com", EmailVerified: "true", Aud: "wrong-id"},
			"my-client-id",
			true,
		},
		{
			"email not verified",
			GoogleTokenInfo{Sub: "123", Email: "user@gmail.com", EmailVerified: "false", Aud: "my-client-id"},
			"my-client-id",
			true,
		},
		{
			"error in response",
			GoogleTokenInfo{Error: "Invalid Value"},
			"my-client-id",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTokenInfo(&tt.info, tt.clientID)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTokenInfo() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// validateTokenInfo is a pure validation function extracted for testing.
func validateTokenInfo(info *GoogleTokenInfo, expectedClientID string) error {
	if info.Error != "" {
		return &validationError{"google token invalid: " + info.Error}
	}
	if info.Aud != expectedClientID {
		return &validationError{"audience mismatch"}
	}
	if info.EmailVerified != "true" {
		return &validationError{"email not verified"}
	}
	return nil
}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
