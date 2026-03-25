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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apollov1 "orchestration-policy-engine/api/v1"
	"orchestration-policy-engine/internal/operator"
)

// OrchestrationPolicyReconciler reconciles a OrchestrationPolicy object
type OrchestrationPolicyReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	OperatorClient *operator.Client
}

// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apollo.keti.re.kr,resources=orchestrationpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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

	case apollov1.PhaseCompleted, apollov1.PhaseFailed, apollov1.PhaseRejected:
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

	if err := r.Status().Update(ctx, policy); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	logger.Info("Policy status updated to Pending",
		"name", policy.Name,
	)

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleAutoApprove automatically approves a policy
func (r *OrchestrationPolicyReconciler) handleAutoApprove(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("========================================")
	logger.Info("AUTO-APPROVING POLICY",
		"name", policy.Name,
		"policyType", policy.Spec.PolicyType,
	)
	logger.Info("========================================")

	// Update to Approved
	policy.Status.Phase = apollov1.PhaseApproved
	policy.Status.Message = "Policy auto-approved based on autoExecute flag"

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Approved",
		Status:             metav1.ConditionTrue,
		Reason:             "AutoApproved",
		Message:            "Policy was automatically approved",
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, policy); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	// Requeue immediately to start execution
	return ctrl.Result{Requeue: true}, nil
}

