/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apollov1 "orchestration-policy-engine/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// attemptOperatorRollback은 실패 처리 직전 status.result에 남아 있는 오퍼레이터 리소스 ID로
// DELETE API가 정의된 정책 타입에 한해 보상 삭제를 최대 3회 시도한다.
//
// Parameters:
// - resultBeforeFailure: markPolicyFailed가 status.result를 덮어쓰기 전 문자열
//
// Returns:
// - status: succeeded | failed | skipped
// - detail: skipped/failed 사유(짧은 문자열)
func (r *OrchestrationPolicyReconciler) attemptOperatorRollback(ctx context.Context, policy *apollov1.OrchestrationPolicy, resultBeforeFailure string) (status string, detail string) {
	if r.OperatorClient == nil {
		return "skipped", "OperatorClient nil"
	}
	id, ok := extractResultResourceID(resultBeforeFailure)
	if !ok || id == "" {
		return "skipped", "no operator resource id in status.result"
	}

	var doDelete func(context.Context, string) error
	switch policy.Spec.PolicyType {
	case apollov1.PolicyTypeScaling:
		doDelete = r.OperatorClient.DeleteAutoscaling
	case apollov1.PolicyTypeProvisioning:
		doDelete = r.OperatorClient.DeleteProvisioning
	case apollov1.PolicyTypeCaching:
		doDelete = r.OperatorClient.DeleteCaching
	case apollov1.PolicyTypeLoadbalance:
		doDelete = r.OperatorClient.DeleteLoadbalancing
	case apollov1.PolicyTypeMigration, apollov1.PolicyTypePreemption:
		return "skipped", "orchestrator has no DELETE /api/v1 for this policy type"
	default:
		return "skipped", "no rollback delete mapped for policy type"
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		if err := doDelete(ctx, id); err == nil {
			return "succeeded", ""
		} else {
			lastErr = err
		}
	}
	return "failed", fmt.Sprintf("%v", lastErr)
}

// patchRollbackMeta는 rollback 시도 결과를 metadata annotation에 한 번에 기록한다.
func (r *OrchestrationPolicyReconciler) patchRollbackMeta(ctx context.Context, policy *apollov1.OrchestrationPolicy, status, detail string) error {
	base := policy.DeepCopy()
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	policy.Annotations[annRollbackStatus] = status
	if detail == "" {
		delete(policy.Annotations, annRollbackDetail)
	} else {
		policy.Annotations[annRollbackDetail] = detail
	}
	return r.Patch(ctx, policy, client.MergeFrom(base))
}
