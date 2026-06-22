// Package controller는 executionMode별 정책 상태 전이를 검증한다.
//
// Author: 미정 <<unknown@example.com>>
// Created: 2026-06-19
package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apollov1 "orchestration-policy-engine/api/v1"
)

func TestHandleApprovedPolicy_OnTargetCreatedMovesToWaiting(t *testing.T) {
	policy := newExecutionModePolicy("waiting-policy")
	policy.Status.Phase = apollov1.PhaseApproved
	policy.Spec.ExecutionMode = apollov1.ExecutionModeOnTargetCreated

	reconciler, k8sClient := newExecutionModeTestReconciler(t, policy)
	if _, err := reconciler.handleApprovedPolicy(context.Background(), policy); err != nil {
		t.Fatalf("handleApprovedPolicy() error = %v", err)
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhaseWaitingForTarget {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhaseWaitingForTarget)
	}
	if latest.Status.Consumed {
		t.Fatalf("latest.Status.Consumed = true, want false")
	}
}

func TestReconcile_OnTargetCreatedConsumesMatchedTargetOnce(t *testing.T) {
	policy := newExecutionModePolicy("consume-once-policy")
	policy.Status.Phase = apollov1.PhaseWaitingForTarget
	policy.Spec.ExecutionMode = apollov1.ExecutionModeOnTargetCreated
	policy.Spec.TargetRunID = "run-123"
	policy.Spec.TargetStage = "train"
	policy.Spec.Selector = map[string]string{"app": "trainer"}
	policy.CreationTimestamp = metav1.NewTime(time.Now().Add(-1 * time.Minute))

	target := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "trainer-pod-new",
			Namespace:         "default",
			UID:               "pod-uid-1",
			CreationTimestamp: metav1.NewTime(time.Now()),
			Labels: map[string]string{
				"app": "trainer",
			},
			Annotations: map[string]string{
				"run_id":       "run-123",
				"target_stage": "train",
			},
		},
	}

	reconciler, k8sClient := newExecutionModeTestReconciler(t, policy, target)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if !latest.Status.Consumed {
		t.Fatalf("latest.Status.Consumed = false, want true")
	}
	if len(latest.Status.AppliedTargets) != 1 {
		t.Fatalf("len(latest.Status.AppliedTargets) = %d, want 1", len(latest.Status.AppliedTargets))
	}
	if latest.Status.AppliedTargets[0].UID != "pod-uid-1" {
		t.Fatalf("appliedTargets[0].uid = %q, want %q", latest.Status.AppliedTargets[0].UID, "pod-uid-1")
	}
}

func TestHandleAutoApprove_ContinuousDisallowedForMigration(t *testing.T) {
	policy := newExecutionModePolicy("continuous-migration-policy")
	policy.Status.Phase = apollov1.PhasePending
	policy.Spec.AutoExecute = true
	policy.Spec.PolicyType = apollov1.PolicyTypeMigration
	policy.Spec.ExecutionMode = apollov1.ExecutionModeContinuous

	reconciler, k8sClient := newExecutionModeTestReconciler(t, policy)
	result, err := reconciler.handleAutoApprove(context.Background(), policy)
	if err != nil {
		t.Fatalf("handleAutoApprove() error = %v", err)
	}
	if result.Requeue {
		t.Fatalf("result.Requeue = true, want false")
	}

	latest := getPolicyForTest(t, k8sClient, policy)
	if latest.Status.Phase != apollov1.PhaseRejected {
		t.Fatalf("latest.Status.Phase = %q, want %q", latest.Status.Phase, apollov1.PhaseRejected)
	}
}

func newExecutionModePolicy(name string) *apollov1.OrchestrationPolicy {
	return &apollov1.OrchestrationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: apollov1.OrchestrationPolicySpec{
			PolicyType:      apollov1.PolicyTypeScaling,
			Probability:     90,
			Urgency:         apollov1.UrgencyHigh,
			TargetWorkload:  "training-job",
			TargetNamespace: "default",
			Reason:          "execution mode test",
			AutoExecute:     true,
			ExecutionMode:   apollov1.ExecutionModeImmediate,
		},
	}
}

func newExecutionModeTestReconciler(
	t *testing.T,
	policy *apollov1.OrchestrationPolicy,
	extraObjects ...client.Object,
) (*OrchestrationPolicyReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := apollov1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(apollov1) error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(appsv1) error = %v", err)
	}

	objects := []client.Object{policy}
	objects = append(objects, extraObjects...)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apollov1.OrchestrationPolicy{}).
		WithObjects(objects...).
		Build()

	return &OrchestrationPolicyReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}, k8sClient
}
