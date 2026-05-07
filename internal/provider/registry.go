package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/AoManoh/openace-mcp/internal/ace"
	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type Registry struct {
	defaultID string
	clients   map[string]*ace.Client
	profiles  []auth.Profile
}

var _ workspace.ClientRouter = (*Registry)(nil)

func NewRegistry(profiles []auth.Profile) (*Registry, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no provider profiles configured")
	}
	registry := &Registry{
		clients:  make(map[string]*ace.Client, len(profiles)),
		profiles: append([]auth.Profile(nil), profiles...),
	}
	for _, profile := range profiles {
		id := strings.TrimSpace(profile.ID)
		if id == "" {
			return nil, fmt.Errorf("provider profile has empty id")
		}
		if _, ok := registry.clients[id]; ok {
			return nil, fmt.Errorf("duplicate provider profile id %q", id)
		}
		session := profile.Session
		registry.clients[id] = ace.NewClient(staticSessionLoader{session: session})
		if profile.Default {
			if registry.defaultID != "" {
				return nil, fmt.Errorf("multiple default provider profiles configured")
			}
			registry.defaultID = id
		}
	}
	if registry.defaultID == "" {
		registry.defaultID = profiles[0].ID
	}
	return registry, nil
}

func (r *Registry) DefaultProviderProfileID() string {
	if r == nil {
		return ""
	}
	return r.defaultID
}

func (r *Registry) ProviderProfileIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.clients))
	for id := range r.clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) ClientForProviderProfile(providerProfileID string) (workspace.ACEClient, error) {
	if r == nil {
		return nil, fmt.Errorf("provider registry is not configured")
	}
	id := strings.TrimSpace(providerProfileID)
	if id == "" {
		id = r.defaultID
	}
	client := r.clients[id]
	if client == nil {
		return nil, fmt.Errorf("unknown provider_profile_id %q", id)
	}
	return client, nil
}

func (r *Registry) HealthSnapshotForProviderProfile(providerProfileID string) (ace.HealthSnapshot, bool) {
	if r == nil {
		return ace.HealthSnapshot{}, false
	}
	id := strings.TrimSpace(providerProfileID)
	if id == "" {
		id = r.defaultID
	}
	client := r.clients[id]
	if client == nil {
		return ace.HealthSnapshot{}, false
	}
	return client.HealthSnapshot(), true
}

type staticSessionLoader struct {
	session auth.Session
}

func (l staticSessionLoader) Load(ctx context.Context) (auth.Session, error) {
	if err := ctx.Err(); err != nil {
		return auth.Session{}, err
	}
	return l.session, nil
}
