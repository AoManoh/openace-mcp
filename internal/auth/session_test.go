package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfilesFallsBackToLegacySessionEnv(t *testing.T) {
	t.Setenv("OPENACE_PROFILES_FILE", "")
	t.Setenv("AUGMENT_SESSION_AUTH", "")
	t.Setenv("OPENACE_SESSION_FILE", "")
	t.Setenv("AUGMENT_TOKEN", "token-default")
	t.Setenv("AUGMENT_TENANT", "https://example.test/")

	profiles, err := NewLoader().LoadProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected one fallback profile, got %d", len(profiles))
	}
	profile := profiles[0]
	if profile.ID != "default" || !profile.Default {
		t.Fatalf("unexpected fallback profile metadata: %+v", profile)
	}
	if profile.Session.AccessToken != "token-default" || profile.Session.TenantURL != "https://example.test/" {
		t.Fatalf("unexpected fallback profile session: %+v", profile.Session)
	}
}

func TestLoadProfilesFileParsesDefaultAndSessionFile(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "standby.json")
	if err := os.WriteFile(sessionPath, []byte(`{"accessToken":"token-standby","tenantURL":"https://standby.example.test/"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profilesPath := filepath.Join(dir, "profiles.json")
	data := `{
  "default_profile_id": "primary",
  "profiles": [
    {"id": "primary", "accessToken": "token-primary", "tenantURL": "https://primary.example.test/"},
    {"id": "standby", "sessionFile": "` + filepath.ToSlash(sessionPath) + `"}
  ]
}`
	if err := os.WriteFile(profilesPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENACE_PROFILES_FILE", profilesPath)

	profiles, err := NewLoader().LoadProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected two profiles, got %d", len(profiles))
	}
	if profiles[0].ID != "primary" || !profiles[0].Default || profiles[0].Session.AccessToken != "token-primary" {
		t.Fatalf("unexpected primary profile: %+v", profiles[0])
	}
	if profiles[1].ID != "standby" || profiles[1].Default || profiles[1].Session.AccessToken != "token-standby" {
		t.Fatalf("unexpected standby profile: %+v", profiles[1])
	}
}

func TestParseProfilesRejectsDuplicateIDs(t *testing.T) {
	_, err := parseProfilesJSON([]byte(`[
  {"id":"primary","accessToken":"token-a","tenantURL":"https://a.example.test/"},
  {"id":"primary","accessToken":"token-b","tenantURL":"https://b.example.test/"}
]`), "test")
	if err == nil {
		t.Fatal("duplicate profile IDs should be rejected")
	}
}
