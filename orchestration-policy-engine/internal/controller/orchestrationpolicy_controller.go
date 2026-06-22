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
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/operator"
	"orchestration-policy-engine/internal/policyagent"
)

// PolicyAgentEvaluator는 실행 전 정책 평가 클라이언트의 최소 계약이다.
type PolicyAgentEvaluator interface {
	Evaluate(ctx context.Context, policy *policyagent.Policy) (policyagent.Decision, error)
}

// OrchestrationPolicyReconciler reconciles a OrchestrationPolicy object
type OrchestrationPolicyReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	OperatorClient          *operator.Client
	PolicyAgentClient       PolicyAgentEvaluator
	PolicyAgentEnabled      bool
	PolicyAgentRequeueAfter time.Duration
	MaxConcurrentReconciles int

	// ExecutionTimeout은 Executing 단계 최대 허용 시간(초과 시 Failed).
	// 0이면 handleExecutingPolicy에서 30분 기본값을 사용한다.
	ExecutionTimeout time.Duration

	// SyncCompleteGrace는 migration 이외(sync형 operator API) 정책에서
	// status.result 반영 후 Completed로 보내기까지 대기하는 최소 경과 시간.
	// 0이면 45초 기본값을 사용한다.
	SyncCompleteGrace time.Duration

	// MigrationAPITimeoutSec은 Operator StartMigration 요청의 timeout(초)이다.
	MigrationAPITimeoutSec int

	// MigrationMaxConsecutive404는 마이그레이션 상태 GET이 연속 404일 때 Failed로 보내는 횟수 상한.
	MigrationMaxConsecutive404 int

	// MigrationMaxConsecutivePollErrors는 마이그레이션 폴링 오류(네트워크 등) 연속 허용 횟수 상한.
	MigrationMaxConsecutivePollErrors int

	// SyncMaxConsecutive404는 동기형 정책 검증 GET의 연속 404 허용 상한.
	SyncMaxConsecutive404 int

	// SyncMaxConsecutiveGETErrors는 동기형 정책 검증 GET 오류 연속 허용 상한.
	SyncMaxConsecutiveGETErrors int

	// ProvisioningStrictReadyOnly가 true이면 프로비저닝은 Operator ready일 때만 Completed.
	ProvisioningStrictReadyOnly bool
}

// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="argoproj.io",resources=workflows,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *OrchestrationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the OrchestrationPolicy instance
	policy := &apollov1.OrchestrationPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		// Policy deleted, nothing to do
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("============================================")
	logger.Info("Reconciling OrchestrationPolicy",
		"name", policy.Name,
		"namespace", policy.Namespace,
		"policyType", policy.Spec.PolicyType,
		"urgency", policy.Spec.Urgency,
		"probability", policy.Spec.Probability,
		"priorityScore", policy.Spec.PriorityScore,
	)

	// Handle based on current phase
	switch policy.Status.Phase {
	case "":
		// New policy - set to Pending
		return r.handleNewPolicy(ctx, policy)

	case apollov1.PhasePending:
		// Check if auto-execute is enabled
		if policy.Spec.AutoExecute {
			return r.handleAutoApprove(ctx, policy)
		}
		// Otherwise, wait for manual approval
		logger.Info("Policy waiting for approval",
			"name", policy.Name,
			"autoExecute", policy.Spec.AutoExecute,
		)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil

	case apollov1.PhaseApproved:
		// Execute the policy
		return r.handleApprovedPolicy(ctx, policy)

	case apollov1.PhaseExecuting:
		// Monitor execution
		return r.handleExecutingPolicy(ctx, policy)

	case apollov1.PhaseWaitingForTarget:
		return r.handleWaitingForTarget(ctx, policy)

	case apollov1.PhaseFailed:
		if policy.Annotations != nil && policy.Annotations[annRetryFromFailure] == "true" {
			return r.handleRetryFromFailure(ctx, policy)
		}
		logger.Info("Policy in terminal state",
			"name", policy.Name,
			"phase", policy.Status.Phase,
		)
		return ctrl.Result{}, nil

	case apollov1.PhaseApplied, apollov1.PhaseCompleted, apollov1.PhaseRejected:
		// Terminal states - no action needed
		logger.Info("Policy in terminal state",
			"name", policy.Name,
			"phase", policy.Status.Phase,
		)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// handleNewPolicy initializes a new policy
func (r *OrchestrationPolicyReconciler) handleNewPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if policy.Spec.ExecutionMode == "" {
		if err := r.patchSpecWithRetry(ctx, policy, func(spec *apollov1.OrchestrationPolicySpec) {
			spec.ExecutionMode = defaultExecutionModeForPolicyType(spec.PolicyType)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("========================================")
	logger.Info("NEW POLICY DETECTED",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"urgency", policy.Spec.Urgency,
		"probability", fmt.Sprintf("%d%%", policy.Spec.Probability),
		"targetNode", policy.Spec.TargetNode,
		"reason", policy.Spec.Reason,
	)
	logger.Info("========================================")

	// Update status to Pending
	policy.Status.Phase = apollov1.PhasePending
	policy.Status.Message = "Policy created, waiting for approval"

	// Set condition
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "PolicyCreated",
		Message:            "Policy has been created and is pending approval",
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	logger.Info("Policy status updated to Pending",
		"name", policy.Name,
	)

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleAutoApprove는 autoExecute 정책을 Policy Agent 평가 후 승인한다.
func (r *OrchestrationPolicyReconciler) handleAutoApprove(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("========================================")
	logger.Info("AUTO-APPROVING POLICY",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
	)
	logger.Info("========================================")

	if err := validateExecutionModeCompatibility(policy); err != nil {
		return r.rejectPolicy(ctx, policy, policyagent.Decision{
			Action: policyagent.DecisionReject,
			Reason: err.Error(),
		})
	}

	if r.PolicyAgentEnabled {
		decision, err := r.evaluatePolicyAgent(ctx, policy)
		if err != nil {
			logger.Error(err, "Policy Agent evaluation failed")
			return ctrl.Result{RequeueAfter: r.policyAgentRequeueAfter()}, nil
		}

		logger.Info(fmt.Sprintf("Policy Agent decision: action=%s reason=%s", decision.Action, decision.Reason),
			"name", policy.Name,
			"action", decision.Action,
			"reason", decision.Reason,
		)

		switch decision.Action {
		case policyagent.DecisionApprove:
			return r.approvePolicy(ctx, policy, "PolicyAgentApproved", "Policy Agent approved auto-execute policy")
		case policyagent.DecisionDelay:
			return r.delayPolicy(ctx, policy, decision)
		case policyagent.DecisionReject:
			return r.rejectPolicy(ctx, policy, decision)
		case policyagent.DecisionModify:
			return r.modifyAndApprovePolicy(ctx, policy, decision)
		default:
			return ctrl.Result{}, fmt.Errorf("unknown policy agent decision: %s", decision.Action)
		}
	}

	return r.approvePolicy(ctx, policy, "AutoApproved", "Policy auto-approved based on autoExecute flag")
}

func (r *OrchestrationPolicyReconciler) evaluatePolicyAgent(ctx context.Context, policy *apollov1.OrchestrationPolicy) (policyagent.Decision, error) {
	agentPolicy := toPolicyAgentPolicy(policy)
	if r.PolicyAgentClient != nil {
		return r.PolicyAgentClient.Evaluate(ctx, agentPolicy)
	}
	return policyagent.RuleBasedEvaluate(agentPolicy, r.policyAgentRequeueAfter()), nil
}

func (r *OrchestrationPolicyReconciler) policyAgentRequeueAfter() time.Duration {
	if r.PolicyAgentRequeueAfter <= 0 {
		return 30 * time.Second
	}
	return r.PolicyAgentRequeueAfter
}

func (r *OrchestrationPolicyReconciler) approvePolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy, reason, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Policy Agent 승인 이후에만 실행 단계로 넘어가도록 상태 전이를 분리한다.
	policy.Status.Phase = apollov1.PhaseApproved
	policy.Status.Message = message

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Approved",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	// Approved 단계 Reconcile을 즉시 유도해 기존 실행 흐름을 그대로 재사용한다.
	return ctrl.Result{Requeue: true}, nil
}

func (r *OrchestrationPolicyReconciler) delayPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy, decision policyagent.Decision) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	requeueAfter := decision.RequeueAfter
	if requeueAfter <= 0 {
		requeueAfter = r.policyAgentRequeueAfter()
	}
	message := decision.Reason
	if message == "" {
		message = "Policy Agent delayed auto-execute policy"
	}

	policy.Status.Phase = apollov1.PhasePending
	policy.Status.Message = message
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "PolicyAgentDelayed",
		Status:             metav1.ConditionTrue,
		Reason:             "PolicyAgentDelayed",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update delayed policy status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *OrchestrationPolicyReconciler) rejectPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy, decision policyagent.Decision) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	message := decision.Reason
	if message == "" {
		message = "Policy Agent rejected auto-execute policy"
	}

	policy.Status.Phase = apollov1.PhaseRejected
	policy.Status.Message = message
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Rejected",
		Status:             metav1.ConditionTrue,
		Reason:             "PolicyAgentRejected",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update rejected policy status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *OrchestrationPolicyReconciler) modifyAndApprovePolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy, decision policyagent.Decision) (ctrl.Result, error) {
	if decision.ModifiedSpec != nil {
		if err := r.patchSpecWithRetry(ctx, policy, func(spec *apollov1.OrchestrationPolicySpec) {
			*spec = fromPolicyAgentSpec(*decision.ModifiedSpec)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	message := decision.Reason
	if message == "" {
		message = "Policy Agent modified and approved auto-execute policy"
	}
	return r.approvePolicy(ctx, policy, "PolicyAgentModified", message)
}

func toPolicyAgentPolicy(policy *apollov1.OrchestrationPolicy) *policyagent.Policy {
	return &policyagent.Policy{
		APIVersion: policy.APIVersion,
		Kind:       policy.Kind,
		Metadata: policyagent.PolicyMetadata{
			Name:      policy.Name,
			Namespace: policy.Namespace,
		},
		Spec: policyagent.PolicySpec{
			PolicyType:      string(policy.Spec.PolicyType),
			Probability:     policy.Spec.Probability,
			Urgency:         string(policy.Spec.Urgency),
			ResourceType:    string(policy.Spec.ResourceType),
			TargetNode:      policy.Spec.TargetNode,
			SourceNode:      policy.Spec.SourceNode,
			TargetWorkload:  policy.Spec.TargetWorkload,
			TargetNamespace: policy.Spec.TargetNamespace,
			Reason:          policy.Spec.Reason,
			Horizon:         policy.Spec.Horizon,
			AutoExecute:     policy.Spec.AutoExecute,
			ExecutionMode:   string(policy.Spec.ExecutionMode),
			TargetRunID:     policy.Spec.TargetRunID,
			TargetStage:     policy.Spec.TargetStage,
			Selector:        copyPolicyAgentParameters(policy.Spec.Selector),
			PriorityScore:   policy.Spec.PriorityScore,
			Parameters:      copyPolicyAgentParameters(policy.Spec.Parameters),
		},
	}
}

func fromPolicyAgentSpec(spec policyagent.PolicySpec) apollov1.OrchestrationPolicySpec {
	return apollov1.OrchestrationPolicySpec{
		PolicyType:      apollov1.PolicyType(spec.PolicyType),
		Probability:     spec.Probability,
		Urgency:         apollov1.Urgency(spec.Urgency),
		ResourceType:    apollov1.ResourceType(spec.ResourceType),
		TargetNode:      spec.TargetNode,
		SourceNode:      spec.SourceNode,
		TargetWorkload:  spec.TargetWorkload,
		TargetNamespace: spec.TargetNamespace,
		Reason:          spec.Reason,
		Horizon:         spec.Horizon,
		AutoExecute:     spec.AutoExecute,
		ExecutionMode:   apollov1.ExecutionMode(spec.ExecutionMode),
		TargetRunID:     spec.TargetRunID,
		TargetStage:     spec.TargetStage,
		Selector:        copyPolicyAgentParameters(spec.Selector),
		PriorityScore:   spec.PriorityScore,
		Parameters:      copyPolicyAgentParameters(spec.Parameters),
	}
}

func copyPolicyAgentParameters(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

// handleApprovedPolicy executes an approved policy
func (r *OrchestrationPolicyReconciler) handleApprovedPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if err := validateExecutionModeCompatibility(policy); err != nil {
		return r.rejectPolicy(ctx, policy, policyagent.Decision{
			Action: policyagent.DecisionReject,
			Reason: err.Error(),
		})
	}
	if policy.Spec.ExecutionMode == apollov1.ExecutionModeOnTargetCreated {
		return r.movePolicyToWaitingTarget(ctx, policy)
	}

	logger.Info("========================================")
	logger.Info("EXECUTING APPROVED POLICY",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"targetNode", policy.Spec.TargetNode,
		"targetWorkload", policy.Spec.TargetWorkload,
	)
	logger.Info("========================================")

	// Update to Executing
	now := metav1.Now()
	policy.Status.Phase = apollov1.PhaseExecuting
	policy.Status.ExecutedAt = &now
	policy.Status.ExecutedBy = "orchestration-policy-engine-controller"
	policy.Status.Message = fmt.Sprintf("Running %s policy", policy.Spec.PolicyType)

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Running",
		Status:             metav1.ConditionTrue,
		Reason:             "ExecutionRunning",
		Message:            fmt.Sprintf("%s policy is running", policy.Spec.PolicyType),
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	// 정책 타입별 실행
	execErr := r.executePolicyByType(ctx, policy)

	if execErr != nil {
		if r.isWaitingForTargetError(execErr) {
			policy.Status.Phase = apollov1.PhaseWaitingForTarget
			policy.Status.Message = execErr.Error()
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:               "WaitingForTarget",
				Status:             metav1.ConditionTrue,
				Reason:             "TargetNotReady",
				Message:            execErr.Error(),
				LastTransitionTime: metav1.Now(),
			})
			if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
				*st = policy.Status
			}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(execErr, "Policy execution failed")
		retries := annInt(policy, annExecStartErrStreak) + 1
		if patchErr := r.patchAnnotationInt(ctx, policy, annExecStartErrStreak, retries); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		if retries < 3 {
			logger.Info("Execution start failed, retrying",
				"name", policy.Name,
				"retry", retries,
			)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.markPolicyFailed(ctx, policy, execErr.Error())
	}
	if patchErr := r.patchAnnotationInt(ctx, policy, annExecStartErrStreak, 0); patchErr != nil {
		return ctrl.Result{}, patchErr
	}

	// Requeue to monitor execution
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

type executionTarget struct {
	UID       types.UID
	Kind      string
	Name      string
	Namespace string
}

func validateExecutionModeCompatibility(policy *apollov1.OrchestrationPolicy) error {
	mode := policy.Spec.ExecutionMode
	if mode == "" || mode == apollov1.ExecutionModeImmediate {
		return nil
	}
	if mode == apollov1.ExecutionModeContinuous && policy.Spec.PolicyType != apollov1.PolicyTypeScaling {
		return fmt.Errorf("executionMode Continuous is only allowed for scaling policies")
	}
	return nil
}

func defaultExecutionModeForPolicyType(policyType apollov1.PolicyType) apollov1.ExecutionMode {
	switch policyType {
	case apollov1.PolicyTypeScaling:
		return apollov1.ExecutionModeContinuous
	case apollov1.PolicyTypeMigration, apollov1.PolicyTypePreemption:
		return apollov1.ExecutionModeImmediate
	case apollov1.PolicyTypeProvisioning, apollov1.PolicyTypeCaching:
		return apollov1.ExecutionModeImmediate
	default:
		return apollov1.ExecutionModeImmediate
	}
}

func (r *OrchestrationPolicyReconciler) isWaitingForTargetError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *operator.OperatorAPIError
	if errors.As(err, &apiErr) && apiErr.IsWaitingForTarget() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 425") ||
		strings.Contains(msg, "error_code=waitingfortarget") ||
		strings.Contains(msg, "waiting_for_target") ||
		strings.Contains(msg, "waiting for target")
}

func (r *OrchestrationPolicyReconciler) movePolicyToWaitingTarget(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	if policy.Status.Consumed {
		now := metav1.Now()
		policy.Status.Phase = apollov1.PhaseApplied
		policy.Status.Message = "Policy already consumed"
		policy.Status.CompletedAt = &now
		if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
			*st = policy.Status
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	policy.Status.Phase = apollov1.PhaseWaitingForTarget
	policy.Status.Message = "Waiting for a new target resource matching targetRunID, targetStage, selector"
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "WaitingForTarget",
		Status:             metav1.ConditionTrue,
		Reason:             "OnTargetCreated",
		Message:            policy.Status.Message,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *OrchestrationPolicyReconciler) handleWaitingForTarget(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	if strings.TrimSpace(policy.Spec.TargetWorkload) == "" || strings.TrimSpace(policy.Spec.TargetNamespace) == "" {
		return r.markPolicyFailed(ctx, policy, "InvalidPolicy: targetWorkload and targetNamespace are required for WaitingForTarget")
	}

	if policy.Status.Consumed {
		if policy.Status.Phase != apollov1.PhaseApplied {
			now := metav1.Now()
			policy.Status.Phase = apollov1.PhaseApplied
			policy.Status.Message = "Policy already consumed"
			policy.Status.CompletedAt = &now
			if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
				*st = policy.Status
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	target, err := r.findNewMatchingTarget(ctx, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	now := metav1.Now()
	policy.Status.Consumed = true
	policy.Status.AppliedTargets = append(policy.Status.AppliedTargets, apollov1.TargetReference{
		UID:       string(target.UID),
		Kind:      target.Kind,
		Name:      target.Name,
		Namespace: target.Namespace,
		AppliedAt: &now,
	})
	policy.Status.Phase = apollov1.PhaseApproved
	policy.Status.Message = fmt.Sprintf("Matched new target %s/%s (%s), applying policy", target.Namespace, target.Name, target.Kind)
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "TargetMatched",
		Status:             metav1.ConditionTrue,
		Reason:             "OnTargetCreatedMatched",
		Message:            policy.Status.Message,
		LastTransitionTime: now,
	})

	if strings.TrimSpace(policy.Spec.TargetNamespace) == "" {
		policy.Spec.TargetNamespace = target.Namespace
	}
	if strings.TrimSpace(policy.Spec.TargetWorkload) == "" {
		policy.Spec.TargetWorkload = target.Name
	}

	if err := r.patchSpecWithRetry(ctx, policy, func(spec *apollov1.OrchestrationPolicySpec) {
		if strings.TrimSpace(spec.TargetNamespace) == "" {
			spec.TargetNamespace = target.Namespace
		}
		if strings.TrimSpace(spec.TargetWorkload) == "" {
			spec.TargetWorkload = target.Name
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handleExecutingPolicy monitors an executing policy
func (r *OrchestrationPolicyReconciler) handleExecutingPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Monitoring executing policy",
		"name", policy.Name,
		"executedAt", policy.Status.ExecutedAt,
	)

	timeout := r.ExecutionTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	syncGrace := r.SyncCompleteGrace
	if syncGrace <= 0 {
		syncGrace = 45 * time.Second
	}

	if policy.Status.ExecutedAt == nil {
		logger.Info("Executing policy status snapshot",
			"name", policy.Name,
			"phase", policy.Status.Phase,
			"result", policy.Status.Result,
			"elapsed", "executedAt=nil",
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	elapsed := time.Since(policy.Status.ExecutedAt.Time)
	logger.Info("Executing policy status snapshot",
		"name", policy.Name,
		"phase", policy.Status.Phase,
		"result", policy.Status.Result,
		"elapsed", elapsed.String(),
	)
	if elapsed > timeout {
		logger.Error(fmt.Errorf("execution timeout"), "Execution exceeded timeout",
			"name", policy.Name,
			"phase", policy.Status.Phase,
			"elapsed", elapsed.String(),
			"timeout", timeout.String(),
		)
		return r.markPolicyFailed(ctx, policy, fmt.Sprintf("Execution timeout after %s", timeout.String()))
	}

	if policy.Status.Result == "" {
		// 동기형 정책은 시작 응답 ID(result)가 비어 있으면 짧게 1회 재실행해 복구한다.
		// 동일 시점 status 충돌로 결과 저장만 유실된 케이스를 최소 변경으로 보정하기 위함.
		if r.isSyncOperatorPolicyType(policy.Spec.PolicyType) && elapsed >= 10*time.Second {
			if retries := annInt(policy, annExecStartErrStreak); retries < 1 {
				if execErr := r.executePolicyByType(ctx, policy); execErr != nil {
					logger.Error(execErr, "Result backfill re-execution failed")
				} else if policy.Status.Result != "" {
					if patchErr := r.patchAnnotationInt(ctx, policy, annExecStartErrStreak, 1); patchErr != nil {
						return ctrl.Result{}, patchErr
					}
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
			}
		}

		// 실행 직후 operator가 status.result를 채우기까지 지연이 있음.
		// annExecNoResultStreak를 metadata Patch로 올리면 object generation이 바뀌어
		// watch가 즉시 재조정을 연속 발생시키고, RequeueAfter(10s)와 무관하게
		// streak가 빠르게 6에 도달한다(클러스터 Failed policy: ExecutedAt 대비 약 4초 내 종료).
		// 기존 6회×10초와 동등한 관측 창을 실행 시각 기준 경과로만 판정한다.
		if elapsed >= 60*time.Second {
			return r.markPolicyFailed(ctx, policy, "execution hung: no operator result observed")
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check execution status from operator
	if r.OperatorClient != nil && policy.Status.Result != "" {
		if policy.Spec.PolicyType == apollov1.PolicyTypeMigration {
			migrationID, ok := extractResultResourceID(policy.Status.Result)
			if !ok || migrationID == "" {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			status, code, err := r.OperatorClient.GetMigrationStatus(ctx, migrationID)
			if err == nil && status != nil {
				if patchErr := r.clearMigrationPollAnnotations(ctx, policy); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				logger.Info("Migration status",
					"migrationID", migrationID,
					"status", status.Status,
				)
				s := strings.ToLower(strings.TrimSpace(status.Status))
				switch s {
				case "completed":
					return r.markPolicyCompleted(ctx, policy)
				case "failed":
					return r.markPolicyFailed(ctx, policy, status.Message)
				case "cancelled":
					return r.markPolicyFailed(ctx, policy, "migration cancelled: "+status.Message)
				default:
					return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
				}
			}

			logger.Error(err, "Failed to get migration status", "code", code)

			max404 := r.MigrationMaxConsecutive404
			if max404 <= 0 {
				max404 = 5
			}
			maxPoll := r.MigrationMaxConsecutivePollErrors
			if maxPoll <= 0 {
				maxPoll = 10
			}

			if code == http.StatusNotFound {
				n := annInt(policy, annMigration404Streak) + 1
				if patchErr := r.patchAnnotationInt(ctx, policy, annMigration404Streak, n); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				if n >= max404 {
					return r.markPolicyFailed(ctx, policy,
						fmt.Sprintf("migration status 404 exceeded threshold (%d); orchestrator may have restarted", max404))
				}
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}

			n := annInt(policy, annMigrationPollErrStreak) + 1
			if patchErr := r.patchAnnotationInt(ctx, policy, annMigrationPollErrStreak, n); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			if n >= maxPoll {
				return r.markPolicyFailed(ctx, policy,
					fmt.Sprintf("migration polling failed %d times (last error: %v)", maxPoll, err))
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		if r.isSyncOperatorPolicyType(policy.Spec.PolicyType) {
			if elapsed >= syncGrace {
				return r.handleSyncOperatorAfterGrace(ctx, policy)
			}
		}
	}

	// operator 미구성 시 데모 완료
	if r.OperatorClient == nil && elapsed > 30*time.Second {
		return r.markPolicyCompleted(ctx, policy)
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// executePolicyByType은 정책 타입에 맞는 실행 함수를 호출한다.
func (r *OrchestrationPolicyReconciler) executePolicyByType(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	switch policy.Spec.PolicyType {
	case apollov1.PolicyTypeMigration:
		return r.executeMigrationPolicy(ctx, policy)
	case apollov1.PolicyTypeScaling:
		return r.executeScalingPolicy(ctx, policy)
	case apollov1.PolicyTypeProvisioning:
		return r.executeProvisioningPolicy(ctx, policy)
	case apollov1.PolicyTypeCaching:
		return r.executeCachingPolicy(ctx, policy)
	case apollov1.PolicyTypeLoadbalance:
		return r.executeLoadbalancePolicy(ctx, policy)
	case apollov1.PolicyTypePreemption:
		return r.executePreemptionPolicy(ctx, policy)
	default:
		return fmt.Errorf("unknown policy type: %s", policy.Spec.PolicyType)
	}
}

// isSyncOperatorPolicyType은 migration처럼 장기 폴링이 아닌 operator API에 해당하는 정책 타입인지 판별한다.
func (r *OrchestrationPolicyReconciler) isSyncOperatorPolicyType(pt apollov1.PolicyType) bool {
	switch pt {
	case apollov1.PolicyTypeScaling, apollov1.PolicyTypeProvisioning,
		apollov1.PolicyTypeCaching, apollov1.PolicyTypeLoadbalance, apollov1.PolicyTypePreemption:
		return true
	default:
		return false
	}
}

// executeMigrationPolicy executes a migration policy
func (r *OrchestrationPolicyReconciler) executeMigrationPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            MIGRATION POLICY EXECUTION                         ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Workload:     " + fmt.Sprintf("%-47s", policy.Spec.TargetWorkload) + " ║")
	logger.Info("║ Namespace:    " + fmt.Sprintf("%-47s", policy.Spec.TargetNamespace) + " ║")
	logger.Info("║ Source Node:  " + fmt.Sprintf("%-47s", policy.Spec.SourceNode) + " ║")
	logger.Info("║ Target Node:  " + fmt.Sprintf("%-47s", policy.Spec.TargetNode) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating migration")
		return nil
	}

	targetNamespace := policy.Spec.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = policy.Namespace
	}
	targetWorkload := strings.TrimSpace(policy.Spec.TargetWorkload)
	if targetWorkload == "" {
		return fmt.Errorf("invalid policy: targetWorkload is required")
	}
	targetNode := strings.TrimSpace(paramStr(policy, "targetNode", policy.Spec.TargetNode))
	if targetNode == "" {
		return fmt.Errorf("invalid policy: targetNode is required")
	}
	workloadType := strings.TrimSpace(paramStr(policy, "workloadType", "auto"))
	if workloadType == "" {
		workloadType = "auto"
	}
	explicitPodName := strings.TrimSpace(paramStr(policy, "podName", ""))
	explicitPodNamespace := strings.TrimSpace(paramStr(policy, "podNamespace", targetNamespace))
	sourceNode := strings.TrimSpace(paramStr(policy, "sourceNode", policy.Spec.SourceNode))

	timeoutSec := r.MigrationAPITimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 600
	}

	logger.Info("Migration request prepared",
		"workloadName", targetWorkload,
		"workloadNamespace", targetNamespace,
		"workloadType", workloadType,
		"podName", explicitPodName,
		"podNamespace", explicitPodNamespace,
		"sourceNode", sourceNode,
		"targetNode", targetNode,
	)

	// Call operator to start migration
	migrationReq := &operator.MigrationRequest{
		WorkloadName:      targetWorkload,
		WorkloadNamespace: targetNamespace,
		WorkloadType:      workloadType,
		SourceNode:        sourceNode,
		TargetNode:        targetNode,
		PreservePV:        paramBool(policy, "preservePV", true),
		Timeout:           timeoutSec,
		Stage:             paramStr(policy, "targetStage", ""),
		RunID:             paramStr(policy, "runID", policy.Name),
		PolicyName:        policy.Name,
		Action:            "migration",
		ResourceRequests: map[string]string{
			"cpu":    paramStr(policy, "resourceAfterCPU", "2"),
			"memory": paramStr(policy, "resourceAfterMemory", "4Gi"),
		},
		SchedulerName: paramStr(policy, "schedulerName", "ai-storage-scheduler"),
		NodeSelector: map[string]string{
			"kubernetes.io/hostname": targetNode,
		},
		QueueLabel: paramStr(policy, "queueLabel", paramStr(policy, "targetStage", "output")),
		StorageAnnotations: map[string]string{
			"run_id":       paramStr(policy, "runID", policy.Name),
			"target_stage": paramStr(policy, "targetStage", "output"),
		},
		PolicyAnnotations: map[string]string{
			"selected_policy": policy.Name,
			"action_plan":     "migration",
		},
	}
	if explicitPodName != "" {
		migrationReq.PodName = explicitPodName
		migrationReq.PodNamespace = explicitPodNamespace
	} else {
		// #13a: 마이그레이션은 targetWorkload가 곧 Pod 이름(generator의
		// resolveTargetWorkloadForRecommendation이 migration엔 podName 반환).
		// orchestrator /migrations는 pod_name 필수라 비우면 HTTP 400 → targetWorkload로 채운다.
		migrationReq.PodName = targetWorkload
		migrationReq.PodNamespace = targetNamespace
	}
	resp, err := r.OperatorClient.StartMigration(ctx, migrationReq)
	if err != nil {
		return fmt.Errorf("failed to start migration: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.MigrationID,
		"operatorStatus", resp.Status,
	)

	// Store migration ID for status tracking
	policy.Status.Result = formatSelectedPolicyResult(policy, "migration", resp.MigrationID, map[string]string{
		"target_stage":    paramStr(policy, "targetStage", "output"),
		"resource_before": buildResourceSnapshot(policy, "before"),
		"resource_after":  buildResourceSnapshot(policy, "after"),
		"target_node":     targetNode,
		"source_node":     sourceNode,
	})
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", resp.MigrationID,
		)
		return fmt.Errorf("failed to update policy with migration ID: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Migration initiated",
		"migrationID", resp.MigrationID,
		"status", resp.Status,
	)

	return nil
}

// executeScalingPolicy executes a scaling policy
func (r *OrchestrationPolicyReconciler) executeScalingPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            AUTOSCALING POLICY EXECUTION                       ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Workload:     " + fmt.Sprintf("%-47s", policy.Spec.TargetWorkload) + " ║")
	logger.Info("║ Namespace:    " + fmt.Sprintf("%-47s", policy.Spec.TargetNamespace) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating scaling")
		return nil
	}

	// WHY: 데모/운영에서 정책 CR이 spec.parameters로 임계치를 내려 보낼 수 있어야
	//      MinReplicas>현재 replicas 또는 낮은 targetCPU로 즉시 스케일업이 트리거된다.
	workloadType := paramStr(policy, "workloadType", "auto")
	resp, err := r.OperatorClient.ConfigureAutoscaling(ctx, &operator.AutoscalingRequest{
		WorkloadName:      policy.Spec.TargetWorkload,
		WorkloadNamespace: policy.Spec.TargetNamespace,
		WorkloadType:      workloadType,
		MinReplicas:       paramInt32(policy, "minReplicas", 1),
		MaxReplicas:       paramInt32(policy, "maxReplicas", 5),
		TargetCPU:         paramInt32(policy, "targetCPU", 80),
		TargetMemory:      paramInt32(policy, "targetMemory", 80),
	})
	if err != nil {
		return fmt.Errorf("failed to configure autoscaling: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.AutoscalingID,
		"operatorStatus", resp.Status,
	)

	if strings.EqualFold(resp.Status, "failed") {
		return fmt.Errorf("autoscaling reported failed: %s", resp.Message)
	}

	policy.Status.Result = fmt.Sprintf("type=autoscaling;id=%s;status=%s", resp.AutoscalingID, resp.Status)
	if resp.Message != "" {
		policy.Status.Result += fmt.Sprintf(";msg=%s", truncate(resp.Message, 120))
	}
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", policy.Status.Result,
		)
		return fmt.Errorf("failed to persist autoscaling result: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Autoscaling configured",
		"autoscalingID", resp.AutoscalingID,
		"status", resp.Status,
	)

	return nil
}

// defaultProvisioningStorageClass는 storageClass 파라미터가 없을 때 사용하는 기본 tier다.
// WHY: 종전 기본값 "high-throughput"은 orchestrator의 normalizeTier(L1/L2/L3/S3 + 별칭)가
// 인식하지 못해 프로비저닝 요청이 400으로 거부됐다. "L2"(NVMe/hot)는 서버가 storage-l2로
// 정규화하며 전처리/학습 입력 워크로드의 hot-data 특성과도 맞는 안전한 기본값이다.
// 클러스터별 SC 이름이 다르면 policy.Spec.Parameters["storageClass"]로 덮어쓸 수 있다.
const defaultProvisioningStorageClass = "L2"

// executeProvisioningPolicy executes a provisioning policy
func (r *OrchestrationPolicyReconciler) executeProvisioningPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            PROVISIONING POLICY EXECUTION                      ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Target Node:  " + fmt.Sprintf("%-47s", policy.Spec.TargetNode) + " ║")
	logger.Info("║ Resource:     " + fmt.Sprintf("%-47s", string(policy.Spec.ResourceType)) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating provisioning")
		return nil
	}

	// WHY: 클러스터에 따라 동적 프로비저너가 묶여 있는 StorageClass 이름이 다르므로
	//      Parameters로 덮어쓸 수 있어야 PVC가 Bound 까지 도달한다.
	resp, err := r.OperatorClient.StartProvisioning(ctx, &operator.ProvisioningRequest{
		WorkloadName:      policy.Spec.TargetWorkload,
		WorkloadNamespace: policy.Spec.TargetNamespace,
		WorkloadType:      paramStr(policy, "workloadType", "training"),
		RunID:             paramStr(policy, "runID", policy.Name),
		TargetStage:       paramStr(policy, "targetStage", "storage"),
		StorageSize:       paramStr(policy, "storageSize", "100Gi"),
		StorageClass:      paramStr(policy, "storageClass", defaultProvisioningStorageClass),
		AccessMode:        paramStr(policy, "accessMode", "ReadWriteOnce"),
		Labels: map[string]string{
			"kueue.x-k8s.io/queue-name": paramStr(policy, "queueLabel", paramStr(policy, "targetStage", "storage")),
		},
		Annotations: map[string]string{
			"run_id":          paramStr(policy, "runID", policy.Name),
			"target_stage":    paramStr(policy, "targetStage", "storage"),
			"selected_policy": policy.Name,
			"action_plan":     "provisioning",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to start provisioning: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.ProvisioningID,
		"operatorStatus", resp.Status,
	)

	if strings.EqualFold(resp.Status, "failed") {
		return fmt.Errorf("provisioning reported failed: %s", resp.Message)
	}

	policy.Status.Result = formatSelectedPolicyResult(policy, "provisioning", resp.ProvisioningID, map[string]string{
		"status":          resp.Status,
		"target_stage":    paramStr(policy, "targetStage", "storage"),
		"resource_before": buildResourceSnapshot(policy, "before"),
		"resource_after":  buildResourceSnapshot(policy, "after"),
		"storage_class":   paramStr(policy, "storageClass", defaultProvisioningStorageClass),
	})
	if resp.Message != "" {
		policy.Status.Result += fmt.Sprintf(";msg=%s", truncate(resp.Message, 120))
	}
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", policy.Status.Result,
		)
		return fmt.Errorf("failed to persist provisioning result: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Provisioning initiated",
		"provisioningID", resp.ProvisioningID,
		"status", resp.Status,
	)

	return nil
}

// executeCachingPolicy executes a caching policy
func (r *OrchestrationPolicyReconciler) executeCachingPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            CACHING POLICY EXECUTION                           ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Workload:     " + fmt.Sprintf("%-47s", policy.Spec.TargetWorkload) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating caching")
		return nil
	}

	// migration(executeMigrationPolicy)과 동일: spec.targetNamespace가 없으면 정책 CR의 네임스페이스를 워크로드/PVC 네임스페이스로 쓴다.
	sourceNamespace := strings.TrimSpace(policy.Spec.TargetNamespace)
	if sourceNamespace == "" {
		sourceNamespace = policy.Namespace
	}
	logger.Info("Caching source namespace resolved", "namespace", sourceNamespace)

	sourcePath := strings.TrimSpace(policy.Spec.Parameters["sourcePath"])
	if sourcePath == "" {
		sourcePath = "/data"
	}

	// WHY: 데모/운영에서 실제 존재·Bound 된 PVC 이름을 정책 CR이 직접 지정해야
	//      cache-prefetch Pod이 source-pvc 마운트에 성공한다.
	//      기존 합성 규칙(<workload>+"-pvc")은 fallback으로만 사용한다.
	sourcePVC := paramStr(policy, "sourcePVC", "")
	if sourcePVC == "" {
		sourcePVC = policy.Spec.TargetWorkload + "-pvc"
	}
	resp, err := r.OperatorClient.ConfigureCaching(ctx, &operator.CachingRequest{
		SourcePVC:       sourcePVC,
		SourceNamespace: sourceNamespace,
		SourcePath:      sourcePath,
		TargetTier:      paramStr(policy, "targetTier", "nvme"),
		CachePolicy:     paramStr(policy, "cachePolicy", "lru"),
		Priority:        int32(policy.Spec.PriorityScore),
		Prefetch:        paramBool(policy, "prefetch", true),
		Reason:          policy.Spec.Reason,
	})
	if err != nil {
		return fmt.Errorf("failed to configure caching: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.CacheID,
		"operatorStatus", resp.Status,
	)

	if strings.EqualFold(resp.Status, "failed") {
		return fmt.Errorf("caching reported failed: %s", resp.Message)
	}

	policy.Status.Result = fmt.Sprintf("type=caching;id=%s;status=%s", resp.CacheID, resp.Status)
	if resp.Message != "" {
		policy.Status.Result += fmt.Sprintf(";msg=%s", truncate(resp.Message, 120))
	}
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", policy.Status.Result,
		)
		return fmt.Errorf("failed to persist caching result: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Caching configured",
		"cacheID", resp.CacheID,
		"status", resp.Status,
	)

	return nil
}

// executeLoadbalancePolicy executes a loadbalance policy
func (r *OrchestrationPolicyReconciler) executeLoadbalancePolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            LOADBALANCE POLICY EXECUTION                       ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Target Node:  " + fmt.Sprintf("%-47s", policy.Spec.TargetNode) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating loadbalance")
		return nil
	}

	lbStrategy := strings.TrimSpace(policy.Spec.Parameters["strategy"])
	if lbStrategy == "" {
		lbStrategy = "load_spreading"
	}
	// WHY: TargetNodes를 단일 노드로 한정하면 underloaded/overloaded 비교 자체가 불가능해
	//      migration plan이 항상 0 이 된다. parameters.targetNodes(콤마구분) 또는
	//      parameters.scope=all 일 때는 전체 노드를 대상으로 한다.
	targetNodes := parseCSV(paramStr(policy, "targetNodes", ""))
	scope := strings.ToLower(strings.TrimSpace(paramStr(policy, "scope", "")))
	if scope != "all" && len(targetNodes) == 0 && policy.Spec.TargetNode != "" {
		targetNodes = []string{policy.Spec.TargetNode}
	}
	resp, err := r.OperatorClient.ConfigureLoadbalance(ctx, &operator.LoadbalanceRequest{
		Namespace:             strings.TrimSpace(policy.Spec.TargetNamespace),
		TargetNodes:           targetNodes,
		Strategy:              lbStrategy,
		CPUThreshold:          paramInt32(policy, "cpuThreshold", 0),
		MemoryThreshold:       paramInt32(policy, "memoryThreshold", 0),
		GPUThreshold:          paramInt32(policy, "gpuThreshold", 0),
		MaxMigrationsPerCycle: paramInt32(policy, "maxMigrationsPerCycle", 0),
	})
	if err != nil {
		return fmt.Errorf("failed to configure loadbalance: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.LoadbalanceID,
		"operatorStatus", resp.Status,
	)

	if strings.EqualFold(resp.Status, "failed") {
		return fmt.Errorf("loadbalance reported failed: %s", resp.Message)
	}

	policy.Status.Result = fmt.Sprintf("type=loadbalance;id=%s;status=%s", resp.LoadbalanceID, resp.Status)
	if resp.Message != "" {
		policy.Status.Result += fmt.Sprintf(";msg=%s", truncate(resp.Message, 120))
	}
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", policy.Status.Result,
		)
		return fmt.Errorf("failed to persist loadbalance result: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Loadbalance configured",
		"loadbalanceID", resp.LoadbalanceID,
		"status", resp.Status,
	)

	return nil
}

// executePreemptionPolicy executes a preemption policy
func (r *OrchestrationPolicyReconciler) executePreemptionPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) error {
	logger := log.FromContext(ctx)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            PREEMPTION POLICY EXECUTION                        ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Workload:     " + fmt.Sprintf("%-47s", policy.Spec.TargetWorkload) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(policy.Spec.Reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if r.OperatorClient == nil {
		logger.Info("No operator client configured, simulating preemption")
		return nil
	}

	// WHY: 오케스트레이터 validateRequest는 target_amount가 비면 400을 반환한다.
	//      MinPriority도 0이면 일반 Pod(priority=0)을 후보 제외하므로 데모에선 1 이상 필수.
	targetAmount := strings.TrimSpace(policy.Spec.Parameters["targetAmount"])
	if targetAmount == "" {
		targetAmount = strings.TrimSpace(policy.Spec.Parameters["target_amount"])
	}
	resp, err := r.OperatorClient.StartPreemption(ctx, &operator.PreemptionRequest{
		NodeName:            strings.TrimSpace(policy.Spec.TargetNode),
		Namespace:           strings.TrimSpace(policy.Spec.TargetNamespace),
		ResourceType:        preemptionResourceType(policy.Spec.ResourceType),
		TargetAmount:        targetAmount,
		Reason:              policy.Spec.Reason,
		Strategy:            paramStr(policy, "strategy", ""),
		MinPriority:         paramInt32(policy, "minPriority", 0),
		MaxPodsToPreempt:    paramInt32(policy, "maxPodsToPreempt", 0),
		GracePeriodSeconds:  int64(paramInt32(policy, "gracePeriodSeconds", 0)),
		ProtectedNamespaces: parseCSV(paramStr(policy, "protectedNamespaces", "")),
	})
	if err != nil {
		return fmt.Errorf("failed to start preemption: %w", err)
	}
	logger.Info("Operator response received",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"operatorID", resp.PreemptionID,
		"operatorStatus", resp.Status,
	)

	if strings.EqualFold(resp.Status, "failed") {
		return fmt.Errorf("preemption reported failed: %s", resp.Message)
	}

	policy.Status.Result = fmt.Sprintf("type=preemption;id=%s;status=%s", resp.PreemptionID, resp.Status)
	if resp.Message != "" {
		policy.Status.Result += fmt.Sprintf(";msg=%s", truncate(resp.Message, 120))
	}
	logger.Info("Patching policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultBeforePatch", policy.Status.Result,
	)
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		st.Result = policy.Status.Result
	}); err != nil {
		logger.Error(err, "Failed to patch policy status.result",
			"name", policy.Name,
			"policyType", policy.Spec.PolicyType,
			"resultAttempted", policy.Status.Result,
		)
		return fmt.Errorf("failed to persist preemption result: %w", err)
	}
	logger.Info("Patched policy status.result",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"resultAfterPatch", policy.Status.Result,
	)

	logger.Info("Preemption initiated",
		"preemptionID", resp.PreemptionID,
		"status", resp.Status,
	)

	return nil
}

// findPodOnNode finds a pod running on the specified node
func (r *OrchestrationPolicyReconciler) findPodOnNode(ctx context.Context, nodeName, namespace string) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for _, pod := range podList.Items {
		if pod.Spec.NodeName == nodeName && pod.Status.Phase == corev1.PodRunning {
			return &pod, nil
		}
	}

	return nil, nil
}

func (r *OrchestrationPolicyReconciler) findNewMatchingTarget(ctx context.Context, policy *apollov1.OrchestrationPolicy) (*executionTarget, error) {
	namespace := strings.TrimSpace(policy.Spec.TargetNamespace)
	if namespace == "" {
		namespace = policy.Namespace
	}
	cutoff := policy.CreationTimestamp.Time

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for _, pod := range podList.Items {
		if !isCreatedAfterCutoff(pod.CreationTimestamp, cutoff) {
			continue
		}
		if !matchPolicyTargetMeta(policy, pod.GetName(), pod.GetLabels(), pod.GetAnnotations()) {
			continue
		}
		if hasAppliedTarget(policy, string(pod.UID)) {
			continue
		}
		return &executionTarget{
			UID:       pod.UID,
			Kind:      "Pod",
			Name:      pod.Name,
			Namespace: pod.Namespace,
		}, nil
	}

	deployList := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for _, deploy := range deployList.Items {
		if !isCreatedAfterCutoff(deploy.CreationTimestamp, cutoff) {
			continue
		}
		if !matchPolicyTargetMeta(policy, deploy.GetName(), deploy.GetLabels(), deploy.GetAnnotations()) {
			continue
		}
		if hasAppliedTarget(policy, string(deploy.UID)) {
			continue
		}
		return &executionTarget{
			UID:       deploy.UID,
			Kind:      "Deployment",
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		}, nil
	}

	workflowList := &unstructured.UnstructuredList{}
	workflowList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "WorkflowList",
	})
	if err := r.List(ctx, workflowList, client.InNamespace(namespace)); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return nil, err
		}
		return nil, nil
	}
	for _, workflow := range workflowList.Items {
		if !isCreatedAfterCutoff(workflow.GetCreationTimestamp(), cutoff) {
			continue
		}
		if !matchPolicyTargetMeta(policy, workflow.GetName(), workflow.GetLabels(), workflow.GetAnnotations()) {
			continue
		}
		if hasAppliedTarget(policy, string(workflow.GetUID())) {
			continue
		}
		return &executionTarget{
			UID:       workflow.GetUID(),
			Kind:      "Workflow",
			Name:      workflow.GetName(),
			Namespace: workflow.GetNamespace(),
		}, nil
	}

	return nil, nil
}

