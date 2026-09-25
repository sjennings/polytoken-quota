package selection

import (
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/quota"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

func TestParseGroup(t *testing.T) {
	g, err := ParseGroup([]byte(`{"group":"fast","models":["codex/gpt-5.6-sol(high)","zai/glm-5.2"]}`))
	if err != nil || g.Name != "fast" || len(g.Models) != 2 || g.Models[0] != "codex/gpt-5.6-sol(high)" {
		t.Fatalf("got %+v, %v", g, err)
	}
	for name, doc := range map[string]string{
		"empty":         ``,
		"no name":       `{"models":["codex/gpt-5.6-sol"]}`,
		"no models":     `{"group":"fast","models":[]}`,
		"unknown field": `{"group":"fast","models":["codex/gpt-5.6-sol"],"extra":1}`,
		"trailing":      `{"group":"fast","models":["codex/gpt-5.6-sol"]} {}`,
		"nested ref":    `{"group":"fast","models":["@mg:pair"]}`,
		"bare mg ref":   `{"group":"fast","models":["mg:pair"]}`,
		"malformed":     `{"group":"fast","models":["codex/x(high"]}`,
		"oversize":      `{"group":"fast","models":["` + strings.Repeat("a", MaxGroupBytes) + `"]}`,
	} {
		if _, err := ParseGroup([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSelectGroupFailoverOrder(t *testing.T) {
	fresh := func(mid string, rem float64) *quota.QuotaSnapshot {
		return selSnap(mid, selNow.Add(-time.Minute), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 1-rem, 1))
	}
	stale := func(mid string) *quota.QuotaSnapshot {
		return selSnap(mid, selNow.Add(-24*time.Hour), quota.SourceFresh, quota.QuotaAvailable, selWin("weekly", 0.1, 1))
	}
	run := func(st state.State, models []string, excluded ...string) ([]MemberReport, int) {
		return SelectGroup(ModelGroup{Name: "g", Models: models}, selDesired(), st, selNow, excluded)
	}

	t.Run("first confirmed wins over higher headroom later", func(t *testing.T) {
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(fresh("codex", 0.10), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.90), nil),
		})
		m, sel := run(st, []string{"codex/gpt-5.6-sol(high)", "zai/glm-5.2"})
		if sel != 0 || m[0].Status != MemberConfirmed || m[0].Reference != "codex/gpt-5.6-sol(high)" || m[0].CheckedAt == nil {
			t.Fatalf("sel=%d members=%+v", sel, m)
		}
	})

	t.Run("exhausted member skipped, later confirmed chosen over earlier uncertain", func(t *testing.T) {
		st := selState(map[string]state.ProviderState{
			"codex": *selHealthyPS(stale("codex"), nil),
			"zai":   *selHealthyPS(fresh("zai", 0.50), nil),
		})
		m, sel := run(st, []string{"codex/gpt-5.6-sol", "zai/glm-5.2"})
		if sel != 1 || m[0].Status != MemberUncertain || m[0].Reason != EvidenceStale {
			t.Fatalf("sel=%d members=%+v", sel, m)
		}
		st.Providers["zai"] = *selHealthyPS(fresh("zai", 0), nil)
		m, sel = run(st, []string{"zai/glm-5.2", "codex/gpt-5.6-sol"})
		if sel != 1 || m[0].Status != MemberExcluded {
			t.Fatalf("exhausted not skipped: sel=%d members=%+v", sel, m)
		}
	})

	t.Run("unmanaged member is uncertain fallback", func(t *testing.T) {
		m, sel := run(selState(nil), []string{"local/unregistered"})
		if sel != 0 || m[0].Status != MemberUncertain || m[0].Reason != EvidenceUnmanaged || m[0].Mapping != "" {
			t.Fatalf("sel=%d members=%+v", sel, m)
		}
	})

	t.Run("all excluded selects nothing", func(t *testing.T) {
		st := selState(map[string]state.ProviderState{"codex": *selHealthyPS(fresh("codex", 0.5), nil)})
		m, sel := run(st, []string{"codex/gpt-5.6-sol"}, "codex")
		if sel != -1 || m[0].Status != MemberExcluded {
			t.Fatalf("sel=%d members=%+v", sel, m)
		}
	})
}
