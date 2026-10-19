package nostr

import (
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
)

// These tests pin the filter semantics a relay applies on the way out and a client applies when
// it decides what a subscription returned. The rule is conjunctive across the fields that are
// set and disjunctive within each: an event matches when it satisfies every constraint.

// filterEvent is an event with one of each tag the market filters select on.
func filterEvent() event.Event {
	return event.Event{
		ID:        "aa" + "",
		Pubkey:    "bb",
		Kind:      event.KindTextNote,
		CreatedAt: 100,
		Tags: [][]string{
			{"t", "blake-swap-market"},
			{"e", "cc"},
			{"p", "dd"},
			{"d", "eeee00000001"},
		},
		Content: "{}",
	}
}

// TestFilterMatchesAnEmptyFilter: no constraint means everything matches, which is what an empty
// filter means on the wire.
func TestFilterMatchesAnEmptyFilter(t *testing.T) {
	if !(Filter{}).Matches(filterEvent()) {
		t.Fatal("an empty filter refused an event")
	}
}

// TestFilterMatchesEachField: every field, when set, is a constraint.
func TestFilterMatchesEachField(t *testing.T) {
	candidate := filterEvent()

	cases := map[string]Filter{
		"kind":   {Kinds: []int{int(event.KindTextNote)}},
		"author": {Authors: []string{"bb"}},
		"id":     {Ids: []string{"aa"}},
		"e tag":  {ETags: []string{"cc"}},
		"p tag":  {PTags: []string{"dd"}},
		"d tag":  {DTags: []string{"eeee00000001"}},
		"topic":  {Topics: []string{"blake-swap-market"}},
	}

	for name, filter := range cases {
		if !filter.Matches(candidate) {
			t.Errorf("%s: a matching event was refused", name)
		}
	}
}

// TestFilterRefusesEachMismatch: the same fields, with a value the event does not have.
func TestFilterRefusesEachMismatch(t *testing.T) {
	candidate := filterEvent()

	cases := map[string]Filter{
		"kind":   {Kinds: []int{7}},
		"author": {Authors: []string{"ff"}},
		"id":     {Ids: []string{"ff"}},
		"e tag":  {ETags: []string{"ff"}},
		"p tag":  {PTags: []string{"ff"}},
		"d tag":  {DTags: []string{"ff"}},
		"topic":  {Topics: []string{"something-else"}},
	}

	for name, filter := range cases {
		if filter.Matches(candidate) {
			t.Errorf("%s: a non-matching event was accepted", name)
		}
	}
}

// TestFilterIsConjunctiveAcrossFields: satisfying one field is not satisfying the filter.
func TestFilterIsConjunctiveAcrossFields(t *testing.T) {
	candidate := filterEvent()

	if (Filter{Authors: []string{"bb"}, Topics: []string{"something-else"}}).Matches(candidate) {
		t.Fatal("a filter matched with only one of its constraints satisfied")
	}
}

// TestFilterIsDisjunctiveWithinAField: any listed value is enough.
func TestFilterIsDisjunctiveWithinAField(t *testing.T) {
	candidate := filterEvent()

	// An empty topic means no constraint, so it must not narrow the selection.
	if (Filter{Authors: []string{"ff", "bb"}}).Matches(candidate) == false {
		t.Fatal("the second of two authors did not match")
	}

	if !(Filter{Topics: []string{"first", "blake-swap-market"}}).Matches(candidate) {
		t.Fatal("the second of two topics did not match")
	}
}
