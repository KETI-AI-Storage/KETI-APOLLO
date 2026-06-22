/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/operator"
	ctrl "sigs.k8s.io/controller-runtime"
)

// handleSyncOperatorAfterGrace는 grace 경과 후 Operator GET으로 동기형 정책의 터미널 상태를 검증한다.
func (r *OrchestrationPolicyReconciler) handleSyncOperatorAfterGrace(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	opID, ok := extractResultResourceID(policy.Status.Result)
	if !ok {
		return r.markPolicyFailed(ctx, policy, "sync policy: cannot parse operator id from status.result")
	}

	switch policy.Spec.PolicyType {
	case apollov1.PolicyTypeScaling:
		resp, code, err := r.OperatorClient.GetAutoscaling(ctx, opID)
		if fail, reason, e := r.onSyncGETOutcome(ctx, policy, code, err); e != nil {
			return ctrl.Result{}, e
		} else if fail {
			return r.markPolicyFailed(ctx, policy, reason)
		}
		if err != nil || code != http.StatusOK {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return r.syncDecisionAutoscaling(ctx, policy, resp)

	case apollov1.PolicyTypeProvisioning:
		resp, code, err := r.OperatorClient.GetProvisioning(ctx, opID)
		if fail, reason, e := r.onSyncGETOutcome(ctx, policy, code, err); e != nil {
			return ctrl.Result{}, e
		} else if fail {
			return r.markPolicyFailed(ctx, policy, reason)
		}
		if err != nil || code != http.StatusOK {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return r.syncDecisionProvisioning(ctx, policy, resp)

	case apollov1.PolicyTypeCaching:
		resp, code, err := r.OperatorClient.GetCaching(ctx, opID)
		if fail, reason, e := r.onSyncGETOutcome(ctx, policy, code, err); e != nil {
			return ctrl.Result{}, e
		} else if fail {
			return r.markPolicyFailed(ctx, policy, reason)
		}
		if err != nil || code != http.StatusOK {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return r.syncDecisionCaching(ctx, policy, resp)

	case apollov1.PolicyTypeLoadbalance:
		resp, code, err := r.OperatorClient.GetLoadbalancing(ctx, opID)
		if fail, reason, e := r.onSyncGETOutcome(ctx, policy, code, err); e != nil {
			return ctrl.Result{}, e
		} else if fail {
			return r.markPolicyFailed(ctx, policy, reason)
		}
		if err != nil || code != http.StatusOK {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return r.syncDecisionLoadbalance(ctx, policy, resp)

	case apollov1.PolicyTypePreemption:
		resp, code, err := r.OperatorClient.GetPreemption(ctx, opID)
		if fail, reason, e := r.onSyncGETOutcome(ctx, policy, code, err); e != nil {
			return ctrl.Result{}, e
		} else if fail {
			return r.markPolicyFailed(ctx, policy, reason)
		}
		if err != nil || code != http.StatusOK {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return r.syncDecisionPreemption(ctx, policy, resp)

	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *OrchestrationPolicyReconciler) onSyncGETOutcome(ctx context.Context, p *apollov1.OrchestrationPolicy, code int, err error) (failNow bool, reason string, patchErr error) {
	if err == nil && code == http.StatusOK {
		patchErr = r.clearSyncVerifyAnnotations(ctx, p)
		return false, "", patchErr
	}
	max404 := r.SyncMaxConsecutive404
	if max404 <= 0 {
		max404 = 5
	}
	maxErr := r.SyncMaxConsecutiveGETErrors
	if maxErr <= 0 {
		maxErr = 15
	}

	if code == http.StatusNotFound {
		n := annInt(p, annSync404Streak) + 1
		if patchErr = r.patchAnnotationInt(ctx, p, annSync404Streak, n); patchErr != nil {
			return false, "", patchErr
		}
		if n >= max404 {
			return true, fmt.Sprintf("sync operator resource not found (404) %d times in a row", n), nil
		}
		return false, "", nil
	}

	n := annInt(p, annSyncGETErrStreak) + 1
	if patchErr = r.patchAnnotationInt(ctx, p, annSyncGETErrStreak, n); patchErr != nil {
		return false, "", patchErr
	}
	if n >= maxErr {
		return true, fmt.Sprintf("sync verify GET errors exceeded (%d): %v", maxErr, err), nil
	}
	return false, "", nil
}

func (r *OrchestrationPolicyReconciler) syncDecisionAutoscaling(ctx context.Context, policy *apollov1.OrchestrationPolicy, resp *operator.AutoscalingResponse) (ctrl.Result, error) {
	s := strings.ToLower(strings.TrimSpace(resp.Status))
	switch s {
	case "failed":
		return r.markPolicyFailed(ctx, policy, resp.Message)
	case "active", "inactive":
		return r.markPolicyCompleted(ctx, policy)
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *OrchestrationPolicyReconciler) syncDecisionProvisioning(ctx context.Context, policy *apollov1.OrchestrationPolicy, resp *operator.ProvisioningResponse) (ctrl.Result, error) {
	s := strings.ToLower(strings.TrimSpace(resp.Status))
	switch s {
	case "failed":
		return r.markPolicyFailed(ctx, policy, resp.Message)
	case "ready":
		return r.markPolicyCompleted(ctx, policy)
	case "pending", "creating":
		// GET 200으로 리소스 존재가 확인된 상태 — strict 모드가 아니면 E2E·시뮬레이터(pending 고정)와 호환
		if !r.ProvisioningStrictReadyOnly {
			return r.markPolicyCompleted(ctx, policy)
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *OrchestrationPolicyReconciler) syncDecisionCaching(ctx context.Context, policy *apollov1.OrchestrationPolicy, resp *operator.CachingResponse) (ctrl.Result, error) {
	s := strings.ToLower(strings.TrimSpace(resp.Status))
	switch s {
	case "failed":
		return r.markPolicyFailed(ctx, policy, resp.Message)
	case "active", "inactive":
		return r.markPolicyCompleted(ctx, policy)
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *OrchestrationPolicyReconciler) syncDecisionLoadbalance(ctx context.Context, policy *apollov1.OrchestrationPolicy, resp *operator.LoadbalanceResponse) (ctrl.Result, error) {
	s := strings.ToLower(strings.TrimSpace(resp.Status))
	switch s {
	case "failed", "cancelled":
		msg := resp.Message
		if s == "cancelled" && msg == "" {
			msg = "loadbalancing cancelled"
		}
		return r.markPolicyFailed(ctx, policy, msg)
	case "completed":
		return r.markPolicyCompleted(ctx, policy)
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

func (r *OrchestrationPolicyReconciler) syncDecisionPreemption(ctx context.Context, policy *apollov1.OrchestrationPolicy, resp *operator.PreemptionResponse) (ctrl.Result, error) {
	s := strings.ToLower(strings.TrimSpace(resp.Status))
	switch s {
	case "failed":
		return r.markPolicyFailed(ctx, policy, resp.Message)
	case "completed":
		return r.markPolicyCompleted(ctx, policy)
	default:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}
