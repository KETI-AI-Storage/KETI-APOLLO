/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apollov1 "orchestration-policy-engine/api/v1"
)

// TerminatedPolicyGC는 종료(Completed/Failed/Rejected) OrchestrationPolicy CR을
// 주기적으로 정리한다. 종료 CR이 정리되지 않으면 etcd/컨트롤러에 무한 누적되므로
// (분당 수개 → 일 수천 개), 종료 후 TTL이 지난 CR을 삭제해 누적을 멈춘다.
//
// manager.Runnable / LeaderElectionRunnable로 등록되어 leader에서만 실행된다
// (중복 삭제 방지). PolicyGenerator와 동일한 생명주기 패턴을 따른다.
type TerminatedPolicyGC struct {
	// Client는 CR 조회/삭제에 사용한다(reconciler와 동일한 mgr.GetClient()).
	Client client.Client
	// Namespace는 정리 대상 OrchestrationPolicy 네임스페이스다.
	Namespace string
	// Interval은 정리 주기다.
	Interval time.Duration
	// TTL은 종료 시각 이후 CR을 보존하는 시간이다(초과 시 삭제).
	TTL time.Duration
	// BatchMax는 한 주기당 삭제 건수 상한이다(0 이하면 무제한).
	BatchMax int
}

// Start는 manager.Runnable 인터페이스를 구현한다.
// 시작 시 즉시 한 번 정리하고, 이후 Interval마다 반복한다.
func (g *TerminatedPolicyGC) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("policy-gc")
	logger.Info("Terminated policy GC started",
		"namespace", g.Namespace,
		"interval", g.Interval,
		"ttl", g.TTL,
		"batchMax", g.BatchMax,
	)

	interval := g.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 시작 시 즉시 한 번 정리(누적 backlog를 빠르게 줄이기 위함).
	g.sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info("Terminated policy GC stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			g.sweep(ctx)
		}
	}
}

// NeedLeaderElection은 manager.LeaderElectionRunnable 인터페이스를 구현한다.
// GC는 leader에서만 실행해 중복 삭제를 방지한다.
func (g *TerminatedPolicyGC) NeedLeaderElection() bool {
	return true
}

// sweep는 종료 후 TTL이 지난 CR을 한 주기 분량만큼 삭제한다.
// 개별 삭제 실패는 로깅 후 계속 진행하며(한 건 실패가 전체를 멈추지 않게),
// BatchMax에 도달하면 남은 추정치를 로깅하고 다음 주기로 미룬다.
func (g *TerminatedPolicyGC) sweep(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("policy-gc")

	var list apollov1.OrchestrationPolicyList
	if err := g.Client.List(ctx, &list, client.InNamespace(g.Namespace)); err != nil {
		logger.Error(err, "Failed to list OrchestrationPolicy for GC")
		return
	}

	now := time.Now()
	deleted := 0
	eligible := 0
	for i := range list.Items {
		policy := &list.Items[i]
		if !isTerminalPhase(policy.Status.Phase) {
			continue
		}
		// 이미 삭제 진행 중인 CR은 건너뛴다.
		if policy.GetDeletionTimestamp() != nil {
			continue
		}
		terminalAt := terminalTime(policy)
		if now.Sub(terminalAt) <= g.TTL {
			continue
		}
		eligible++

		if g.BatchMax > 0 && deleted >= g.BatchMax {
			// 이번 주기 상한 도달 — 남은 건 다음 주기에 처리. silent cap 금지: 로깅한다.
			continue
		}

		if err := g.Client.Delete(ctx, policy); err != nil {
			if client.IgnoreNotFound(err) == nil {
				// 다른 인스턴스/사용자가 먼저 삭제 — 정상.
				deleted++
				continue
			}
			logger.Error(err, "Failed to delete terminated OrchestrationPolicy",
				"name", policy.Name, "phase", policy.Status.Phase)
			continue
		}
		deleted++
		logger.V(1).Info("Deleted terminated OrchestrationPolicy",
			"name", policy.Name, "phase", policy.Status.Phase, "terminalAt", terminalAt)
	}

	if deleted > 0 || eligible > 0 {
		remaining := eligible - deleted
		if remaining < 0 {
			remaining = 0
		}
		logger.Info("Terminated policy GC sweep complete",
			"scanned", len(list.Items),
			"eligible", eligible,
			"deleted", deleted,
			"deferredToNextSweep", remaining,
		)
	}
}

// isTerminalPhase는 phase가 종료 상태인지 판단한다.
func isTerminalPhase(phase apollov1.ExecutionPhase) bool {
	switch phase {
	case apollov1.PhaseApplied, apollov1.PhaseCompleted, apollov1.PhaseFailed, apollov1.PhaseRejected:
		return true
	default:
		return false
	}
}

// terminalTime은 CR이 종료 상태가 된 시각을 추정한다.
// 우선순위:
//  1. status.CompletedAt (Completed/Failed는 항상 설정됨)
//  2. Rejected 컨디션의 LastTransitionTime (Rejected는 CompletedAt이 없음)
//  3. metadata.CreationTimestamp (둘 다 없을 때 안전한 fallback)
func terminalTime(policy *apollov1.OrchestrationPolicy) time.Time {
	if policy.Status.CompletedAt != nil {
		return policy.Status.CompletedAt.Time
	}
	for _, cond := range policy.Status.Conditions {
		if cond.Type == "Rejected" {
			return cond.LastTransitionTime.Time
		}
	}
	return policy.CreationTimestamp.Time
}
