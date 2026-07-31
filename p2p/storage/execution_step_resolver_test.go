package storage

import (
	"testing"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutableObservationUsesPersistedObservedStatus(t *testing.T) {
	const (
		owner         = "@owner:example.test"
		observationID = "11111111-1111-4111-8111-111111111111"
		targetID      = "22222222-2222-4222-8222-222222222222"
		digest        = coreexecution.Digest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	)
	ref := coreexecution.TargetObservationRef{
		ObservationID:     observationID,
		TargetID:          targetID,
		TargetRevision:    1,
		ObservationDigest: digest,
	}
	stage := coreexecution.RunStage{TargetID: targetID, TargetRevision: 1}
	record := TargetObservationRecord{
		OwnerID:       owner,
		ObservationID: observationID,
		Status:        "observed",
		Observation: coreexecution.TargetObservation{
			TargetID:       targetID,
			TargetRevision: 1,
			State:          "ready",
			Digest:         digest,
		},
	}
	if !executableObservation(record, owner, ref, stage) {
		t.Fatal("a complete persisted observation was rejected")
	}

	for name, mutate := range map[string]func(*TargetObservationRecord){
		"failed_record": func(v *TargetObservationRecord) { v.Status = "failed" },
		"partial":       func(v *TargetObservationRecord) { v.Observation.Partial = true },
		"stale":         func(v *TargetObservationRecord) { v.Observation.Stale = true },
		"not_ready":     func(v *TargetObservationRecord) { v.Observation.State = "unavailable" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := record
			mutate(&candidate)
			if executableObservation(candidate, owner, ref, stage) {
				t.Fatal("non-executable observation was accepted")
			}
		})
	}
}
