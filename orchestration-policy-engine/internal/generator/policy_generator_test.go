package generator

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"orchestration-policy-engine/internal/forecaster"
)

// mkPod builds a minimal Pod for migration target-selection unit tests.
func mkPod(ns, name string, labels map[string]string, images ...string) *corev1.Pod {
	cs := make([]corev1.Container, 0, len(images))
	for i, img := range images {
		cs = append(cs, corev1.Container{Name: fmt.Sprintf("c%d", i), Image: img})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.PodSpec{Containers: cs},
	}
}

// TestMigrationUnsuitableReason locks in which pods the generator may target for
// migration. suitable == true means the pod is a valid target (reason == "").
func TestMigrationUnsuitableReason(t *testing.T) {
	cases := []struct {
		name     string
		pod      *corev1.Pod
		suitable bool
	}{
		{"plain workload", mkPod("ai-storage-workloads", "trainer-xyz", nil, "busybox:1.36"), true},
		{"infra ns argo", mkPod("argo", "anything", nil, "busybox:1.36"), false},
		{"infra ns keti", mkPod("keti", "insight-hub-1", nil, "busybox:1.36"), false},
		{"controller by name", mkPod("ai-storage-workloads", "workflow-controller-1", nil, "busybox"), false},
		{"operator by name", mkPod("ai-storage-workloads", "kueue-operator-x", nil, "busybox"), false},
		{"distroless image", mkPod("ai-storage-workloads", "svc-1", nil, "gcr.io/distroless/static:nonroot"), false},
		// Regression: re-migrating an already-migrated pod duplicates checkpoint-volume
		// and the optimized pod fails to create. The generator must exclude it.
		{"already-migrated product (re-migration guard)",
			mkPod("ai-storage-workloads", "trainer-xyz-migrated-ab12",
				map[string]string{"migration.ai-storage/job": "true"}, "busybox:1.36"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := migrationUnsuitableReason(tc.pod)
			if (reason == "") != tc.suitable {
				t.Fatalf("suitable=%v want=%v (reason=%q)", reason == "", tc.suitable, reason)
			}
		})
	}
}

// TestIsWorkloadNamespace verifies the #10 namespace whitelist gate that keeps the
// orchestrator from targeting platform/infra namespaces (kubeflow, argo, ...).
func TestIsWorkloadNamespace(t *testing.T) {
	saved := workloadNamespaces
	t.Cleanup(func() { workloadNamespaces = saved })
	workloadNamespaces = map[string]bool{"ai-storage-workloads": true}

	if !isWorkloadNamespace("ai-storage-workloads") {
		t.Error("ai-storage-workloads must be a workload namespace")
	}
	for _, ns := range []string{"kubeflow", "argo", "default", ""} {
		if isWorkloadNamespace(ns) {
			t.Errorf("%q must NOT be a workload namespace", ns)
		}
	}
}

// TestResolveTargetWorkloadForRecommendation: migration policies target the Pod name
// (orchestrator API needs a pod); scaling/provisioning/etc. target the Deployment name.
func TestResolveTargetWorkloadForRecommendation(t *testing.T) {
	g := &PolicyGenerator{}
	if got := g.resolveTargetWorkloadForRecommendation(
		forecaster.PolicyRecommendation{PolicyType: "migration"}, "dep-a", "dep-a-pod-xyz"); got != "dep-a-pod-xyz" {
		t.Errorf("migration target = %q, want pod name dep-a-pod-xyz", got)
	}
	for _, pt := range []string{"scaling", "provisioning", "caching", "loadbalance"} {
		if got := g.resolveTargetWorkloadForRecommendation(
			forecaster.PolicyRecommendation{PolicyType: pt}, "dep-a", "dep-a-pod-xyz"); got != "dep-a" {
			t.Errorf("%s target = %q, want deployment name dep-a", pt, got)
		}
	}
}