func isCreatedAfterCutoff(created metav1.Time, cutoff time.Time) bool {
	return created.Time.After(cutoff)
}

func matchPolicyTargetMeta(policy *apollov1.OrchestrationPolicy, name string, labels, annotations map[string]string) bool {
	if policy.Spec.ExecutionMode != apollov1.ExecutionModeOnTargetCreated {
		if workload := strings.TrimSpace(policy.Spec.TargetWorkload); workload != "" && workload != name {
			return false
		}
	}
	if !matchSelector(policy.Spec.Selector, labels) {
		return false
	}
	if !matchRunID(policy.Spec.TargetRunID, labels, annotations) {
		return false
	}
	if !matchTargetStage(policy.Spec.TargetStage, labels, annotations) {
		return false
	}
	return true
}

func matchSelector(selector map[string]string, labels map[string]string) bool {
	if len(selector) == 0 {
		return true
	}
	for key, expected := range selector {
		if labels == nil {
			return false
		}
		if labels[key] != expected {
			return false
		}
	}
	return true
}

func matchRunID(targetRunID string, labels, annotations map[string]string) bool {
	targetRunID = strings.TrimSpace(targetRunID)
	if targetRunID == "" {
		return true
	}
	keys := []string{"run_id", "runID"}
	return anyMetaValueMatches(keys, targetRunID, labels, annotations)
}

