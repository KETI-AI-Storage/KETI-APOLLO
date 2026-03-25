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
// +kubebuilder:validation:Enum=Pending;Approved;Executing;Completed;Failed;Rejected
type ExecutionPhase string

const (
	PhasePending   ExecutionPhase = "Pending"   // 생성됨, 승인 대기
	PhaseApproved  ExecutionPhase = "Approved"  // 승인됨, 실행 대기
	PhaseExecuting ExecutionPhase = "Executing" // 실행 중
	PhaseCompleted ExecutionPhase = "Completed" // 완료
	PhaseFailed    ExecutionPhase = "Failed"    // 실패
	PhaseRejected  ExecutionPhase = "Rejected"  // 거부됨
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

	// Conditions 상태 조건들
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
