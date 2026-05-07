package provider

import (
	"testing"

	"github.com/AoManoh/openace-mcp/internal/auth"
)

func TestRegistryRoutesDefaultAndExplicitProfiles(t *testing.T) {
	registry, err := NewRegistry([]auth.Profile{
		{
			ID:      "primary",
			Default: true,
			Session: auth.Session{AccessToken: "token-primary", TenantURL: "https://primary.example.test/"},
		},
		{
			ID:      "standby",
			Session: auth.Session{AccessToken: "token-standby", TenantURL: "https://standby.example.test/"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.DefaultProviderProfileID(); got != "primary" {
		t.Fatalf("default provider = %q", got)
	}
	defaultClient, err := registry.ClientForProviderProfile("")
	if err != nil {
		t.Fatal(err)
	}
	explicitClient, err := registry.ClientForProviderProfile("primary")
	if err != nil {
		t.Fatal(err)
	}
	if defaultClient != explicitClient {
		t.Fatal("empty provider should route to configured default profile client")
	}
	standbyClient, err := registry.ClientForProviderProfile("standby")
	if err != nil {
		t.Fatal(err)
	}
	if standbyClient == defaultClient {
		t.Fatal("standby profile should have an independent ACE client")
	}
	if _, err := registry.ClientForProviderProfile("missing"); err == nil {
		t.Fatal("unknown provider profile should be rejected")
	}
}

func TestRegistryRejectsDuplicateProfiles(t *testing.T) {
	_, err := NewRegistry([]auth.Profile{
		{ID: "primary", Session: auth.Session{AccessToken: "token-a", TenantURL: "https://a.example.test/"}},
		{ID: "primary", Session: auth.Session{AccessToken: "token-b", TenantURL: "https://b.example.test/"}},
	})
	if err == nil {
		t.Fatal("duplicate provider profile IDs should be rejected")
	}
}
