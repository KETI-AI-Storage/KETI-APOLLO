// Package policyagent는 정책 평가 규칙을 검증한다.
// Created: 2026-05-22
package policyagent

import (
	"testing"
	"time"
)

func TestRuleBasedEvaluateDecisions(t *testing.T) {
	tests := []struct {
		name   string
		spec   PolicySpec
		action DecisionType
	}{
		{
			name: "target workload missing",
			spec: PolicySpec{
				PolicyType:    "scaling",
				Probability:   90,
				Urgency:       "HIGH",
				PriorityScore: 90,
			},
			action: DecisionReject,
		},
		{
			name: "low probability rejected",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    30,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  90,
			},
			action: DecisionReject,
		},
		{
			name: "proactive high priority approved",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    45,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  84,
			},
			action: DecisionApprove,
		},
		{
			name: "high urgency sufficient priorityScore approved despite sub-proactive probability",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    44,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  84,
			},
			action: DecisionApprove,
		},
		{
			name: "high urgency below priorityScore threshold still delayed",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    44,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  83,
			},
			action: DecisionDelay,
		},
		{
			name: "high urgency high priorityScore but low probability rejected",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    30,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  84,
			},
			action: DecisionReject,
		},
		{
			name: "low priority warning delayed",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    45,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  79,
			},
			action: DecisionDelay,
		},
		{
			name: "critical priority modified",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    45,
				Urgency:        "CRITICAL",
				TargetWorkload: "training-job",
				PriorityScore:  79,
			},
			action: DecisionModify,
		},
		{
			name: "standard threshold approved",
			spec: PolicySpec{
				PolicyType:     "scaling",
				Probability:    50,
				Urgency:        "HIGH",
				TargetWorkload: "training-job",
				PriorityScore:  80,
			},
			action: DecisionApprove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &Policy{Spec: tt.spec}
			decision := RuleBasedEvaluate(policy, 15*time.Second)
			if decision.Action != tt.action {
				t.Fatalf("decision.Action = %q, want %q", decision.Action, tt.action)
			}
		})
	}
}

func TestRuleBasedEvaluateRejectsSameMigrationNode(t *testing.T) {
	policy := &Policy{
		Spec: PolicySpec{
			PolicyType:     "migration",
			Probability:    90,
			Urgency:        "HIGH",
			SourceNode:     "node-a",
			TargetNode:     "node-a",
			TargetWorkload: "training-job",
			PriorityScore:  90,
		},
	}

	decision := RuleBasedEvaluate(policy, 15*time.Second)
	if decision.Action != DecisionReject {
		t.Fatalf("decision.Action = %q, want %q", decision.Action, DecisionReject)
	}
}
