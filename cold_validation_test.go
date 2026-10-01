package main

import "testing"

func TestReviewColdModelValidationIgnoresUnrelatedOldHistory(t *testing.T) {
	recent := pooledContrastSegments(24)
	base, err := fitQuotaWeights(recent, nil, 1800000100, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	history := []quotaSegment{}
	for i := 0; i < 180; i++ {
		history = append(history, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: int64(10 + i/60), RegimeResetAt: int64(1799900000 + i/60*10000), StartAt: 1799900000 + int64(i), EndAt: 1799900000 + int64(i), DP: 1, BoundaryWeight: 1, Features: []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: 1000000}}})
	}
	full, err := fitQuotaWeights(append(history, recent...), nil, 1800000100, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []weightFit{base, full} {
		for _, p := range f.PooledModels {
			if p.Model == "gpt-6.1-sol" {
				t.Logf("n=%d applied=%t reason=%s training=%d validation=%d", f.SegmentCount, p.Applied, p.Evidence.Reason, p.Evidence.TrainingSegments, p.Evidence.ValidationSegments)
				if !p.Applied {
					t.Error("irrelevant earlier history must not prevent validating recent identifiable contrasts")
				}
			}
		}
	}
}

func TestPooledValidationKeepsTimestampTiesOutOfTraining(t *testing.T) {
	segments := pooledContrastSegments(24)
	// The ninth target exposure is the target-based cutoff. Another target and
	// a background observation at that same time must both remain in validation.
	cutoff := segments[17].EndAt
	segments[15].EndAt, segments[16].EndAt = cutoff, cutoff
	evidence := pooledValidation("gpt-6.1-sol", segments, nil, defaultWeightLearnerOptions())
	if evidence.TrainingSegments != 15 || evidence.ValidationSegments != 5 || !evidence.Accepted {
		t.Fatalf("timestamp group leaked into training: %+v", evidence)
	}
	for _, n := range []int{8, 18} {
		short := pooledValidation("gpt-6.1-sol", pooledContrastSegments(n), nil, defaultWeightLearnerOptions())
		if short.Accepted {
			t.Fatalf("too few target validation observations were accepted: %+v", short)
		}
	}
}

func TestPooledValidationAcceptsCandidateImprovingOutOfSampleError(t *testing.T) {
	segments := pooledContrastSegments(24)
	for i := range segments {
		segments[i].Features = append(segments[i].Features, segmentFeature{
			Model: weightReferenceModel, Type: "input", Tokens: 5000000,
		})
		segments[i].DP += 5.0
	}
	evidence := pooledValidation("gpt-6.1-sol", segments, nil, defaultWeightLearnerOptions())
	if !evidence.Accepted || evidence.Reason != "validation_improved" {
		t.Fatalf("expected accepted validation in mixed workload, got: %+v", evidence)
	}
	if evidence.CandidateMAE >= evidence.PriorMAE {
		t.Fatalf("expected CandidateMAE < PriorMAE: %+v", evidence)
	}
}
