package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/selection"
)

type groupSpy struct {
	calls   int
	request selection.GroupRequest
	outcome selection.GroupOutcome
}

func (g *groupSpy) RunGroup(_ context.Context, req selection.GroupRequest) (selection.GroupOutcome, error) {
	g.calls++
	g.request = req
	return g.outcome, nil
}

func TestSelectGroupJSONAndExitCodes(t *testing.T) {
	headroom := 0.4
	spy := &groupSpy{outcome: selection.GroupOutcome{
		Status: selection.SelectConfirmed, Reason: selection.ReasonFreshQuotaEvidence, Group: "fast", Selected: 1,
		AsOf: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Members: []selection.MemberReport{
			{Reference: "codex/a(high)", Mapping: "codex", Status: selection.MemberExcluded, Reason: "quota exhausted"},
			{Reference: "zai/b", Mapping: "zai", Status: selection.MemberConfirmed, Reason: "available quota", Headroom: &headroom},
		},
	}}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select-group", "--exclude-family", "anthropic", "--json"},
		strings.NewReader(`{"group":"fast","models":["codex/a(high)","zai/b"]}`), &stdout, &stderr, Dependencies{SelectGroup: spy})
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if spy.request.Group.Name != "fast" || len(spy.request.Group.Models) != 2 || spy.request.ExcludedFamilies[0] != "anthropic" {
		t.Fatalf("request=%+v", spy.request)
	}
	var got groupOutcomeJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Model == nil || *got.Model != "zai/b" || len(got.Members) != 2 || got.Members[0].Status != "excluded" || got.Members[1].Headroom == nil {
		t.Fatalf("envelope=%s", stdout.String())
	}

	spy.outcome.Status, spy.outcome.Selected = selection.SelectNoSelection, -1
	stdout.Reset()
	code = Run(context.Background(), []string{"select-group", "--json"}, strings.NewReader(`{"group":"fast","models":["codex/a"]}`), &stdout, &stderr, Dependencies{SelectGroup: spy})
	if code != ExitPending || !strings.Contains(stdout.String(), `"model":null`) {
		t.Fatalf("no_selection exit=%d out=%s", code, stdout.String())
	}
}

func TestSelectGroupRejectsUnflattenedGroupBeforeRunner(t *testing.T) {
	spy := &groupSpy{}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"select-group", "--json"}, strings.NewReader(`{"group":"fast","models":["@mg:pair"]}`), &stdout, &stderr, Dependencies{SelectGroup: spy})
	if code != ExitRejected || spy.calls != 0 || !strings.Contains(stdout.String(), `"status":"error"`) {
		t.Fatalf("exit=%d calls=%d out=%s", code, spy.calls, stdout.String())
	}
}
