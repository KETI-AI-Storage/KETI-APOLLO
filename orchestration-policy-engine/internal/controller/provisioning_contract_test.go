// Package controller는 provisioning 정책이 orchestrator 계약(storage_class tier)을
// 만족하는 요청을 보내는지 검증한다.
//
// 배경(ISSUE-2 후속): 종전 기본 storageClass "high-throughput"은 orchestrator의
// normalizeTier(L1/L2/L3/S3 + 별칭)가 인식하지 못해 프로비저닝이 400으로 거부됐다.
// 기본값을 "L2"로 바꿔 400→201 전환되는지, override가 그대로 전달되는지 확인한다.
package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/operator"
	"orchestration-policy-engine/internal/policyagent"
)

// normalizeTierForTest는 orchestrator(ai-storage-orchestrator)의 normalizeTier 검증을
// 모사한다. 인식 가능한 tier면 표준 키를, 아니면 빈 문자열을 반환한다.
func normalizeTierForTest(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "l1", "burst", "cache":
		return "L1"
	case "l2", "performance":
		return "L2"
	case "l3", "capacity":
		return "L3"
	case "s3", "archive":
		return "L4"
	default:
		return ""
	}
}

// newProvisioningContractServer는 orchestrator /api/v1/provisioning 핸들러를 모사한다.
// 알 수 없는 storage_class는 400으로 거부하고, 받은 요청 본문을 lastBody에 캡처한다.
func newProvisioningContractServer(t *testing.T) (*httptest.Server, *operator.ProvisioningRequest) {
	t.Helper()
	captured := &operator.ProvisioningRequest{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/provisioning" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req operator.ProvisioningRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*captured = req

		// 필수 필드 검증 (orchestrator 계약과 동일)
		if req.WorkloadName == "" || req.WorkloadNamespace == "" || req.WorkloadType == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"missing required field"}`))
			return
		}
		// storage_class tier 검증 — high-throughput 같은 미인식 값은 400.
		if req.StorageClass != "" && normalizeTierForTest(req.StorageClass) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"invalid storage_class: ` + req.StorageClass + `"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"provisioning_id":"prov-test-1","status":"pending","message":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func newProvisioningPolicy(name string, params map[string]string) *apollov1.OrchestrationPolicy {
	return &apollov1.OrchestrationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: apollov1.OrchestrationPolicySpec{
			PolicyType:      apollov1.PolicyTypeProvisioning,
			Probability:     90,
			Urgency:         apollov1.UrgencyHigh,
			TargetWorkload:  "training-job",
			TargetNamespace: "ai-storage-workloads",
			PriorityScore:   90,
			AutoExecute:     true,
			Reason:          "test provisioning",
			Parameters:      params,
		},
		Status: apollov1.OrchestrationPolicyStatus{Phase: apollov1.PhaseExecuting},
	}
}

// TestExecuteProvisioningSendsAcceptedTier는 storageClass 파라미터가 없을 때
// 기본값이 orchestrator가 수용하는 tier(L2)로 전송되어 201을 받는지 검증한다.
// (회귀 방지: 종전 기본값 high-throughput은 400으로 거부됐다.)
func TestExecuteProvisioningSendsAcceptedTier(t *testing.T) {
	srv, captured := newProvisioningContractServer(t)
	policy := newProvisioningPolicy("prov-default", nil)

	reconciler, _ := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action: policyagent.DecisionApprove,
	})
	reconciler.OperatorClient = operator.NewClient(srv.URL, 5*time.Second)

	if err := reconciler.executeProvisioningPolicy(context.Background(), policy); err != nil {
		t.Fatalf("executeProvisioningPolicy() error = %v (default storageClass must be accepted by orchestrator)", err)
	}

	if captured.StorageClass != defaultProvisioningStorageClass {
		t.Fatalf("captured StorageClass = %q, want %q", captured.StorageClass, defaultProvisioningStorageClass)
	}
	if normalizeTierForTest(captured.StorageClass) == "" {
		t.Fatalf("default StorageClass %q is not an orchestrator-recognized tier", captured.StorageClass)
	}
}

// TestExecuteProvisioningRejectsHighThroughput는 명시적으로 옛 값 high-throughput을
// 주면 orchestrator가 400으로 거부함을 확인한다(계약 모사 검증).
func TestExecuteProvisioningRejectsHighThroughput(t *testing.T) {
	srv, _ := newProvisioningContractServer(t)
	policy := newProvisioningPolicy("prov-bad", map[string]string{"storageClass": "high-throughput"})

	reconciler, _ := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action: policyagent.DecisionApprove,
	})
	reconciler.OperatorClient = operator.NewClient(srv.URL, 5*time.Second)

	err := reconciler.executeProvisioningPolicy(context.Background(), policy)
	if err == nil {
		t.Fatalf("executeProvisioningPolicy() error = nil, want 400 rejection for storageClass=high-throughput")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v, want it to contain status 400", err)
	}
}

// TestExecuteProvisioningHonorsOverride는 파라미터로 준 tier가 그대로 전송되는지 검증한다.
func TestExecuteProvisioningHonorsOverride(t *testing.T) {
	srv, captured := newProvisioningContractServer(t)
	policy := newProvisioningPolicy("prov-override", map[string]string{"storageClass": "L1"})

	reconciler, _ := newPolicyAgentTestReconciler(t, policy, policyagent.Decision{
		Action: policyagent.DecisionApprove,
	})
	reconciler.OperatorClient = operator.NewClient(srv.URL, 5*time.Second)

	if err := reconciler.executeProvisioningPolicy(context.Background(), policy); err != nil {
		t.Fatalf("executeProvisioningPolicy() error = %v", err)
	}
	if captured.StorageClass != "L1" {
		t.Fatalf("captured StorageClass = %q, want override %q", captured.StorageClass, "L1")
	}
}
