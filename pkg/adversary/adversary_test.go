package adversary

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"testing"
)

// This file mirrors the scheduling half of the reference's
// `tests/swap_seeded_adversarial_tests.py`. The corpus's own claim is that another language can
// reproduce the same schedule from the same seed with SHA-256, so the test that matters is
// agreement with the reference: the expected schedules below were produced by the reference's
// own derivation, not by this implementation.

// TestSeededAdversarialSchedulesMatchTheReference pins the default corpus exactly.
//
// These values are the corpus. If this port derived a different schedule from seed 4, a run
// that called itself the same corpus would be injecting different faults — and the agreement
// between implementations would be a coincidence rather than a property.
// Parity: tests/swap_seeded_adversarial_tests.py
func TestSeededAdversarialSchedulesMatchTheReference(t *testing.T) {
	type expected struct {
		primary   string
		reorg     bool
		secondary string
	}

	reference := map[string]expected{
		"4/claim":         {SettlementCrash, true, SettlementNormal},
		"4/refund":        {SettlementCrash, true, SettlementRace},
		"17/claim":        {SettlementCrash, false, SettlementNormal},
		"17/refund":       {SettlementRace, true, SettlementRace},
		"184467/claim":    {SettlementCrash, true, SettlementRace},
		"184467/refund":   {SettlementRace, false, SettlementNormal},
		"20261004/claim":  {SettlementNormal, true, SettlementRace},
		"20261004/refund": {SettlementNormal, true, SettlementRace},
	}

	for _, seed := range DefaultSeeds {
		for _, path := range Paths {
			schedule, err := Build(seed, path)

			if err != nil {
				t.Fatalf("%d/%s: %v", seed, path, err)
			}

			want, present := reference[fmt.Sprintf("%d/%s", seed, path)]

			if !present {
				t.Fatalf("no reference value for %d/%s", seed, path)
			}

			if schedule.PrimarySettlement != want.primary ||
				schedule.ReorgProbe != want.reorg ||
				schedule.SecondarySettle != want.secondary {
				t.Errorf("%d/%s = %+v, want %+v", seed, path, schedule, want)
			}
		}
	}
}

// TestSeededAdversarialChoiceIsSaltedByEveryInput: the derivation is a hash over the version,
// the seed, the path and the stage, so changing any one of them must be able to change the
// answer — otherwise the pieces of the corpus would be correlated in a way it does not intend.
func TestSeededAdversarialChoiceIsSaltedByEveryInput(t *testing.T) {
	options := []string{"a", "b", "c", "d", "e", "f", "g"}

	base, err := Choice(ScheduleV1, 4, PathClaim, "primary-settlement", options)

	if err != nil {
		t.Fatal(err)
	}

	differences := 0

	for _, other := range []struct {
		name    string
		version string
		seed    uint64
		path    string
		stage   string
	}{
		{"another version", "scheduled-v2", 4, PathClaim, "primary-settlement"},
		{"another seed", ScheduleV1, 5, PathClaim, "primary-settlement"},
		{"another path", ScheduleV1, 4, PathRefund, "primary-settlement"},
		{"another stage", ScheduleV1, 4, PathClaim, "reorg-probe"},
	} {
		got, err := Choice(other.version, other.seed, other.path, other.stage, options)

		if err != nil {
			t.Fatalf("%s: %v", other.name, err)
		}

		if got != base {
			differences++
		}
	}

	if differences < 3 {
		t.Fatalf("only %d of four input changes moved the choice: the derivation is not salted", differences)
	}
}

// TestSeededAdversarialDerivationIsTheDocumentedOne recomputes one choice by hand, because the
// contract is the sentence rather than the code.
func TestSeededAdversarialDerivationIsTheDocumentedOne(t *testing.T) {
	material := fmt.Sprintf("%s:%d:%s:%s", ScheduleV1, 4, PathClaim, "primary-settlement")

	digest := sha256.Sum256([]byte(material))

	options := []string{SettlementNormal, SettlementCrash, SettlementRace}

	value := new(big.Int).SetBytes(digest[:])

	want := options[value.Mod(value, big.NewInt(int64(len(options)))).Int64()]

	got, err := Choice(ScheduleV1, 4, PathClaim, "primary-settlement", options)

	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Fatalf("choice = %q, want %q from the documented derivation", got, want)
	}
}

// TestSeededAdversarialNeverDegeneratesToTheHappyPath: every seed must inject at least one
// fault class, which is the rule that keeps the corpus adversarial.
func TestSeededAdversarialNeverDegeneratesToTheHappyPath(t *testing.T) {
	for _, schedule := range Corpus() {
		faulted := schedule.PrimarySettlement != SettlementNormal ||
			schedule.ReorgProbe ||
			schedule.SecondarySettle != SettlementNormal

		if !faulted {
			t.Errorf("%s/%s is a happy path: %+v", schedule.Version, schedule.Path, schedule)
		}
	}
}

// TestSeededAdversarialCorpusCoversBothPaths: a corpus that only ran one path would leave half
// the refund logic untested while still reporting success.
func TestSeededAdversarialCorpusCoversBothPaths(t *testing.T) {
	corpus := Corpus()

	if len(corpus) != len(DefaultSeeds)*len(Paths) {
		t.Fatalf("corpus has %d schedules, want %d", len(corpus), len(DefaultSeeds)*len(Paths))
	}

	paths := map[string]int{}

	for _, schedule := range corpus {
		paths[schedule.Path]++
	}

	for _, path := range Paths {
		if paths[path] == 0 {
			t.Errorf("the corpus never runs the %s path", path)
		}
	}
}

// TestSeededAdversarialRefusesUnknownInputs: an unknown path or an empty stage is a caller
// mistake, and it is refused rather than defaulting to something plausible.
func TestSeededAdversarialRefusesUnknownInputs(t *testing.T) {
	if _, err := Build(4, "neither"); err == nil {
		t.Fatal("an unknown settlement path was accepted")
	}

	if _, err := Choice(ScheduleV1, 4, PathClaim, "stage", nil); err == nil {
		t.Fatal("a stage with no options was accepted")
	}

	if _, err := Choice("", 4, PathClaim, "stage", []string{"a"}); err == nil {
		t.Fatal("a schedule with no version was accepted")
	}
}
