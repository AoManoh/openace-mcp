package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Session struct {
	AccessToken string   `json:"accessToken"`
	TenantURL   string   `json:"tenantURL"`
	Scopes      []string `json:"scopes,omitempty"`
	Source      string   `json:"-"`
}

type Loader struct{}

func NewLoader() *Loader {
	return &Loader{}
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

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("empty path")
	}
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
