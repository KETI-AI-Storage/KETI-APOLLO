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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ============================================================
// Enum 타입 정의
// ============================================================

// PolicyType 정책 타입 (6가지 오퍼레이터에 대응)
// +kubebuilder:validation:Enum=migration;scaling;provisioning;caching;loadbalance;preemption
type PolicyType string

const (
	PolicyTypeMigration    PolicyType = "migration"    // 티어 마이그레이션
	PolicyTypeScaling      PolicyType = "scaling"      // 오토스케일링
	PolicyTypeProvisioning PolicyType = "provisioning" // 사전 프로비저닝
	PolicyTypeCaching      PolicyType = "caching"      // 글로벌 캐싱
	PolicyTypeLoadbalance  PolicyType = "loadbalance"  // 로드밸런싱
	PolicyTypePreemption   PolicyType = "preemption"   // 선점
)

// Urgency 긴급도
// +kubebuilder:validation:Enum=LOW;MEDIUM;HIGH;CRITICAL
type Urgency string

const (
	UrgencyLow      Urgency = "LOW"
	UrgencyMedium   Urgency = "MEDIUM"
	UrgencyHigh     Urgency = "HIGH"
	UrgencyCritical Urgency = "CRITICAL"
)

// ResourceType 리소스 타입
// +kubebuilder:validation:Enum=CPU;MEMORY;GPU;STORAGE_IO;NETWORK
type ResourceType string

const (
	ResourceTypeCPU       ResourceType = "CPU"
	ResourceTypeMemory    ResourceType = "MEMORY"
	ResourceTypeGPU       ResourceType = "GPU"
	ResourceTypeStorageIO ResourceType = "STORAGE_IO"
	ResourceTypeNetwork   ResourceType = "NETWORK"
)

// ExecutionPhase 실행 단계
// +kubebuilder:validation:Enum=Pending;Approved;Executing;WaitingForTarget;Applied;Completed;Failed;Rejected
type ExecutionPhase string

const (
	PhasePending          ExecutionPhase = "Pending"          // 생성됨, 승인 대기
	PhaseApproved         ExecutionPhase = "Approved"         // 승인됨, 실행 대기
	PhaseExecuting        ExecutionPhase = "Executing"        // 실행 중
	PhaseWaitingForTarget ExecutionPhase = "WaitingForTarget" // 타깃 생성 대기
	PhaseApplied          ExecutionPhase = "Applied"          // 1회 적용 완료
	PhaseCompleted        ExecutionPhase = "Completed"        // 완료
	PhaseFailed           ExecutionPhase = "Failed"           // 실패
	PhaseRejected         ExecutionPhase = "Rejected"         // 거부됨
)

// ExecutionMode 정책 실행 모드
// +kubebuilder:validation:Enum=Immediate;OnTargetCreated;Continuous
type ExecutionMode string

const (
	ExecutionModeImmediate       ExecutionMode = "Immediate"       // 현재 대상에 즉시 적용
	ExecutionModeOnTargetCreated ExecutionMode = "OnTargetCreated" // 새 대상 생성 시 1회 적용
	ExecutionModeContinuous      ExecutionMode = "Continuous"      // 반복 적용
)

// ============================================================
// Spec 정의 (원하는 상태)
// ============================================================

