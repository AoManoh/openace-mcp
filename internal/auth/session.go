package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/pathutil"
)

type Session struct {
	AccessToken string   `json:"accessToken"`
	TenantURL   string   `json:"tenantURL"`
	Scopes      []string `json:"scopes,omitempty"`
	Source      string   `json:"-"`
}

type Profile struct {
	ID      string
	Session Session
	Default bool
	Source  string
}

type Loader struct{}

func NewLoader() *Loader {
	return &Loader{}
}

func (l *Loader) LoadProfiles(ctx context.Context) ([]Profile, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if path := strings.TrimSpace(os.Getenv("OPENACE_PROFILES_FILE")); path != "" {
		return loadProfilesFile(path)
	}
	session, err := l.Load(ctx)
	if err != nil {
		return nil, err
	}
	return []Profile{{
		ID:      "default",
		Session: session,
		Default: true,
		Source:  session.Source,
	}}, nil
}

func (l *Loader) Load(ctx context.Context) (Session, error) {
	if ctx.Err() != nil {
		return Session{}, ctx.Err()
	}

	if raw := strings.TrimSpace(os.Getenv("AUGMENT_SESSION_AUTH")); raw != "" {
		s, err := parseSessionJSON([]byte(raw))
		if err != nil {
			return Session{}, fmt.Errorf("parse AUGMENT_SESSION_AUTH: %w", err)
		}
		s.Source = "AUGMENT_SESSION_AUTH"
		return validate(s)
	}

	if token, tenant := firstEnv("AUGMENT_TOKEN", "SR_TOKEN"), firstEnv("AUGMENT_TENANT", "SR_TENANT"); token != "" || tenant != "" {
		s := Session{AccessToken: token, TenantURL: tenant, Source: "AUGMENT_TOKEN/AUGMENT_TENANT"}
		return validate(s)
	}

	if path := strings.TrimSpace(os.Getenv("OPENACE_SESSION_FILE")); path != "" {
		return loadSessionFile(path, "OPENACE_SESSION_FILE")
	}

	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".augment", "session.json")
		if _, statErr := os.Stat(path); statErr == nil {
			return loadSessionFile(path, "~/.augment/session.json")
		}
	}

	return Session{}, errors.New("no Augment session found; set AUGMENT_SESSION_AUTH, AUGMENT_TOKEN/AUGMENT_TENANT, OPENACE_SESSION_FILE, or login so ~/.augment/session.json exists")
}

func loadProfilesFile(path string) ([]Profile, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read OPENACE_PROFILES_FILE: %w", err)
	}
	profiles, err := parseProfilesJSON(data, "OPENACE_PROFILES_FILE")
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, errors.New("OPENACE_PROFILES_FILE contains no profiles")
	}
	return profiles, nil
}

func loadSessionFile(path string, source string) (Session, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return Session{}, err
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return Session{}, fmt.Errorf("read %s: %w", source, err)
	}
	s, err := parseSessionJSON(data)
	if err != nil {
		return Session{}, fmt.Errorf("parse %s: %w", source, err)
	}
	s.Source = source
	return validate(s)
}