func matchTargetStage(targetStage string, labels, annotations map[string]string) bool {
	targetStage = strings.TrimSpace(targetStage)
	if targetStage == "" {
		return true
	}
	keys := []string{"target_stage", "targetStage", "stage"}
	return anyMetaValueMatches(keys, targetStage, labels, annotations)
}

func anyMetaValueMatches(keys []string, expected string, labels, annotations map[string]string) bool {
	for _, key := range keys {
		if labels != nil && labels[key] == expected {
			return true
		}
		if annotations != nil && annotations[key] == expected {
			return true
		}
	}
	return false
}

func hasAppliedTarget(policy *apollov1.OrchestrationPolicy, uid string) bool {
	for _, target := range policy.Status.AppliedTargets {
		if target.UID == uid {
			return true
		}
	}
	return false
}

// paramStr은 spec.parameters[key] 값을 trim 해 반환하고, 비어 있으면 fallback을 반환한다.
//
// Parameters:
//   - policy: 평가 대상 OrchestrationPolicy
//   - key: parameters 키
//   - fallback: 키가 없거나 빈 문자열일 때 사용할 기본값
//
// Returns:
//   - string: trim 된 값 또는 fallback
func paramStr(policy *apollov1.OrchestrationPolicy, key, fallback string) string {
	if policy == nil || policy.Spec.Parameters == nil {
		return fallback
	}
	v := strings.TrimSpace(policy.Spec.Parameters[key])
	if v == "" {
		return fallback
	}
	return v
}

