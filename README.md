# APOLLO - Adaptive Policy Optimization for Low-Latency I/O

KETI AI Storage System의 정책 최적화 엔진 - **RL 기반 적응형 스케줄링/오케스트레이션**

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              APOLLO Architecture                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  ┌──────────────────┐     ┌──────────────────┐     ┌────────────────────────┐   │
│  │   Insight Scope  │────▶│   Insight Trace  │────▶│    APOLLO Policy       │   │
│  │   (외부 폴더)     │     │   (외부 폴더)     │     │       Engines          │   │
│  │                  │     │                  │     │                        │   │
│  │ - YAML Analysis  │     │ - Runtime Trace  │     │  ┌──────────────────┐  │   │
│  │ - Model Detection│     │ - GPU Metrics    │     │  │Scheduling Policy │  │   │
│  │ - DAG Analysis   │     │ - I/O Patterns   │     │  │ Engine (PPO RL)  │  │   │
│  │ - Cluster Status │     │ - Stage Detection│     │  │ Port: 50054      │  │   │
│  └────────┬─────────┘     └────────┬─────────┘     │  └──────────────────┘  │   │
│           │                        │               │                        │   │
│           │    WorkloadSignature   │               │  ┌──────────────────┐  │   │
│           └────────────────────────┼──────────────▶│  │Node Resource     │  │   │
│                                    │               │  │Forecaster (LSTM) │  │   │
│                                    │               │  │ Port: 50055      │  │   │
│                                    │               │  └──────────────────┘  │   │
│                                    │               │                        │   │
│                                    │               │  ┌──────────────────┐  │   │
│                                    └──────────────▶│  │Orchestration     │  │   │
│                                                    │  │Policy (DQN/AC)   │  │   │
│                                                    │  │ Port: 50056      │  │   │
│                                                    │  └──────────────────┘  │   │
│                                                    └────────────────────────┘   │
│                                                               │                 │
│                                                               ▼                 │
│  ┌───────────────────────────────────────────────────────────────────────────┐ │
│  │                         AI-Storage Scheduler                               │ │
│  │  Plugins: DataLocalityAware | StorageTierAware | IOPatternBased           │ │
│  │           KueueAware | PipelineStageAware                                  │ │
│  │           ← 가중치는 Scheduling Policy Engine (PPO)에서 실시간 제공         │ │
│  └───────────────────────────────────────────────────────────────────────────┘ │
│                                         │                                       │
│                                         ▼                                       │
│  ┌───────────────────────────────────────────────────────────────────────────┐ │
│  │                        AI-Storage Orchestrator                             │ │
│  │  Actions: Migration | ScaleUp/Down | CacheWarmup | TierMigrate | Preempt  │ │
│  │           ← 결정은 Orchestration Policy Engine (DQN)에서 제공              │ │
│  └───────────────────────────────────────────────────────────────────────────┘ │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## Modules

### 1. Insight Scope (`/root/workspace/insight-scope/`) - 기존 구현 사용
**YAML 분석 및 AI 모델 탐지**
- Pod YAML 정적 분석
- AI 프레임워크/모델 탐지 (PyTorch, TensorFlow, etc.)
- Argo Workflow DAG 분석
- 클러스터 상태 수집

### 2. Insight Trace (`/root/workspace/insight-trace/`) - 기존 구현 사용
**런타임 메트릭 수집 및 APOLLO 연동**
- 실시간 리소스 사용량 모니터링
- GPU 메트릭 수집 (DCGM 연동)
- I/O 패턴 탐지
- APOLLO로 WorkloadSignature 전송 (구현 완료)

### 3. Node Resource Forecaster (`apollo/node-resource-forecaster/`)
**LSTM 기반 노드 리소스 예측**
- 다중 Horizon 예측 (5/10/30/60분)
- 피크/유휴 시간대 예측
- 배치 작업 최적 실행 시간 추천
- 온라인 학습 지원

### 4. Scheduling Policy Engine (`apollo/scheduling-policy-engine/`)
**PPO (Proximal Policy Optimization) RL 기반**
- Feature Encoders: Workload, Pipeline, Cluster
- Actor-Critic Network
- 5개 스케줄러 플러그인 가중치 출력
  - DataLocalityAware, StorageTierAware, IOPatternBased
  - KueueAware, PipelineStageAware
