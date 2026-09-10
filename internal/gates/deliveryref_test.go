package gates

import (
	"strings"
	"testing"
)

func protectedRuleset(rules ...string) Rulesets {
	return Rulesets{Applicable: []Ruleset{{
		ID: 7, Name: "delivery ref", Enforcement: "active", Rules: rules,
	}}}
}

func TestCheckDeliveryRefAcceptsAnAppendOnlyRef(t *testing.T) {
	rs := protectedRuleset(ruleNonFastForward, ruleDeletion)
	if got := CheckDeliveryRef("queued", rs); len(got) != 0 {
		t.Fatalf("a protected ref was refused: %v", got)
	}
}

// TestCheckDeliveryRefRefusesAnUnprotectedRef is the case that matters most.
// An unprotected ref is not a weaker gate, it is a path to production nobody
// is watching, so publishing onto it must be refused rather than allowed with
// a warning.
func TestCheckDeliveryRefRefusesAnUnprotectedRef(t *testing.T) {
	got := CheckDeliveryRef("queued", Rulesets{})
	if len(got) == 0 {
		t.Fatal("a ref with no ruleset at all was accepted")
	}
	joined := strings.Join(got, "; ")
	if !strings.Contains(joined, "queued") {
		t.Errorf("problems = %q, want the ref named", joined)
	}
	for _, want := range []string{ruleNonFastForward, ruleDeletion} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems = %q, want it to say which rules to add (%s)", joined, want)
		}
	}
	// ⚠️ THE WORDING IS THE POINT, NOT JUST THE REFUSAL. With no ruleset the
	// rule-union check below also fires, so a broken gate still refuses here
	// -- but it would say "the rulesets applying to queued do not include..."
	// about rulesets that do not exist, sending an operator to edit something
	// that is not there. Both outcomes refuse; only one tells the truth.
	if !strings.Contains(joined, "no ruleset applies") {
		t.Errorf("problems = %q, want it to say no ruleset applies at all", joined)
	}
	if strings.Contains(joined, "do not include") {
		t.Errorf("problems = %q, want it not to describe rulesets that do not exist", joined)
	}
}

func TestCheckDeliveryRefNamesTheRuleThatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present string
		missing string
	}{
		{"force pushes still allowed", ruleDeletion, ruleNonFastForward},
		{"deletion still allowed", ruleNonFastForward, ruleDeletion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDeliveryRef("queued", protectedRuleset(tc.present))
			joined := strings.Join(got, "; ")
			if len(got) == 0 {
				t.Fatalf("a ref missing %s was accepted", tc.missing)
			}
			if !strings.Contains(joined, tc.missing) {
				t.Errorf("problems = %q, want it to name %s", joined, tc.missing)
			}
			if strings.Contains(joined, tc.present) {
				t.Errorf("problems = %q, want it not to demand %s, which is already there", joined, tc.present)
			}
		})
	}
}

// TestCheckDeliveryRefTakesTheUnionAcrossRulesets: two rulesets may each
// contribute one rule and between them protect the ref. Asking a single
// ruleset to carry both would refuse a correct configuration, and an operator
// told to fix something that is not broken learns to distrust the message.
func TestCheckDeliveryRefTakesTheUnionAcrossRulesets(t *testing.T) {
	rs := Rulesets{Applicable: []Ruleset{
		{ID: 1, Name: "no force push", Enforcement: "active", Rules: []string{ruleNonFastForward}},
		{ID: 2, Name: "no deletion", Enforcement: "active", Rules: []string{ruleDeletion}},
	}}
	if got := CheckDeliveryRef("queued", rs); len(got) != 0 {
		t.Fatalf("two rulesets that between them protect the ref were refused: %v", got)
	}
}

// TestCheckDeliveryRefInheritsTheRulesetRefusals: an inactive ruleset or one
// anybody can bypass protects nothing, whatever rules it lists.
func TestCheckDeliveryRefInheritsTheRulesetRefusals(t *testing.T) {
	inactive := protectedRuleset(ruleNonFastForward, ruleDeletion)
	inactive.Applicable[0].Enforcement = "evaluate"
	if got := CheckDeliveryRef("queued", inactive); len(got) == 0 {
		t.Error("a ruleset that is not active was accepted")
	}

	bypassed := protectedRuleset(ruleNonFastForward, ruleDeletion)
	bypassed.Applicable[0].BypassActors = []BypassActor{{ActorType: "User", BypassMode: "always"}}
	got := CheckDeliveryRef("queued", bypassed)
	if len(got) == 0 {
		t.Fatal("a ruleset with a bypass actor was accepted")
	}
	if !strings.Contains(strings.Join(got, "; "), "bypass_actors") {
		t.Errorf("problems = %v, want the bypass named", got)
	}
}
