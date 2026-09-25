package selection

// group.go — quota-aware selection within one caller-supplied Polytoken model
// group. polytoken-quota never reads Polytoken's model-group configuration:
// the caller (typically a skill that already knows which group it is about to
// use) sends the group's flattened, ordered members as JSON, and this file
// reports each member's quota evidence and recommends one member.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/geofffranks/polytoken-quota/internal/policy"
	"github.com/geofffranks/polytoken-quota/internal/state"
)

// MaxGroupBytes bounds a model-group request document.
const MaxGroupBytes = MaxPolicyBytes

// EvidenceUnmanaged marks a group member whose model is not registered in the
// desired configuration: polytoken-quota has no quota evidence for it, but
// Polytoken may still route to it, so it remains an uncertain fallback.
const EvidenceUnmanaged = "unmanaged"

// Member statuses in a group report.
const (
	MemberConfirmed = "confirmed"
	MemberUncertain = "uncertain"
	MemberExcluded  = "excluded"
)

// groupRefPrefixes are Polytoken's model-group reference spellings. Members
// must already be flattened by the caller; nested references are rejected.
var groupRefPrefixes = []string{"@modelgroup:", "@mg:", "modelgroup:", "mg:"}

// ModelGroup is a parsed model-group request: a caller-chosen name (reported back,
// never resolved) and the group's concrete members in failover order.
type ModelGroup struct {
	Name   string
	Models []string
}

// ParseGroup parses a strict model-group request:
//
//	{"group": "fast", "models": ["codex/example(high)", "anthropic/example"]}
//
// Unknown fields, trailing data, an empty name or member list, malformed
// references, and unflattened group references are rejected. Members need not
// be registered in the desired configuration.
func ParseGroup(data []byte) (ModelGroup, error) {
	if len(data) > MaxGroupBytes {
		return ModelGroup{}, fmt.Errorf("selection: group request is %d bytes, over the %d byte limit", len(data), MaxGroupBytes)
	}
	var w struct {
		Group  string   `json:"group"`
		Models []string `json:"models"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		if errors.Is(err, io.EOF) {
			return ModelGroup{}, errors.New("selection: empty group request")
		}
		return ModelGroup{}, fmt.Errorf("selection: parse group request: %w", err)
	}
	if dec.More() {
		return ModelGroup{}, errors.New("selection: unexpected trailing data after the group request")
	}
	if w.Group == "" {
		return ModelGroup{}, errors.New("selection: group name is required")
	}
	if len(w.Models) == 0 {
		return ModelGroup{}, errors.New("selection: group has no models")
	}
	for i, ref := range w.Models {
		for _, prefix := range groupRefPrefixes {
			if strings.HasPrefix(ref, prefix) {
				return ModelGroup{}, fmt.Errorf("selection: member %d is a model-group reference; flatten nested groups before calling", i)
			}
		}
		if _, _, err := policy.ParseModelRef(ref); err != nil {
			return ModelGroup{}, fmt.Errorf("selection: member %d: %w", i, err)
		}
	}
	return ModelGroup{Name: w.Group, Models: w.Models}, nil
}

// MemberReport is one group member's sanitized quota classification.
type MemberReport struct {
	Reference string
	Mapping   string
	// Status is MemberConfirmed, MemberUncertain, or MemberExcluded.
	Status string
	// Reason is the exclusion reason, the uncertain evidence category, or
	// "available quota" for confirmed members.
	Reason    string
	Headroom  *float64
	CheckedAt *time.Time
}

// SelectGroup classifies every member in order and picks one with Polytoken's
// failover semantics: the first confirmed member, otherwise the first
// uncertain member. Unlike candidate-policy groups, members are not ranked by
// headroom — the authored order is the operator's preference. selected is -1
// when every member is excluded.
func SelectGroup(g ModelGroup, desired policy.Desired, st state.State, asOf time.Time, excludedFamilies []string) (members []MemberReport, selected int) {
	excluded := make(map[string]bool, len(excludedFamilies))
	for _, f := range excludedFamilies {
		if f != "" {
			excluded[f] = true
		}
	}
	members = make([]MemberReport, len(g.Models))
	firstUncertain := -1
	selected = -1
	for i, ref := range g.Models {
		c := classify(ref, desired, st, asOf, excluded)
		m := MemberReport{Reference: ref, Mapping: string(c.mapping), Reason: c.reason, Headroom: c.headroom}
		switch {
		case c.status == candConfirmed:
			m.Status = MemberConfirmed
		case c.status == candUncertain:
			m.Status = MemberUncertain
		case c.reason == "unresolved model":
			m.Status, m.Reason = MemberUncertain, EvidenceUnmanaged
		default:
			m.Status = MemberExcluded
		}
		if ps, ok := st.Providers[m.Mapping]; ok && m.Mapping != "" && ps.QuotaSnapshot != nil && !ps.QuotaSnapshot.CheckedAt.IsZero() {
			at := ps.QuotaSnapshot.CheckedAt
			m.CheckedAt = &at
		}
		members[i] = m
		if m.Status == MemberConfirmed && selected < 0 {
			selected = i
		}
		if m.Status == MemberUncertain && firstUncertain < 0 {
			firstUncertain = i
		}
	}
	if selected < 0 {
		selected = firstUncertain
	}
	return members, selected
}

// GroupRequest is one select-group invocation.
type GroupRequest struct {
	Group            ModelGroup
	ExcludedFamilies []string
	// RefreshFirst performs one quota check (no reconciliation) first.
	RefreshFirst bool
}

// GroupOutcome is one safe select-group result.
type GroupOutcome struct {
	Status    SelectStatus
	Reason    string
	Group     string
	Refreshed bool
	AsOf      time.Time
	// Selected indexes Members; -1 when Status is no_selection.
	Selected int
	Members  []MemberReport
}

// RunGroup executes one select-group invocation against the same snapshot and
// refresh sources as Run. It never assesses difficulty: the caller already
// chose the group. The returned error is always a *FatalError.
func (r *SelectRunner) RunGroup(ctx context.Context, req GroupRequest) (GroupOutcome, error) {
	out := GroupOutcome{Group: req.Group.Name, Selected: -1}
	if req.Group.Name == "" || len(req.Group.Models) == 0 {
		return GroupOutcome{}, fatal(FatalRequest, errors.New("a non-empty model group is required"))
	}
	if req.RefreshFirst {
		if r.Refresh == nil {
			return GroupOutcome{}, fatal(FatalRefresh, errors.New("no refresher configured"))
		}
		if err := r.Refresh.RefreshQuota(ctx); err != nil {
			return GroupOutcome{}, fatal(FatalRefresh, err)
		}
		out.Refreshed = true
	}
	if r.Snapshot == nil {
		return GroupOutcome{}, fatal(FatalSnapshot, errors.New("no snapshot source configured"))
	}
	snap, err := r.Snapshot.SelectionSnapshot(ctx)
	if err != nil {
		return GroupOutcome{}, fatal(FatalSnapshot, err)
	}
	out.AsOf = snap.AsOf
	out.Members, out.Selected = SelectGroup(req.Group, snap.Desired, snap.State, snap.AsOf, req.ExcludedFamilies)
	switch {
	case out.Selected < 0:
		out.Status, out.Reason = SelectNoSelection, ReasonNoEligibleCandidate
	case out.Members[out.Selected].Status == MemberConfirmed:
		out.Status, out.Reason = SelectConfirmed, ReasonFreshQuotaEvidence
	default:
		out.Status, out.Reason = SelectUncertain, ReasonUncertainQuotaEvidence
	}
	return out, nil
}