// paramInt32는 spec.parameters[key]를 int32로 파싱한다. 잘못된 값이면 fallback.
func paramInt32(policy *apollov1.OrchestrationPolicy, key string, fallback int32) int32 {
	v := paramStr(policy, key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(n)
}

// paramBool은 "true"/"1"/"yes"를 true로, "false"/"0"/"no"를 false로 본다.
func paramBool(policy *apollov1.OrchestrationPolicy, key string, fallback bool) bool {
	v := strings.ToLower(paramStr(policy, key, ""))
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return fallback
	}
}

// parseCSV는 콤마 구분 문자열을 trim 된 슬라이스로 분해한다.
func parseCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func buildResourceSnapshot(policy *apollov1.OrchestrationPolicy, prefix string) string {
	cpu := paramStr(policy, prefix+"CPU", "")
	if cpu == "" {
		cpu = paramStr(policy, prefix+"_cpu", "")
	}
	memory := paramStr(policy, prefix+"Memory", "")
	if memory == "" {
		memory = paramStr(policy, prefix+"_memory", "")
	}
	if cpu == "" && memory == "" {
		if prefix == "before" {
			cpu = paramStr(policy, "resourceBeforeCPU", "1")
			memory = paramStr(policy, "resourceBeforeMemory", "2Gi")
		} else {
			cpu = paramStr(policy, "resourceAfterCPU", "2")
			memory = paramStr(policy, "resourceAfterMemory", "4Gi")
		}
	}
	parts := make([]string, 0, 2)
	if cpu != "" {
		parts = append(parts, "cpu="+cpu)
	}
	if memory != "" {
		parts = append(parts, "memory="+memory)
	}
	return strings.Join(parts, ",")
}

