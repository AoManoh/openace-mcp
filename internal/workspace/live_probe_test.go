package workspace_test

// Live multi-account ACE probe. This is a diagnostic harness, NOT a unit test.
//
// It is skipped unless OPENACE_LIVE_ACCOUNTS points at a JSON file of accounts:
//
//	[{"access_token":"...","tenant_url":"https://d9.api.augmentcode.com/",
//	  "email_note":"...","tag_name":"ACE已耗尽"}]
//
// For each account it drives the REAL sync+retrieve path (workspace.NewSyncer ->
// find-missing -> checkpoint -> agents/codebase-retrieval) against a tiny throwaway
// workspace, then reports which stage failed and the upstream HTTP status. This lets
// us discriminate:
//   - per-account credit exhaustion  (retrieval 429 only on "exhausted" accounts)
//   - platform/free-tier change       (retrieval/indexing fails even on full accounts)
//   - account ban / bad token         (401/403 on every stage)
//
// Access tokens are never printed; only a tenant label and the user-provided tag.
//
// Example:
//
//	OPENACE_LIVE_ACCOUNTS=/home/oh/.codex/private/openace-live-accounts.json \
//	  go test ./internal/workspace -run TestLiveAccountsProbe -v -count=1 -timeout=20m

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openace-mcp/internal/ace"
	"github.com/AoManoh/openace-mcp/internal/auth"
	"github.com/AoManoh/openace-mcp/internal/workspace"
)

type liveAccount struct {
	AccessToken string `json:"access_token"`
	TenantURL   string `json:"tenant_url"`
	EmailNote   string `json:"email_note"`
	TagName     string `json:"tag_name"`
}

type liveSessionLoader struct{ session auth.Session }

func (l liveSessionLoader) Load(context.Context) (auth.Session, error) { return l.session, nil }

var liveStageStatusRe = regexp.MustCompile(`([^\s]+) returned HTTP (\d+)`)

type liveOutcome struct {
	index       int
	tenant      string
	tag         string
	category    string
	ok          bool
	stage       string
	status      int
	rateLimited bool
	retryAfter  time.Duration
	elapsed     time.Duration
	detail      string
}

func TestLiveAccountsProbe(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("OPENACE_LIVE_ACCOUNTS"))
	if path == "" {
		t.Skip("set OPENACE_LIVE_ACCOUNTS=/abs/path/to/accounts.json to run the live multi-account probe")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OPENACE_LIVE_ACCOUNTS: %v", err)
	}
	var accounts []liveAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatalf("parse OPENACE_LIVE_ACCOUNTS as []{access_token,tenant_url,...}: %v", err)
	}
	if len(accounts) == 0 {
		t.Fatal("OPENACE_LIVE_ACCOUNTS contained no accounts")
	}

	query := envOr("OPENACE_LIVE_QUERY", "Where is the program entry point and what does main print?")
	perAccountTimeout := 90 * time.Second
	if v := strings.TrimSpace(os.Getenv("OPENACE_LIVE_TIMEOUT")); v != "" {
		if d, e := time.ParseDuration(v); e == nil && d > 0 {
			perAccountTimeout = d
		}
	}
	// Keep the probe snappy: a credit-exhausted/banned account should fail fast, so do
	// not let our own rate-limit backoff floor stall the harness between stages.
	t.Setenv("OPENACE_RATE_LIMIT_BACKOFF", "1s")

	outcomes := make([]liveOutcome, 0, len(accounts))
	for i, acc := range accounts {
		acc := acc
		tenant := tenantLabel(acc.TenantURL)
		category := classifyTag(acc.TagName)

		t.Setenv("OPENACE_CACHE_DIR", t.TempDir())
		ws := t.TempDir()
		writeProbeWorkspace(t, ws)

		client := ace.NewClient(liveSessionLoader{session: auth.Session{
			AccessToken: acc.AccessToken,
			TenantURL:   acc.TenantURL,
		}})
		syncer := workspace.NewSyncer(client)

		ctx, cancel := context.WithTimeout(context.Background(), perAccountTimeout)
		start := time.Now()
		res, rerr := syncer.Retrieve(ctx, ws, query, 0)
		elapsed := time.Since(start)
		cancel()

		o := liveOutcome{index: i + 1, tenant: tenant, tag: acc.TagName, category: category, elapsed: elapsed}
		if rerr == nil {
			o.ok = true
			o.stage = "agents/codebase-retrieval"
			o.status = 200
			o.detail = fmt.Sprintf("checkpoint=%s files=%d text_len=%d", res.CheckpointID, res.FileCount, len(res.Text))
		} else {
			if ra, isRL := ace.RateLimitInfo(rerr); isRL {
				o.rateLimited = true
				o.retryAfter = ra
			}
			o.stage, o.status = parseStageStatus(rerr.Error())
			o.detail = redactToken(clip(rerr.Error(), 220), acc.AccessToken)
		}
		outcomes = append(outcomes, o)

		t.Logf("[%2d] tenant=%-5s category=%-9s tag=%q\n      ok=%v stage=%s status=%d rate_limited=%v retry_after=%s elapsed=%s\n      detail=%s",
			o.index, o.tenant, o.category, o.tag,
			o.ok, o.stage, o.status, o.rateLimited, o.retryAfter, o.elapsed.Round(10*time.Millisecond), o.detail)
	}

	printLiveSummary(t, outcomes)
}

