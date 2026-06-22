#!/usr/bin/env bash
# OPE 5종(provisioning, caching, loadbalance, preemption, migration) 정책 적용·상태 수집
set -euo pipefail
NS="ope-model-verify"
APOLLO_NS="apollo"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
verify_date="20260424"
POD="" SRC="" TGT=""

pick_target_node() {
  local src="$1"
  if [[ "$src" == "ai-storage-worker-01" ]]; then
    echo "gpu-server-03"
  else
    echo "ai-storage-worker-01"
  fi
}

kubectl apply -f "${SCRIPT_DIR}/verify-workloads/pytorch-pvc-inference-verify.yaml"
kubectl rollout status deployment/pytorch-inference-verify -n "$NS" --timeout=300s
kubectl wait --for=condition=ready pod -l app=pytorch-inference-verify -n "$NS" --timeout=180s
POD="$(kubectl get pod -n "$NS" -l app=pytorch-inference-verify -o jsonpath='{.items[0].metadata.name}')"
SRC="$(kubectl get pod -n "$NS" "$POD" -o jsonpath='{.spec.nodeName}')"
TGT="$(pick_target_node "$SRC")"
export POD SRC TGT NS APOLLO_NS

echo "VERIFY_POD=$POD VERIFY_SRC=$SRC VERIFY_TGT=$TGT" >&2

# 정책 CR (고유 이름)
for kind in provisioning caching loadbalance preemption migration; do
  NAME="ope-e2e-${kind}-${verify_date}"
  kubectl delete orchestrationpolicy "$NAME" -n "$APOLLO_NS" --ignore-not-found=true
done

# --- provisioning
kubectl apply -f - <<EOF
apiVersion: apollo.keti.re.kr/v1
kind: OrchestrationPolicy
metadata:
  name: ope-e2e-provisioning-${verify_date}
  namespace: ${APOLLO_NS}
  labels:
    ope.e2e: "20260424"
spec:
  policyType: provisioning
  probability: 92
  urgency: HIGH
  resourceType: GPU
  targetNode: ${SRC}
  targetWorkload: pytorch-inference-verify
  targetNamespace: ${NS}
  reason: "E2E: 프로비저닝 — ope-model-verify PyTorch PVC 워크로드"
  horizon: 15
  autoExecute: true
  priorityScore: 80
  parameters: {}
---
apiVersion: apollo.keti.re.kr/v1
kind: OrchestrationPolicy
metadata:
  name: ope-e2e-caching-${verify_date}
  namespace: ${APOLLO_NS}
  labels:
    ope.e2e: "20260424"
spec:
  policyType: caching
  probability: 90
  urgency: HIGH
  resourceType: STORAGE_IO
  targetNode: ${SRC}
  targetWorkload: pytorch-inference-verify
  targetNamespace: ${NS}
  reason: "E2E: 캐싱 — TargetWorkload-pvc=pytorch-inference-verify-pvc"
  horizon: 12
  autoExecute: true
  priorityScore: 78
  parameters: {}
---
apiVersion: apollo.keti.re.kr/v1
kind: OrchestrationPolicy
metadata:
  name: ope-e2e-loadbalance-${verify_date}
  namespace: ${APOLLO_NS}
  labels:
    ope.e2e: "20260424"
spec:
  policyType: loadbalance
  probability: 88
  urgency: MEDIUM
  resourceType: NETWORK
  targetNode: ${TGT}
  reason: "E2E: 로드밸런싱 — 대상 노드 ${TGT}"
  horizon: 10
  autoExecute: true
  priorityScore: 72
  parameters: {}
---
apiVersion: apollo.keti.re.kr/v1
kind: OrchestrationPolicy
metadata:
  name: ope-e2e-preemption-${verify_date}
  namespace: ${APOLLO_NS}
  labels:
    ope.e2e: "20260424"
spec:
  policyType: preemption
  probability: 85
  urgency: HIGH
  resourceType: CPU
  targetWorkload: ${POD}
  targetNamespace: ${NS}
  targetNode: ${SRC}
  reason: "E2E: 선점 — 검증 Pod ${POD} (namespace=${NS})"
  horizon: 10
  autoExecute: true
  priorityScore: 88
  parameters: {}
---
apiVersion: apollo.keti.re.kr/v1
kind: OrchestrationPolicy
metadata:
  name: ope-e2e-migration-${verify_date}
  namespace: ${APOLLO_NS}
  labels:
    ope.e2e: "20260424"
spec:
  policyType: migration
  probability: 95
  urgency: HIGH
  resourceType: GPU
  targetNode: ${TGT}
  sourceNode: ${SRC}
  targetWorkload: ${POD}
  targetNamespace: ${NS}
  reason: "E2E: 마이그레이션 — PyTorch PVC 워크로드 Pod (preserve PV)"
  horizon: 20
  autoExecute: true
  priorityScore: 90
  parameters:
    preservePV: "true"
EOF

echo "Applied 5 OrchestrationPolicies. Wait for terminal phases (max ~25m)..." >&2
names=(
  "ope-e2e-provisioning-${verify_date}"
  "ope-e2e-caching-${verify_date}"
  "ope-e2e-loadbalance-${verify_date}"
  "ope-e2e-preemption-${verify_date}"
  "ope-e2e-migration-${verify_date}"
)
end=$((SECONDS + 1500))
while [[ $SECONDS -lt $end ]]; do
  all_done=1
  for n in "${names[@]}"; do
    ph=$(kubectl get orchestrationpolicy "$n" -n "$APOLLO_NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "$ph" != "Completed" && "$ph" != "Failed" ]]; then
      all_done=0
      break
    fi
  done
  if [[ $all_done -eq 1 ]]; then
    echo "All policies reached terminal state." >&2
    break
  fi
  sleep 10
done

OPE_POD=$(kubectl get pods -n "$APOLLO_NS" -l app=orchestration-policy-engine -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
ORCH_POD=$(kubectl get pods -n kube-system -l app=ai-storage-orchestrator -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
echo "OPE_POD=${OPE_POD} ORCH_POD=${ORCH_POD}" >&2

if [[ -n "$OPE_POD" ]]; then
  kubectl logs -n "$APOLLO_NS" "$OPE_POD" --since=30m 2>&1 | grep -E 'ope-e2e-(provisioning|caching|loadbalance|preemption|migration)-20260424' | grep -E 'Operator response received|Patching policy status\.result|Patched policy status\.result|execution hung|Result backfill' | tail -200 || true
fi
if [[ -n "$ORCH_POD" ]]; then
  kubectl logs -n kube-system "$ORCH_POD" --since=30m 2>&1 | tail -120 || true
fi

printf '\n---- status table ----\n'
for n in "${names[@]}"; do
  kubectl get orchestrationpolicy "$n" -n "$APOLLO_NS" -o jsonpath="NAME=$n phase={.status.phase} result={.status.result} msg={.status.message}" 2>/dev/null || echo "NAME=$n (missing)"
  echo ""
done