func formatSelectedPolicyResult(policy *apollov1.OrchestrationPolicy, policyType, id string, extras map[string]string) string {
	parts := []string{
		"type=" + policyType,
		"id=" + id,
		"selected_policy=" + policy.Name,
		"action_plan=" + policyType,
	}
	if extras != nil {
		keys := make([]string, 0, len(extras))
		for key := range extras {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := strings.TrimSpace(extras[key])
			if value == "" {
				continue
			}
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, ";")
}

// resolveMigrationPod는 정책 입력을 토대로 실제 Running Pod 1개와 그 노드를 해결한다.
//
// 해결 우선순위:
//  1. spec.parameters.podName 이 주어지면 그 Pod을 직접 사용.
//  2. spec.targetWorkload 가 실제 Pod 이름이면(Get 성공) 그대로 사용.
//  3. spec.targetWorkload 를 Deployment 이름으로 보고 selector → 첫 Running Pod 사용.
//  4. spec.targetNode 의 첫 Running Pod 사용.
//
// Parameters:
//   - ctx: reconcile context
//   - policy: 원본 OrchestrationPolicy
//   - namespace: 해결된 워크로드 네임스페이스
//
// Returns:
//   - string: Pod 이름
//   - string: Pod 이 실행 중인 노드 이름(sourceNode)
//   - error: 해결 실패 시 사유
func (r *OrchestrationPolicyReconciler) resolveMigrationPod(ctx context.Context, policy *apollov1.OrchestrationPolicy, namespace string) (string, string, error) {
	// 1) parameters.podName 우선
	if explicit := paramStr(policy, "podName", ""); explicit != "" {
		pod := &corev1.Pod{}
		key := types.NamespacedName{Namespace: namespace, Name: explicit}
		if err := r.Get(ctx, key, pod); err != nil {
			return "", "", fmt.Errorf("parameters.podName=%q not found in %s: %w", explicit, namespace, err)
		}
		return pod.Name, pod.Spec.NodeName, nil
	}

	tw := strings.TrimSpace(policy.Spec.TargetWorkload)

	// 2) targetWorkload가 Pod 이름이면 그대로
	if tw != "" {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tw}, pod); err == nil {
			return pod.Name, pod.Spec.NodeName, nil
		}
	}

	// 3) targetWorkload를 Deployment selector로 풀이
	if tw != "" {
		deploy := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tw}, deploy); err == nil && deploy.Spec.Selector != nil {
			sel, selErr := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
			if selErr == nil {
				podList := &corev1.PodList{}
				if listErr := r.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); listErr == nil {
					for _, p := range podList.Items {
						if p.Status.Phase == corev1.PodRunning {
							return p.Name, p.Spec.NodeName, nil
						}
					}
				}
			}
		}
	}

	// 4) targetNode 위 첫 Running Pod
	if policy.Spec.TargetNode != "" {
		pod, err := r.findPodOnNode(ctx, policy.Spec.TargetNode, namespace)
		if err != nil {
			return "", "", fmt.Errorf("failed to find pod on node %s: %w", policy.Spec.TargetNode, err)
		}
		if pod != nil {
			return pod.Name, pod.Spec.NodeName, nil
		}
	}

	return "", "", fmt.Errorf("no Running pod resolved for workload=%q on node=%q (ns=%s)", policy.Spec.TargetWorkload, policy.Spec.TargetNode, namespace)
}