// OrchestrationPolicySpec 정책의 상세 내용
type OrchestrationPolicySpec struct {

	// PolicyType 정책 타입 (migration, scaling, provisioning 등)
	// +kubebuilder:validation:Required
	PolicyType PolicyType `json:"policyType"`

	// Probability 이 정책의 신뢰도 (0 ~ 100, 퍼센트)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Probability int32 `json:"probability"`

	// Urgency 긴급도
	// +kubebuilder:validation:Required
	Urgency Urgency `json:"urgency"`

	// ResourceType 주요 리소스 타입
	// +optional
	ResourceType ResourceType `json:"resourceType,omitempty"`

	// TargetNode 대상 노드 (마이그레이션 목적지, 프로비저닝 대상 등)
	// +optional
	TargetNode string `json:"targetNode,omitempty"`

	// SourceNode 소스 노드 (마이그레이션 출발지)
	// +optional
	SourceNode string `json:"sourceNode,omitempty"`

	// TargetWorkload 대상 워크로드 (Pod/Deployment 이름)
	// +optional
	TargetWorkload string `json:"targetWorkload,omitempty"`

	// TargetNamespace 대상 네임스페이스
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// Reason 정책 생성 이유
	// +kubebuilder:validation:Required
	Reason string `json:"reason"`

	// Horizon 예측 시간 범위 (분 단위, 예: 30 = 30분 후)
	// +optional
	Horizon int32 `json:"horizon,omitempty"`

	// AutoExecute 자동 실행 여부 (기본값: false)
	// +kubebuilder:default=false
	// +optional
	AutoExecute bool `json:"autoExecute,omitempty"`

	// ExecutionMode 실행 모드 (기본값: Immediate)
	// +kubebuilder:default=Immediate
	// +optional
	ExecutionMode ExecutionMode `json:"executionMode,omitempty"`

	// TargetRunID OnTargetCreated 매칭 시 사용할 Run 식별자
	// +optional
	TargetRunID string `json:"targetRunID,omitempty"`

	// TargetStage OnTargetCreated 매칭 시 사용할 Stage 식별자
	// +optional
	TargetStage string `json:"targetStage,omitempty"`

	// Selector OnTargetCreated 매칭 시 사용할 라벨 선택자
	// +optional
	Selector map[string]string `json:"selector,omitempty"`

	// PriorityScore 우선순위 점수 (높을수록 중요)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	PriorityScore int32 `json:"priorityScore,omitempty"`

	// Parameters 추가 파라미터 (오퍼레이터별 상세 설정)
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// ============================================================
// Status 정의 (현재 상태)
// ============================================================

// OrchestrationPolicyStatus 정책의 현재 실행 상태
type OrchestrationPolicyStatus struct {

	// Phase 현재 실행 단계
	// +optional
	Phase ExecutionPhase `json:"phase,omitempty"`

	// Message 상태 메시지
	// +optional
	Message string `json:"message,omitempty"`

	// ExecutedAt 실행 시작 시간
	// +optional
	ExecutedAt *metav1.Time `json:"executedAt,omitempty"`

	// CompletedAt 실행 완료 시간
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ExecutedBy 실행한 오퍼레이터 이름
	// +optional
	ExecutedBy string `json:"executedBy,omitempty"`

	// Result 실행 결과 상세
	// +optional
	Result string `json:"result,omitempty"`

	// Consumed 1회성 정책이 이미 소비되었는지 여부
	// +optional
	Consumed bool `json:"consumed,omitempty"`

	// AppliedTargets 실제 적용 대상 이력
	// +optional
	AppliedTargets []TargetReference `json:"appliedTargets,omitempty"`

	// Conditions 상태 조건들
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TargetReference는 정책이 실제로 적용된 타깃의 식별 정보를 담는다.
type TargetReference struct {
	// UID 타깃 리소스 UID
	UID string `json:"uid,omitempty"`

	// Kind 타깃 리소스 Kind
	Kind string `json:"kind,omitempty"`

	// Name 타깃 리소스 이름
	Name string `json:"name,omitempty"`

	// Namespace 타깃 리소스 네임스페이스
	Namespace string `json:"namespace,omitempty"`

	// AppliedAt 적용 시각
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// ============================================================
// CRD 메인 구조체
// ============================================================

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.policyType"
// +kubebuilder:printcolumn:name="Urgency",type="string",JSONPath=".spec.urgency"
// +kubebuilder:printcolumn:name="Prob",type="number",JSONPath=".spec.probability"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetNode"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// OrchestrationPolicy KETI 오케스트레이션 정책 CRD
// Forecaster 예측 결과를 기반으로 Policy Engine이 생성
type OrchestrationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OrchestrationPolicySpec   `json:"spec,omitempty"`
	Status OrchestrationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OrchestrationPolicyList OrchestrationPolicy 목록
type OrchestrationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OrchestrationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OrchestrationPolicy{}, &OrchestrationPolicyList{})
}