// handleApprovedPolicy executes an approved policy
func (r *OrchestrationPolicyReconciler) handleApprovedPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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
	policy.Status.Message = fmt.Sprintf("Executing %s policy", policy.Spec.PolicyType)

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Executing",
		Status:             metav1.ConditionTrue,
		Reason:             "ExecutionStarted",
		Message:            fmt.Sprintf("Started executing %s policy", policy.Spec.PolicyType),
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, policy); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

	// Execute based on policy type
	var execErr error
	switch policy.Spec.PolicyType {
	case apollov1.PolicyTypeMigration:
		execErr = r.executeMigrationPolicy(ctx, policy)
	case apollov1.PolicyTypeScaling:
		execErr = r.executeScalingPolicy(ctx, policy)
	case apollov1.PolicyTypeProvisioning:
		execErr = r.executeProvisioningPolicy(ctx, policy)
	case apollov1.PolicyTypeCaching:
		execErr = r.executeCachingPolicy(ctx, policy)
	case apollov1.PolicyTypeLoadbalance:
		execErr = r.executeLoadbalancePolicy(ctx, policy)
	case apollov1.PolicyTypePreemption:
		execErr = r.executePreemptionPolicy(ctx, policy)
	default:
		logger.Info("Unknown policy type, marking as failed",
			"policyType", policy.Spec.PolicyType,
		)
		return r.markPolicyFailed(ctx, policy, "Unknown policy type")
	}

	if execErr != nil {
		logger.Error(execErr, "Policy execution failed")
		return r.markPolicyFailed(ctx, policy, execErr.Error())
	}

	// Requeue to monitor execution
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// handleExecutingPolicy monitors an executing policy
func (r *OrchestrationPolicyReconciler) handleExecutingPolicy(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Monitoring executing policy",
		"name", policy.Name,
		"executedAt", policy.Status.ExecutedAt,
	)

	// Check execution status from operator
	if r.OperatorClient != nil && policy.Status.Result != "" {
		// Check migration status if it's a migration policy
		if policy.Spec.PolicyType == apollov1.PolicyTypeMigration {
			status, err := r.OperatorClient.GetMigrationStatus(ctx, policy.Status.Result)
			if err != nil {
				logger.Error(err, "Failed to get migration status")
			} else {
				logger.Info("Migration status",
					"migrationID", policy.Status.Result,
					"status", status.Status,
				)

				if status.Status == "completed" {
					return r.markPolicyCompleted(ctx, policy)
				} else if status.Status == "failed" {
					return r.markPolicyFailed(ctx, policy, status.Message)
				}
			}
		}
	}

	// Timeout check (30 minutes max execution time)
	if policy.Status.ExecutedAt != nil {
		elapsed := time.Since(policy.Status.ExecutedAt.Time)
		if elapsed > 30*time.Minute {
			return r.markPolicyFailed(ctx, policy, "Execution timeout (30 minutes)")
		}

		// For demo/testing: mark as completed after 30 seconds if no operator client
		if r.OperatorClient == nil && elapsed > 30*time.Second {
			return r.markPolicyCompleted(ctx, policy)
		}
	}

	// Requeue to check again
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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

	// Find a pod to migrate if not specified
	targetWorkload := policy.Spec.TargetWorkload
	targetNamespace := policy.Spec.TargetNamespace
	if targetNamespace == "" {
		targetNamespace = policy.Namespace
	}

	if targetWorkload == "" {
		// Find a pod on the target node
		pod, err := r.findPodOnNode(ctx, policy.Spec.TargetNode, targetNamespace)
		if err != nil {
			return fmt.Errorf("failed to find pod for migration: %w", err)
		}
		if pod == nil {
			return fmt.Errorf("no suitable pod found for migration on node %s", policy.Spec.TargetNode)
		}
		targetWorkload = pod.Name
	}

	// Call operator to start migration
	resp, err := r.OperatorClient.StartMigration(ctx, &operator.MigrationRequest{
		PodName:      targetWorkload,
		PodNamespace: targetNamespace,
		SourceNode:   policy.Spec.SourceNode,
		TargetNode:   policy.Spec.TargetNode,
		PreservePV:   policy.Spec.Parameters["preservePV"] == "true",
		Timeout:      600,
	})
	if err != nil {
		return fmt.Errorf("failed to start migration: %w", err)
	}

	// Store migration ID for status tracking
	policy.Status.Result = resp.MigrationID
	if err := r.Status().Update(ctx, policy); err != nil {
		return fmt.Errorf("failed to update policy with migration ID: %w", err)
	}

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

	resp, err := r.OperatorClient.ConfigureAutoscaling(ctx, &operator.AutoscalingRequest{
		WorkloadName:      policy.Spec.TargetWorkload,
		WorkloadNamespace: policy.Spec.TargetNamespace,
		WorkloadType:      "Deployment",
		MinReplicas:       1,
		MaxReplicas:       5,
		TargetCPU:         80,
		TargetMemory:      80,
	})
	if err != nil {
		return fmt.Errorf("failed to configure autoscaling: %w", err)
	}

	logger.Info("Autoscaling configured",
		"autoscalingID", resp.AutoscalingID,
		"status", resp.Status,
	)

	return nil
}

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

	resp, err := r.OperatorClient.StartProvisioning(ctx, &operator.ProvisioningRequest{
		WorkloadName:      policy.Spec.TargetWorkload,
		WorkloadNamespace: policy.Spec.TargetNamespace,
		WorkloadType:      "training",
		StorageSize:       "100Gi",
		StorageClass:      "high-throughput",
	})
	if err != nil {
		return fmt.Errorf("failed to start provisioning: %w", err)
	}

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

	resp, err := r.OperatorClient.ConfigureCaching(ctx, &operator.CachingRequest{
		SourcePVC:       policy.Spec.TargetWorkload + "-pvc",
		SourceNamespace: policy.Spec.TargetNamespace,
		TargetTier:      "nvme",
		CachePolicy:     "lru",
		Priority:        int32(policy.Spec.PriorityScore),
		Prefetch:        true,
		Reason:          policy.Spec.Reason,
	})
	if err != nil {
		return fmt.Errorf("failed to configure caching: %w", err)
	}

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

	resp, err := r.OperatorClient.ConfigureLoadbalance(ctx, &operator.LoadbalanceRequest{
		TargetNode: policy.Spec.TargetNode,
		Strategy:   "least-connections",
		Reason:     policy.Spec.Reason,
	})
	if err != nil {
		return fmt.Errorf("failed to configure loadbalance: %w", err)
	}

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

	resp, err := r.OperatorClient.StartPreemption(ctx, &operator.PreemptionRequest{
		WorkloadName:      policy.Spec.TargetWorkload,
		WorkloadNamespace: policy.Spec.TargetNamespace,
		Priority:          int32(policy.Spec.PriorityScore),
		Reason:            policy.Spec.Reason,
	})
	if err != nil {
		return fmt.Errorf("failed to start preemption: %w", err)
	}

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

// markPolicyCompleted marks a policy as completed
func (r *OrchestrationPolicyReconciler) markPolicyCompleted(ctx context.Context, policy *apollov1.OrchestrationPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	now := metav1.Now()
	policy.Status.Phase = apollov1.PhaseCompleted
	policy.Status.CompletedAt = &now
	policy.Status.Message = "Policy executed successfully"
	if policy.Status.Result == "" {
		policy.Status.Result = fmt.Sprintf("Policy %s completed at %s", policy.Spec.PolicyType, now.Format(time.RFC3339))
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ExecutionCompleted",
		Message:            "Policy execution completed successfully",
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, policy); err != nil {
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

	return ctrl.Result{}, nil
}

// markPolicyFailed marks a policy as failed
func (r *OrchestrationPolicyReconciler) markPolicyFailed(ctx context.Context, policy *apollov1.OrchestrationPolicy, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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

	if err := r.Status().Update(ctx, policy); err != nil {
		logger.Error(err, "Failed to update policy status")
		return ctrl.Result{}, err
	}

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
	return ctrl.NewControllerManagedBy(mgr).
		For(&apollov1.OrchestrationPolicy{}).
		Named("orchestrationpolicy").
		Complete(r)
}

// truncate truncates a string to the specified length
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