// pickAlternateNode는 sourceNode 와 다른 Ready 워커 노드 1개를 결정적으로 선택한다.
//
// 선택 규칙:
//   - master/control-plane taint 가 있는 노드는 제외.
//   - 알파벳 순으로 정렬해 첫 번째 후보 사용(결정성 확보).
func (r *OrchestrationPolicyReconciler) pickAlternateNode(ctx context.Context, sourceNode string) (string, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return "", err
	}
	candidates := make([]string, 0, len(nodeList.Items))
	for _, n := range nodeList.Items {
		if n.Name == sourceNode {
			continue
		}
		if !isNodeReady(&n) || isControlPlaneNode(&n) {
			continue
		}
		candidates = append(candidates, n.Name)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", nil
	}
	return candidates[0], nil
}

// allowSameNodeMigration은 단일 노드 데모에서 migration 대상 노드 폴백을 허용한다.
func allowSameNodeMigration() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_SAME_NODE_MIGRATION"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isNodeReady는 NodeReady condition 이 True 인지 확인한다.
func isNodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isControlPlaneNode는 control-plane / master taint 또는 라벨이 붙은 노드인지 확인한다.
func isControlPlaneNode(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == "node-role.kubernetes.io/control-plane" || t.Key == "node-role.kubernetes.io/master" {
			return true
		}
	}
	if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	if _, ok := n.Labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	return false
}