func printLiveSummary(t *testing.T, outcomes []liveOutcome) {
	t.Helper()
	type bucket struct{ total, ok, retr429, sync429, auth, other int }
	byCat := map[string]*bucket{}
	order := []string{}
	for _, o := range outcomes {
		b := byCat[o.category]
		if b == nil {
			b = &bucket{}
			byCat[o.category] = b
			order = append(order, o.category)
		}
		b.total++
		switch {
		case o.ok:
			b.ok++
		case o.status == 401 || o.status == 403:
			b.auth++
		case o.rateLimited && strings.Contains(o.stage, "codebase-retrieval"):
			b.retr429++
		case o.rateLimited:
			b.sync429++
		default:
			b.other++
		}
	}
	sort.Strings(order)
	var sb strings.Builder
	sb.WriteString("\n==== LIVE PROBE SUMMARY ====\n")
	for _, cat := range order {
		b := byCat[cat]
		sb.WriteString(fmt.Sprintf("%-9s total=%d ok=%d retrieval_429=%d sync/index_429=%d auth_401/403=%d other_err=%d\n",
			cat, b.total, b.ok, b.retr429, b.sync429, b.auth, b.other))
	}
	sb.WriteString("\nInterpretation hints:\n")
	sb.WriteString("- full/active accounts ok, exhausted accounts retrieval_429 => per-account credit exhaustion\n")
	sb.WriteString("- full/active accounts ALSO 429 (retrieval or sync/index) => platform / free-tier policy change\n")
	sb.WriteString("- sync/index_429 > 0 => indexing itself is now metered/charged (not just retrieval)\n")
	sb.WriteString("- auth_401/403 => banned or invalid token (independent of credits)\n")
	t.Log(sb.String())
}

func writeProbeWorkspace(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"openace live probe\")\n}\n",
		"README.md": "# Live Probe Workspace\n\nThrowaway repository used only to exercise ACE sync + retrieval.\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write probe file %s: %v", name, err)
		}
	}
}

func parseStageStatus(msg string) (string, int) {
	m := liveStageStatusRe.FindStringSubmatch(msg)
	if len(m) != 3 {
		return "unknown", 0
	}
	status := 0
	for _, r := range m[2] {
		status = status*10 + int(r-'0')
	}
	return m[1], status
}

func classifyTag(tag string) string {
	t := strings.TrimSpace(tag)
	switch {
	case t == "":
		return "untagged"
	case strings.Contains(t, "耗尽"):
		return "exhausted"
	case strings.Contains(t, "使用") || strings.Contains(t, "测试") || strings.Contains(t, "笔记本"):
		return "active"
	default:
		return "other"
	}
}

func tenantLabel(tenantURL string) string {
	u, err := url.Parse(strings.TrimSpace(tenantURL))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(tenantURL)
	}
	if i := strings.IndexByte(u.Host, '.'); i > 0 {
		return u.Host[:i]
	}
	return u.Host
}

func redactToken(s, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "****")
}

func clip(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
