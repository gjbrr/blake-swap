// Package adversary is the seeded adversarial corpus's scheduler.
//
// The reference corpus does not fuzz internals. It generates protocol-level events from a
// seed, and it is deliberately reproducible from another language: every decision is
//
//	SHA-256("<schedule-version>:<seed>:<path>:<stage>") mod len(options)
//
// interpreted as an unsigned big-endian integer. That sentence is the whole contract, and it
// exists so a second implementation can replay the same schedule without sharing code. This
// package is that second implementation.
//
// A schedule names faults only at stages where the fault means something — the settlement the
// path actually performs, the reorganisation probe after it, then the other chain's settlement
// — so a corpus run is adversarial in a way a person can read rather than a stream of noise.
package adversary

import (
	"crypto/sha256"
	"fmt"
	"math/big"
)

// Schedule versions. Each is a separate derivation, kept because a corpus revision must not
// silently change what the previous one meant.
const (
	ScheduleV1 = "scheduled-v1"
)

// DefaultSeeds are the corpus seeds the reference runs by default.
var DefaultSeeds = []uint64{4, 17, 184467, 20261004}

// Paths a settlement can take.
const (
	PathClaim  = "claim"
	PathRefund = "refund"
)

// Paths is the path vocabulary, in the order the reference lists it.
var Paths = []string{PathClaim, PathRefund}

// The decision vocabulary at each stage.
const (
	SettlementNormal = "normal"
	SettlementCrash  = "crash"
	SettlementRace   = "race"
)

// FaultProfiles are the corpus's named fault profiles.
var FaultProfiles = []string{
	"none",
	"settlement-crash-v1",
	"reorg-v1",
	"settlement-race-v1",
	"refund-crash-v1",
	"refund-reorg-v1",
	"refund-race-v1",
	"combined-fault-v1",
	"combined-refund-fault-v1",
	"scheduled-v1",
}

// FaultVocabulary is every fault the scheduled corpus may inject, as the reference writes it.
var FaultVocabulary = []string{
	"primary:normal",
	"primary:crash",
	"primary:race",
	"reorg:skip",
	"reorg:probe",
	"secondary:normal",
	"secondary:race",
}

// Schedule is the fault plan for one seed and path.
//
// The version is carried so a replay knows which derivation produced it, and the path so a
// digest of the plan cannot be confused between a claim run and a refund run.
type Schedule struct {
	Version           string `json:"version"`
	Path              string `json:"path"`
	PrimarySettlement string `json:"primary_settlement"`
	ReorgProbe        bool   `json:"reorg_probe"`
	SecondarySettle   string `json:"secondary_settlement"`
}

// Choice derives one scheduling decision.
//
// The inputs are serialized with the exact separators the reference uses; a different join
// would produce a different schedule, which is why the format is spelled out in the package
// comment rather than left to the caller.
func Choice(version string, seed uint64, path, stage string, options []string) (string, error) {
	if version == "" {
		return "", fmt.Errorf("a schedule needs a version")
	}

	if len(options) == 0 {
		return "", fmt.Errorf("a scheduling stage needs at least one option")
	}

	material := fmt.Sprintf("%s:%d:%s:%s", version, seed, path, stage)

	digest := sha256.Sum256([]byte(material))

	// The reference reads the *whole* digest as an unsigned big-endian integer, not
	// just its leading word: a shorter read would keep the derivation's spirit and lose
	// its identity, which is exactly the kind of near-miss a replay would not notice.
	value := new(big.Int).SetBytes(digest[:])
	index := value.Mod(value, big.NewInt(int64(len(options)))).Int64()

	return options[index], nil
}

// Build returns the fault plan for one seed and path.
//
// The last rule is not scheduling but corpus safety: a seed whose independent choices happen
// to be fault-free would turn an adversarial run into a happy-path run, so the secondary
// settlement is promoted to a race. The reference states the reason exactly that way.
func Build(seed uint64, path string) (Schedule, error) {
	if path != PathClaim && path != PathRefund {
		return Schedule{}, fmt.Errorf("unknown settlement path %q", path)
	}

	primary, err := Choice(ScheduleV1, seed, path, "primary-settlement",
		[]string{SettlementNormal, SettlementCrash, SettlementRace})

	if err != nil {
		return Schedule{}, err
	}

	probe, err := Choice(ScheduleV1, seed, path, "reorg-probe", []string{"false", "true"})

	if err != nil {
		return Schedule{}, err
	}

	secondary, err := Choice(ScheduleV1, seed, path, "secondary-settlement",
		[]string{SettlementNormal, SettlementRace})

	if err != nil {
		return Schedule{}, err
	}

	reorgProbe := probe == "true"

	if primary == SettlementNormal && !reorgProbe && secondary == SettlementNormal {
		secondary = SettlementRace
	}

	return Schedule{
		Version:           ScheduleV1,
		Path:              path,
		PrimarySettlement: primary,
		ReorgProbe:        reorgProbe,
		SecondarySettle:   secondary,
	}, nil
}

// Corpus returns the schedules a default corpus run executes.
func Corpus() []Schedule {
	schedules := make([]Schedule, 0, len(DefaultSeeds)*len(Paths))

	for _, seed := range DefaultSeeds {
		for _, path := range Paths {
			built, err := Build(seed, path)

			if err != nil {
				continue
			}

			schedules = append(schedules, built)
		}
	}

	return schedules
}
