// Package controller는 OrchestrationPolicy Reconciler의 Policy Agent 연동을 검증한다.
//
// Author: 미정 <<unknown@example.com>>
// Created: 2026-05-22
package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/policyagent"
)

type fakePolicyAgent struct {
	decision policyagent.Decision
	err      error
}

func (f fakePolicyAgent) Evaluate(ctx context.Context, policy *policyagent.Policy) (policyagent.Decision, error) {
	return f.decision, f.err
}

// TestHandleAutoApproveWithPolicyAgentApprove는 approve decision이 Approved 상태로 전이되는지 검증한다.
func TestHandleAutoApproveWithPolicyAgentApprove(t *testing.T) {
	policy := newPendingPolicy("approve-policy")
	reconciler, k8sClient := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action: policyagent.DecisionApprove,
		Reason: "approval threshold passed",
	})

	result, err := reconciler.handleAutoApprove(context.Background(), policy)
	if err != nil {
		t.Fatalf("handleAutoApprove() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result.Requeue = false, want true")
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhaseApproved {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhaseApproved)
	}
}

// TestHandleAutoApproveWithPolicyAgentDelay는 delay decision이 Pending 상태와 재평가 시간을 유지하는지 검증한다.
func TestHandleAutoApproveWithPolicyAgentDelay(t *testing.T) {
	policy := newPendingPolicy("delay-policy")
	reconciler, k8sClient := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action:       policyagent.DecisionDelay,
		Reason:       "probability is not stable yet",
		RequeueAfter: 15 * time.Second,
	})

	result, err := reconciler.handleAutoApprove(context.Background(), policy)
	if err != nil {
		t.Fatalf("handleAutoApprove() error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("result.RequeueAfter = %v, want 15s", result.RequeueAfter)
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhasePending {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhasePending)
	}
}

// TestHandleAutoApproveWithPolicyAgentReject는 reject decision이 Rejected 상태로 종료되는지 검증한다.
func TestHandleAutoApproveWithPolicyAgentReject(t *testing.T) {
	policy := newPendingPolicy("reject-policy")
	reconciler, k8sClient := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action: policyagent.DecisionReject,
		Reason: "targetWorkload is required",
	})

	result, err := reconciler.handleAutoApprove(context.Background(), policy)
	if err != nil {
		t.Fatalf("handleAutoApprove() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %+v, want no requeue", result)
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhaseRejected {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhaseRejected)
	}
}

// TestHandleAutoApproveWithPolicyAgentModify는 modify decision이 spec 보정 후 Approved로 전이되는지 검증한다.
func TestHandleAutoApproveWithPolicyAgentModify(t *testing.T) {
	policy := newPendingPolicy("modify-policy")
	modified := toPolicyAgentPolicy(policy).Spec
	modified.PriorityScore = 80
	modified.Parameters = map[string]string{"policyAgentModified": "true"}
	reconciler, k8sClient := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action:       policyagent.DecisionModify,
		Reason:       "critical policy priorityScore raised before approval",
		ModifiedSpec: &modified,
	})

	result, err := reconciler.handleAutoApprove(context.Background(), policy)
	if err != nil {
		t.Fatalf("handleAutoApprove() error = %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result.Requeue = false, want true")
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhaseApproved {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhaseApproved)
	}
	if latest.Spec.PriorityScore != 80 {
		t.Fatalf("latest.Spec.PriorityScore = %d, want 80", latest.Spec.PriorityScore)
	}
	if latest.Spec.Parameters["policyAgentModified"] != "true" {
		t.Fatalf("latest.Spec.Parameters[policyAgentModified] = %q, want true", latest.Spec.Parameters["policyAgentModified"])
	}
}

func newPendingPolicy(name string) *apollov1.OrchestrationPolicy {
	return &apollov1.OrchestrationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: apollov1.OrchestrationPolicySpec{
			PolicyType:     apollov1.PolicyTypeScaling,
			Probability:    90,
			Urgency:        apollov1.UrgencyHigh,
			TargetWorkload: "training-job",
			PriorityScore:  90,
			AutoExecute:    true,
		},
		Status: apollov1.OrchestrationPolicyStatus{
			Phase: apollov1.PhasePending,
		},
	}
}

func newPolicyAgentTestReconciler(
	t *testing.T,
	policy *apollov1.OrchestrationPolicy,
	decision policyagent.Decision,
) (*OrchestrationPolicyReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := apollov1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apollov1.OrchestrationPolicy{}).
		WithObjects(policy).
		Build()

	return &OrchestrationPolicyReconciler{
		Client:                  k8sClient,
		Scheme:                  scheme,
		PolicyAgentClient:       fakePolicyAgent{decision: decision},
		PolicyAgentEnabled:      true,
		PolicyAgentRequeueAfter: 30 * time.Second,
	}, k8sClient
}

func getPolicyForTest(t *testing.T, k8sClient client.Client, policy *apollov1.OrchestrationPolicy) *apollov1.OrchestrationPolicy {
	t.Helper()

	latest := &apollov1.OrchestrationPolicy{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(policy), latest); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return latest
}