func parseProfilesJSON(data []byte, source string) ([]Profile, error) {
	var wrapper struct {
		DefaultProfileID string          `json:"default_profile_id"`
		DefaultProfile   string          `json:"defaultProfile"`
		Profiles         []profileRecord `json:"profiles"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Profiles) > 0 {
		defaultID := firstNonEmpty(wrapper.DefaultProfileID, wrapper.DefaultProfile)
		return buildProfiles(wrapper.Profiles, defaultID, source)
	}
	var records []profileRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	return buildProfiles(records, "", source)
}

type profileRecord struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AccessToken string   `json:"accessToken"`
	Token       string   `json:"token"`
	TenantURL   string   `json:"tenantURL"`
	Tenant      string   `json:"tenant"`
	Scopes      []string `json:"scopes,omitempty"`
	Session     Session  `json:"session"`
	Auth        Session  `json:"auth"`
	SessionFile string   `json:"sessionFile"`
	Default     bool     `json:"default"`
}

func buildProfiles(records []profileRecord, defaultID string, source string) ([]Profile, error) {
	profiles := make([]Profile, 0, len(records))
	seen := map[string]struct{}{}
	defaultID = strings.TrimSpace(defaultID)
	for i, record := range records {
		id := firstNonEmpty(record.ID, record.Name)
		if id == "" && len(records) == 1 {
			id = "default"
		}
		normalizedID, err := validateProfileID(id)
		if err != nil {
			return nil, fmt.Errorf("profile %d: %w", i+1, err)
		}
		if _, ok := seen[normalizedID]; ok {
			return nil, fmt.Errorf("duplicate profile id %q", normalizedID)
		}
		seen[normalizedID] = struct{}{}
		session, err := profileSession(record, fmt.Sprintf("%s:%s", source, normalizedID))
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", normalizedID, err)
		}
		profiles = append(profiles, Profile{
			ID:      normalizedID,
			Session: session,
			Default: record.Default,
			Source:  session.Source,
		})
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	if defaultID != "" {
		normalizedDefault, err := validateProfileID(defaultID)
		if err != nil {
			return nil, fmt.Errorf("default profile: %w", err)
		}
		found := false
		for i := range profiles {
			if profiles[i].ID == normalizedDefault {
				profiles[i].Default = true
				found = true
			} else {
				profiles[i].Default = false
			}
		}
		if !found {
			return nil, fmt.Errorf("default profile %q not found", normalizedDefault)
		}
		return profiles, nil
	}
	defaultIndex := -1
	for i, profile := range profiles {
		if profile.Default {
			if defaultIndex >= 0 {
				return nil, errors.New("multiple default profiles configured")
			}
			defaultIndex = i
		}
	}
	if defaultIndex < 0 {
		profiles[0].Default = true
	}
	return profiles, nil
}

func profileSession(record profileRecord, source string) (Session, error) {
	if path := strings.TrimSpace(record.SessionFile); path != "" {
		return loadSessionFile(path, source)
	}
	if record.Session.AccessToken != "" || record.Session.TenantURL != "" {
		record.Session.Source = source
		return validate(record.Session)
	}
	if record.Auth.AccessToken != "" || record.Auth.TenantURL != "" {
		record.Auth.Source = source
		return validate(record.Auth)
	}
	session := Session{
		AccessToken: firstNonEmpty(record.AccessToken, record.Token),
		TenantURL:   firstNonEmpty(record.TenantURL, record.Tenant),
		Scopes:      append([]string(nil), record.Scopes...),
		Source:      source,
	}
	return validate(session)
}

func parseSessionJSON(data []byte) (Session, error) {
	var s Session
	if err := json.Unmarshal(data, &s); err == nil && (s.AccessToken != "" || s.TenantURL != "") {
		return s, nil
	}

	var wrapper struct {
		Session Session `json:"session"`
		Auth    Session `json:"auth"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Session{}, err
	}
	if wrapper.Session.AccessToken != "" || wrapper.Session.TenantURL != "" {
		return wrapper.Session, nil
	}
	return wrapper.Auth, nil
}

func validate(s Session) (Session, error) {
	s.AccessToken = strings.TrimSpace(s.AccessToken)
	s.TenantURL = strings.TrimSpace(s.TenantURL)
	if s.AccessToken == "" {
		return Session{}, errors.New("missing accessToken")
	}
	if s.TenantURL == "" {
		return Session{}, errors.New("missing tenantURL")
	}
	return s, nil
}

func validateProfileID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("missing profile id")
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("invalid profile id %q", id)
	}
	return id, nil
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func expandPath(path string) (string, error) {
	return pathutil.ExpandUser(path)
}