func preemptionResourceType(rt apollov1.ResourceType) string {
	switch rt {
	case apollov1.ResourceTypeCPU:
		return "cpu"
	case apollov1.ResourceTypeMemory:
		return "memory"
	case apollov1.ResourceTypeGPU:
		return "gpu"
	case apollov1.ResourceTypeStorageIO:
		return "storage_iops"
	default:
		return "all"
	}
}

// handleRetryFromFailure는 annotation annRetryFromFailure=true일 때 Failed → Pending으로 되돌려 재실행 가능하게 한다.
func (r *OrchestrationPolicyReconciler) handleRetryFromFailure(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	base := policy.DeepCopy()
	if policy.Annotations == nil {
		policy.Annotations = map[string]string{}
	}
	delete(policy.Annotations, annRetryFromFailure)
	delete(policy.Annotations, annRollbackStatus)
	delete(policy.Annotations, annRollbackDetail)
	if err := r.Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	policy.Status.Phase = apollov1.PhasePending
	policy.Status.Message = "retry requested from Failed state"
	policy.Status.Result = ""
	policy.Status.ExecutedAt = nil
	policy.Status.CompletedAt = nil
	policy.Status.Conditions = nil
	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("Policy reset to Pending for retry", "name", policy.Name)
	return ctrl.Result{Requeue: true}, nil
}

// markPolicyCompleted marks a policy as completed
func (r *OrchestrationPolicyReconciler) markPolicyCompleted(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	now := metav1.Now()
	if policy.Spec.ExecutionMode == apollov1.ExecutionModeContinuous {
		policy.Status.Phase = apollov1.PhaseApproved
		policy.Status.CompletedAt = nil
	} else {
		policy.Status.Phase = apollov1.PhaseApplied
		policy.Status.CompletedAt = &now
		policy.Status.Consumed = true
	}
	if policy.Status.Result != "" {
		policy.Status.Message = fmt.Sprintf("Policy executed successfully; result=%s", truncate(policy.Status.Result, 120))
	} else {
		policy.Status.Message = "Policy executed successfully"
		policy.Status.Result = fmt.Sprintf("Policy %s completed at %s", policy.Spec.PolicyType, now.Format(time.RFC3339))
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ExecutionCompleted",
		Message:            "Policy execution completed successfully",
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	duration := "N/A"
	if policy.Status.ExecutedAt != nil {
		duration = now.Sub(policy.Status.ExecutedAt.Time).String()
	}

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            POLICY EXECUTION COMPLETED                         ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Policy:       " + fmt.Sprintf("%-47s", policy.Name) + " ║")
	logger.Info("║ Type:         " + fmt.Sprintf("%-47s", string(policy.Spec.PolicyType)) + " ║")
	logger.Info("║ Duration:     " + fmt.Sprintf("%-47s", duration) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	if policy.Spec.ExecutionMode == apollov1.ExecutionModeContinuous {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// markPolicyFailed marks a policy as failed
func (r *OrchestrationPolicyReconciler) markPolicyFailed(ctx context.Context, policy *apollov1.OrchestrationPolicy, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	resultBeforeFailure := policy.Status.Result
	rbStatus, rbDetail := r.attemptOperatorRollback(ctx, policy, resultBeforeFailure)
	detail := rbDetail
	if len(detail) > 200 {
		detail = detail[:197] + "..."
	}
	if err := r.patchRollbackMeta(ctx, policy, rbStatus, detail); err != nil {
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	policy.Status.Phase = apollov1.PhaseFailed
	policy.Status.CompletedAt = &now
	policy.Status.Message = reason
	policy.Status.Result = fmt.Sprintf("Policy failed: %s", reason)

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ExecutionFailed",
		Message:            reason,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.patchStatusWithRetry(ctx, policy, func(st *apollov1.OrchestrationPolicyStatus) {
		*st = policy.Status
	}); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	logger.Error(errors.New(reason), "POLICY_EXECUTION_FAILED",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
		"phase", policy.Status.Phase,
		"result", policy.Status.Result,
	)

	logger.Info("╔═══════════════════════════════════════════════════════════════╗")
	logger.Info("║            POLICY EXECUTION FAILED                            ║")
	logger.Info("╠═══════════════════════════════════════════════════════════════╣")
	logger.Info("║ Policy:       " + fmt.Sprintf("%-47s", policy.Name) + " ║")
	logger.Info("║ Reason:       " + fmt.Sprintf("%-47s", truncate(reason, 47)) + " ║")
	logger.Info("╚═══════════════════════════════════════════════════════════════╝")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrchestrationPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apollov1.OrchestrationPolicy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Named("orchestrationpolicy").
		Complete(r)
}

func (r *OrchestrationPolicyReconciler) patchStatusWithRetry(
	ctx context.Context,
	policy *apollov1.OrchestrationPolicy,
	mutate func(*apollov1.OrchestrationPolicyStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apollov1.OrchestrationPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(policy), latest); err != nil {
			return err
		}
		base := latest.DeepCopy()
		mutate(&latest.Status)
		if err := r.Status().Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		policy.Status = latest.Status
		policy.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

func (r *OrchestrationPolicyReconciler) patchSpecWithRetry(
	ctx context.Context,
	policy *apollov1.OrchestrationPolicy,
	mutate func(*apollov1.OrchestrationPolicySpec),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &apollov1.OrchestrationPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(policy), latest); err != nil {
			return err
		}
		base := latest.DeepCopy()
		mutate(&latest.Spec)
		if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		policy.Spec = latest.Spec
		policy.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// truncate truncates a string to the specified length
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