- 실행 결과 피드백으로 온라인 학습

### 5. Orchestration Policy Engine (`apollo/orchestration-policy-engine/`)
**DQN (Deep Q-Network) / Actor-Critic 기반**
- Migration 결정: 어느 노드로 마이그레이션할지
- Scaling 결정: 레플리카 수 조정
- Caching 정책: L0(Memory)/L1(NVMe)/L2(SSD) 결정
- Storage Tier Migration 결정
- Preemption 결정

## Data Flow

```
[Insight Trace] ─────> WorkloadSignature ─────> APOLLO
    (Sidecar)           (gRPC Stream)           │
                                                v
[Insight Scope] ─────> ClusterInsight ───────> Policy Engines
   (DaemonSet)          (gRPC Stream)           │
                                                v
                                    ┌───────────┴───────────┐
                                    v                       v
                            SchedulingPolicy        OrchestrationPolicy
                                    │                       │
                                    v                       v
                            AI-Storage Scheduler    AI-Storage Orchestrator
```

## Project Structure

```
apollo/
├── api/
│   └── proto/
│       └── apollo.proto        # gRPC 프로토콜 정의
├── cmd/
│   └── main.go                 # 메인 엔트리포인트
├── pkg/
│   ├── forecaster/             # Node Resource Forecaster
│   │   └── forecast_service.go
│   ├── scheduling/             # Scheduling Policy Engine
│   │   ├── scheduling_service.go
│   │   └── scheduler_client.go
│   ├── orchestration/          # Orchestration Policy Engine
│   │   ├── orchestration_service.go
│   │   └── orchestrator_client.go
│   ├── insight/                # Insight 데이터 수신
│   │   └── insight_service.go
│   └── types/
│       └── types.go            # 공통 타입 정의
├── deployments/                # Kubernetes 배포 매니페스트
└── scripts/
    ├── build.sh
    ├── deploy.sh
    └── generate-proto.sh
```

## gRPC Services

### InsightService (데이터 수집)
```protobuf
service InsightService {
  rpc ReportWorkloadSignature(WorkloadSignature) returns (ReportResponse);
  rpc StreamWorkloadSignatures(stream WorkloadSignature) returns (StreamResponse);
  rpc ReportClusterInsight(ClusterInsight) returns (ReportResponse);
  rpc StreamClusterInsights(stream ClusterInsight) returns (StreamResponse);
}
```

### SchedulingPolicyService
```protobuf
service SchedulingPolicyService {
  rpc GetSchedulingPolicy(SchedulingPolicyRequest) returns (SchedulingPolicy);
  rpc ReportSchedulingResult(SchedulingResult) returns (ReportResponse);
}
```

### OrchestrationPolicyService
```protobuf
service OrchestrationPolicyService {
  rpc SubscribePolicies(PolicySubscription) returns (stream OrchestrationPolicy);
  rpc GetOrchestrationPolicy(OrchestrationPolicyRequest) returns (OrchestrationPolicy);
  rpc ReportExecutionResult(ExecutionResult) returns (ReportResponse);
}
```

## Build & Deploy

```bash
# Build
./scripts/build.sh

# Deploy to Kubernetes
./scripts/deploy.sh

# Generate protobuf
./scripts/generate-proto.sh
```

## Related Projects

- [AI-Storage-Scheduler](https://github.com/KETI-AI-Storage/ai-storage-scheduler) - Custom K8s Scheduler
- [AI-Storage-Orchestrator](https://github.com/KETI-AI-Storage/ai-storage-orchestrator) - Pod Migration Controller
- [Insight-Trace](https://github.com/KETI-AI-Storage/insight-trace) - Workload Signature Collector (Sidecar)
- [Insight-Scope](https://github.com/KETI-AI-Storage/insight-scope) - Cluster State Collector (DaemonSet)

## Research

This project is supported by IITP (Institute of Information & Communications Technology Planning & Evaluation), funded by Korea government (MSIT):
- Grant No. RS-2024-00461572
- Project: "Development of High-efficiency Parallel Storage SW Technology Optimized for AI Computational Accelerators"

