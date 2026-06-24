package generator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestFindTargetWorkload_PicksSuitableRunningPod is an integration test (fake clientset)
// for the pod-selection logic: among the pods on a node it must pick the one suitable,
// Running, whitelisted workload and skip the distractors:
//   - infra-namespace pod (argo)            -> skipped (#10 whitelist / infra)
//   - already-migrated product pod          -> skipped (re-migration guard)
//   - Pending (not Running) pod             -> skipped
//   - the plain Running workload            -> chosen
func TestFindTargetWorkload_PicksSuitableRunningPod(t *testing.T) {
	saved := workloadNamespaces
	t.Cleanup(func() { workloadNamespaces = saved })
	workloadNamespaces = map[string]bool{"ai-storage-workloads": true}

	const node = "node-x"
	onNode := func(ns, name string, phase corev1.PodPhase, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			Spec: corev1.PodSpec{
				NodeName:   node,
				Containers: []corev1.Container{{Name: "c", Image: "busybox:1.36"}},
			},
			Status: corev1.PodStatus{Phase: phase},
		}
	}

	infra := onNode("argo", "workflow-runner", corev1.PodRunning, nil)
	migrated := onNode("ai-storage-workloads", "z-migrated-1", corev1.PodRunning,
		map[string]string{"migration.ai-storage/job": "true"})
	pending := onNode("ai-storage-workloads", "warmup-1", corev1.PodPending, nil)
	good := onNode("ai-storage-workloads", "trainer-1", corev1.PodRunning, nil)

	client := fake.NewSimpleClientset(infra, migrated, pending, good)
	g := &PolicyGenerator{kubeClient: client}

	dep, podName, ns := g.findTargetWorkload(context.Background(), node)
	if podName != "trainer-1" || ns != "ai-storage-workloads" {
		t.Fatalf("findTargetWorkload = (dep=%q pod=%q ns=%q); want pod=trainer-1 ns=ai-storage-workloads", dep, podName, ns)
	}
}

// TestFindTargetWorkload_NoSuitablePod returns empty when nothing on the node qualifies.
func TestFindTargetWorkload_NoSuitablePod(t *testing.T) {
	saved := workloadNamespaces
	t.Cleanup(func() { workloadNamespaces = saved })
	workloadNamespaces = map[string]bool{"ai-storage-workloads": true}

	const node = "node-y"
	infraOnly := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "node-exporter"},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(infraOnly)
	g := &PolicyGenerator{kubeClient: client}

	if _, podName, _ := g.findTargetWorkload(context.Background(), node); podName != "" {
		t.Fatalf("expected no target, got pod=%q", podName)
	}
}
