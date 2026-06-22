/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"strconv"

	apollov1 "orchestration-policy-engine/api/v1"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// annMigration404Streak는 연속 마이그레이션 상태 404 횟수(오케스트레이터 재시작 등 감지).
	annMigration404Streak = "policy.apollo.keti.re.kr/migration-404-streak"
	// annMigrationPollErrStreak는 연속 마이그레이션 폴링(네트워크·5xx·디코딩) 실패 횟수.
	annMigrationPollErrStreak = "policy.apollo.keti.re.kr/migration-poll-error-streak"
	// annSync404Streak는 동기형 정책 검증 GET의 연속 404 횟수.
	annSync404Streak = "policy.apollo.keti.re.kr/sync-verify-404-streak"
	// annSyncGETErrStreak는 동기형 정책 검증 GET의 연속 오류 횟수.
	annSyncGETErrStreak = "policy.apollo.keti.re.kr/sync-verify-get-error-streak"
	// annExecStartErrStreak는 Approved 단계에서 실행 시작(API 호출) 연속 실패 횟수.
	annExecStartErrStreak = "policy.apollo.keti.re.kr/exec-start-error-streak"
	// annRollbackStatus는 실패 시 오케스트레이터 보상(삭제) API 시도 결과(succeeded|failed|skipped).
	annRollbackStatus = "policy.apollo.keti.re.kr/rollback-status"
	// annRollbackDetail은 rollback 시도 상세(짧은 문자열).
	annRollbackDetail = "policy.apollo.keti.re.kr/rollback-detail"
	// annRetryFromFailure가 "true"이면 Failed 상태를 Pending으로 되돌려 재실행한다.
	annRetryFromFailure = "policy.apollo.keti.re.kr/retry-from-failure"
)

func annInt(p *apollov1.OrchestrationPolicy, key string) int {
	if p.Annotations == nil {
		return 0
	}
	s := p.Annotations[key]
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (r *OrchestrationPolicyReconciler) patchAnnotationInt(ctx context.Context, p *apollov1.OrchestrationPolicy, key string, value int) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apollov1.OrchestrationPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), latest); err != nil {
			return err
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		if value <= 0 {
			delete(latest.Annotations, key)
		} else {
			latest.Annotations[key] = strconv.Itoa(value)
		}
		if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		p.Annotations = latest.Annotations
		p.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// patchAnnotationString은 문자열 annotation을 설정하거나 value가 비어 있으면 제거한다.
func (r *OrchestrationPolicyReconciler) patchAnnotationString(ctx context.Context, p *apollov1.OrchestrationPolicy, key, value string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apollov1.OrchestrationPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), latest); err != nil {
			return err
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		if value == "" {
			delete(latest.Annotations, key)
		} else {
			latest.Annotations[key] = value
		}
		if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		p.Annotations = latest.Annotations
		p.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

func (r *OrchestrationPolicyReconciler) clearMigrationPollAnnotations(ctx context.Context, p *apollov1.OrchestrationPolicy) error {
	return r.clearAnnotations(ctx, p, annMigration404Streak, annMigrationPollErrStreak)
}

func (r *OrchestrationPolicyReconciler) clearSyncVerifyAnnotations(ctx context.Context, p *apollov1.OrchestrationPolicy) error {
	return r.clearAnnotations(ctx, p, annSync404Streak, annSyncGETErrStreak)
}

func (r *OrchestrationPolicyReconciler) clearAnnotations(ctx context.Context, p *apollov1.OrchestrationPolicy, keys ...string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apollov1.OrchestrationPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			return nil
		}
		base := latest.DeepCopy()
		changed := false
		for _, k := range keys {
			if _, ok := latest.Annotations[k]; ok {
				delete(latest.Annotations, k)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		p.Annotations = latest.Annotations
		p.ResourceVersion = latest.ResourceVersion
		return nil
	})
}
