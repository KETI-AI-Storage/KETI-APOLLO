"""
============================================
PPO (Proximal Policy Optimization) Model
APOLLO Scheduling Policy Engine의 핵심 RL 모델
============================================

이 모델은 워크로드 시그니처, 파이프라인 정보, 클러스터 상태를 입력받아
최적의 스케줄링 정책(플러그인 가중치)을 출력합니다.

Architecture:
- Feature Encoders: Workload, Pipeline, Cluster 각각 인코딩
- Feature Fusion: 인코딩된 특성 통합
- Actor Network: 정책 분포 생성
- Critic Network: 상태 가치 추정
"""

import torch
import torch.nn as nn
import torch.nn.functional as F
import numpy as np
from typing import Dict, Tuple, List, Optional
from dataclasses import dataclass


@dataclass
class PPOConfig:
    """PPO 하이퍼파라미터 설정"""
    # 네트워크 구조
    workload_embed_dim: int = 64
    pipeline_embed_dim: int = 32
    cluster_embed_dim: int = 64
    hidden_dim: int = 256

    # PPO 하이퍼파라미터
    gamma: float = 0.99           # 할인율
    gae_lambda: float = 0.95      # GAE lambda
    clip_epsilon: float = 0.2     # PPO clipping
    entropy_coef: float = 0.01    # 엔트로피 보너스
    value_coef: float = 0.5       # Value loss 계수
    max_grad_norm: float = 0.5    # Gradient clipping

    # 학습 설정
    learning_rate: float = 3e-4
    batch_size: int = 64
    epochs_per_update: int = 10

    # 출력 설정
    num_plugins: int = 5          # 플러그인 수


class WorkloadEncoder(nn.Module):
    """워크로드 시그니처 인코더"""

    def __init__(self, embed_dim: int = 64):
        super().__init__()

        # 워크로드 타입 임베딩
        self.workload_type_embed = nn.Embedding(10, 16)  # 10 types
        self.preprocessing_type_embed = nn.Embedding(10, 16)

        # I/O 패턴 인코더
        self.io_encoder = nn.Sequential(
            nn.Linear(7, 32),  # read_ratio, write_ratio, seq_ratio, etc.
            nn.ReLU(),
            nn.Linear(32, 32)
        )

        # 컴퓨트 프로파일 인코더
        self.compute_encoder = nn.Sequential(
            nn.Linear(6, 32),  # cpu, mem, gpu intensities + estimates
            nn.ReLU(),
            nn.Linear(32, 32)
        )

        # 최종 fusion
        self.fusion = nn.Sequential(
            nn.Linear(16 + 16 + 32 + 32 + 3, embed_dim),  # +3 for flags
            nn.ReLU(),
            nn.Linear(embed_dim, embed_dim)
        )

    def forward(self, workload: Dict[str, torch.Tensor]) -> torch.Tensor:
        """
        Args:
            workload: {
                'workload_type': (B,) int - 워크로드 타입 인덱스
                'preprocessing_type': (B,) int - 전처리 타입 인덱스
                'io_pattern': (B, 7) float - I/O 패턴 벡터
                'compute_profile': (B, 6) float - 컴퓨트 프로파일
                'flags': (B, 3) float - requires_gpu, requires_csd, data_size_normalized
            }
        Returns:
            (B, embed_dim) 워크로드 임베딩
        """
        wt_embed = self.workload_type_embed(workload['workload_type'])
        pt_embed = self.preprocessing_type_embed(workload['preprocessing_type'])
        io_embed = self.io_encoder(workload['io_pattern'])
        compute_embed = self.compute_encoder(workload['compute_profile'])

        combined = torch.cat([
            wt_embed, pt_embed, io_embed, compute_embed, workload['flags']
        ], dim=-1)

        return self.fusion(combined)


class PipelineEncoder(nn.Module):
    """파이프라인 정보 인코더"""

    def __init__(self, embed_dim: int = 32):
        super().__init__()

        # 파이프라인 위치 인코더
        self.position_encoder = nn.Sequential(
            nn.Linear(3, 16),  # current_index, total_stages, progress
            nn.ReLU()
        )

        # DAG 구조 인코더 (간단한 MLP 버전)
        self.dag_encoder = nn.Sequential(
            nn.Linear(4, 16),  # num_prev, num_next, depth, fan_out
            nn.ReLU()
        )

        # 데이터 지역성 인코더
        self.locality_encoder = nn.Sequential(
            nn.Linear(2, 16),  # has_prev_node, same_node_ratio
            nn.ReLU()
        )

        self.fusion = nn.Sequential(
            nn.Linear(48, embed_dim),
            nn.ReLU()
        )

    def forward(self, pipeline: Dict[str, torch.Tensor]) -> torch.Tensor:
        """
        Args:
            pipeline: {
                'position': (B, 3) float - 파이프라인 위치 정보
                'dag_structure': (B, 4) float - DAG 구조 정보
                'locality': (B, 2) float - 데이터 지역성 정보
            }
        Returns:
            (B, embed_dim) 파이프라인 임베딩
        """
        pos_embed = self.position_encoder(pipeline['position'])
        dag_embed = self.dag_encoder(pipeline['dag_structure'])
        loc_embed = self.locality_encoder(pipeline['locality'])

        combined = torch.cat([pos_embed, dag_embed, loc_embed], dim=-1)
        return self.fusion(combined)


class ClusterEncoder(nn.Module):
    """클러스터 상태 인코더"""

    def __init__(self, embed_dim: int = 64, max_nodes: int = 32):
        super().__init__()
        self.max_nodes = max_nodes

        # 노드 상태 인코더
        self.node_encoder = nn.Sequential(
            nn.Linear(10, 32),  # cpu, mem, gpu utils, storage tiers, etc.
            nn.ReLU(),
            nn.Linear(32, 32)
        )

        # 클러스터 레벨 집계: cluster_metrics (B, 8) 입력
        # [0]cpu_util [1]mem_util [2]total_nodes [3]gpu_available
        # [4]gpu_utilization [5]pending_pods [6]node_imbalance [7]storage_io_utilization
        self.cluster_agg = nn.Sequential(
            nn.Linear(8, 32),
            nn.ReLU()
        )

        # 큐 상태 인코더
        self.queue_encoder = nn.Sequential(
            nn.Linear(4, 16),  # pending, admitted, priority, etc.
            nn.ReLU()
        )

        # 최종 fusion (평균 풀링된 노드 + 클러스터 + 큐)
        self.fusion = nn.Sequential(
            nn.Linear(32 + 32 + 16, embed_dim),
            nn.ReLU()
        )

    def forward(self, cluster: Dict[str, torch.Tensor]) -> torch.Tensor:
        """
        Args:
            cluster: {
                'nodes': (B, max_nodes, 10) float - 노드별 상태
                'node_mask': (B, max_nodes) bool - 유효 노드 마스크
                'cluster_metrics': (B, 8) float - [0]cpu [1]mem [2]total_nodes [3]gpu_available
                    [4]gpu_utilization [5]pending_pods [6]node_imbalance [7]storage_io_utilization
                'queue_status': (B, 4) float - 큐 상태
            }
        Returns:
            (B, embed_dim) 클러스터 임베딩
        """
        # 노드 인코딩
        B, N, _ = cluster['nodes'].shape
        node_embeds = self.node_encoder(cluster['nodes'])  # (B, N, 32)

        # 마스크된 평균 풀링
        mask = cluster['node_mask'].unsqueeze(-1).float()  # (B, N, 1)
        node_sum = (node_embeds * mask).sum(dim=1)  # (B, 32)
        node_count = mask.sum(dim=1).clamp(min=1)  # (B, 1)
        node_agg = node_sum / node_count  # (B, 32)

        cluster_embed = self.cluster_agg(cluster['cluster_metrics'])
        queue_embed = self.queue_encoder(cluster['queue_status'])

        combined = torch.cat([node_agg, cluster_embed, queue_embed], dim=-1)
        return self.fusion(combined)


class ActorNetwork(nn.Module):
    """Actor Network - 정책 분포 생성"""

    def __init__(self, input_dim: int, hidden_dim: int, num_plugins: int):
        super().__init__()

        self.network = nn.Sequential(
            nn.Linear(input_dim, hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, hidden_dim),
            nn.ReLU(),
        )

        # 각 플러그인별 가중치 출력 (0-1 범위로 softmax)
        self.policy_head = nn.Linear(hidden_dim, num_plugins)

    def forward(self, features: torch.Tensor) -> torch.Tensor:
        """
        Args:
            features: (B, input_dim) 통합 특성
        Returns:
            (B, num_plugins) 정규화된 플러그인 가중치
        """
        hidden = self.network(features)
        logits = self.policy_head(hidden)
        # Softmax로 정규화 (합이 1이 되도록)
        weights = F.softmax(logits, dim=-1)
        return weights


class CriticNetwork(nn.Module):
    """Critic Network - 상태 가치 추정"""

    def __init__(self, input_dim: int, hidden_dim: int):
        super().__init__()

        self.network = nn.Sequential(
            nn.Linear(input_dim, hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, 1)
        )

    def forward(self, features: torch.Tensor) -> torch.Tensor:
        """
        Args:
            features: (B, input_dim) 통합 특성
        Returns:
            (B, 1) 상태 가치
        """
        return self.network(features)


class SchedulingPolicyPPO(nn.Module):
    """
    APOLLO Scheduling Policy PPO Model

    완전한 PPO 모델로, 인코더 + Actor + Critic으로 구성됩니다.
    """

    def __init__(self, config: PPOConfig = None):
        super().__init__()

        if config is None:
            config = PPOConfig()
        self.config = config

        # Feature Encoders
        self.workload_encoder = WorkloadEncoder(config.workload_embed_dim)
        self.pipeline_encoder = PipelineEncoder(config.pipeline_embed_dim)
        self.cluster_encoder = ClusterEncoder(config.cluster_embed_dim)

        # Feature Fusion
        total_embed_dim = (config.workload_embed_dim +
                          config.pipeline_embed_dim +
                          config.cluster_embed_dim)

        self.feature_fusion = nn.Sequential(
            nn.Linear(total_embed_dim, config.hidden_dim),
            nn.ReLU(),
            nn.Linear(config.hidden_dim, config.hidden_dim)
        )

        # Actor-Critic Networks
        self.actor = ActorNetwork(config.hidden_dim, config.hidden_dim, config.num_plugins)
        self.critic = CriticNetwork(config.hidden_dim, config.hidden_dim)

    def encode_state(self,
                     workload: Dict[str, torch.Tensor],
                     pipeline: Dict[str, torch.Tensor],
                     cluster: Dict[str, torch.Tensor]) -> torch.Tensor:
        """상태를 인코딩하여 통합 특성 벡터 반환"""
        workload_embed = self.workload_encoder(workload)
        pipeline_embed = self.pipeline_encoder(pipeline)
        cluster_embed = self.cluster_encoder(cluster)

        combined = torch.cat([workload_embed, pipeline_embed, cluster_embed], dim=-1)
        features = self.feature_fusion(combined)

        return features

    def forward(self,
                workload: Dict[str, torch.Tensor],
                pipeline: Dict[str, torch.Tensor],
                cluster: Dict[str, torch.Tensor]) -> Tuple[torch.Tensor, torch.Tensor]:
        """
        Forward pass - 정책과 가치 동시 계산

        Returns:
            policy: (B, num_plugins) 플러그인 가중치
            value: (B, 1) 상태 가치
        """
        features = self.encode_state(workload, pipeline, cluster)
        policy = self.actor(features)
        value = self.critic(features)

        return policy, value

    def get_action(self,
                   workload: Dict[str, torch.Tensor],
                   pipeline: Dict[str, torch.Tensor],
                   cluster: Dict[str, torch.Tensor],
                   deterministic: bool = False) -> Tuple[torch.Tensor, torch.Tensor, torch.Tensor]:
        """
        행동(정책) 샘플링

        Args:
            deterministic: True면 평균 정책 반환, False면 샘플링

        Returns:
            action: (B, num_plugins) 선택된 가중치
            log_prob: (B,) 로그 확률
            value: (B, 1) 상태 가치
        """
        policy, value = self.forward(workload, pipeline, cluster)

        if deterministic:
            action = policy
            # Deterministic일 때 log_prob는 0
            log_prob = torch.zeros(policy.shape[0], device=policy.device)
        else:
            # Dirichlet 분포에서 샘플링 (가중치 합이 1이 되도록)
            # 단순화: 정책 자체를 action으로 사용하고 약간의 노이즈 추가
            noise = torch.randn_like(policy) * 0.1
            action = F.softmax(policy + noise, dim=-1)

            # Log probability 근사
            log_prob = -F.kl_div(action.log(), policy, reduction='none').sum(dim=-1)

        return action, log_prob, value

    def evaluate_actions(self,
                        workload: Dict[str, torch.Tensor],
                        pipeline: Dict[str, torch.Tensor],
                        cluster: Dict[str, torch.Tensor],
                        actions: torch.Tensor) -> Tuple[torch.Tensor, torch.Tensor, torch.Tensor]:
        """
        주어진 행동에 대한 로그 확률, 가치, 엔트로피 계산 (학습용)

        Returns:
            log_prob: (B,) 로그 확률
            value: (B, 1) 상태 가치
            entropy: (B,) 엔트로피
        """
        policy, value = self.forward(workload, pipeline, cluster)

        # Log probability
        log_prob = -F.kl_div(actions.log(), policy, reduction='none').sum(dim=-1)

        # Entropy (정책 분포의 불확실성)
        entropy = -(policy * policy.log().clamp(min=-100)).sum(dim=-1)

        return log_prob, value, entropy


class PPOTrainer:
    """PPO 학습 관리자"""

    def __init__(self, model: SchedulingPolicyPPO, config: PPOConfig = None):
        self.model = model
        self.config = config or model.config

        self.optimizer = torch.optim.Adam(
            model.parameters(),
            lr=self.config.learning_rate
        )

        # Experience Buffer
        self.states = []
        self.actions = []
        self.rewards = []
        self.values = []
        self.log_probs = []
        self.dones = []

    def store_transition(self, state, action, reward, value, log_prob, done):
        """Experience 저장"""
        self.states.append(state)
        self.actions.append(action)
        self.rewards.append(reward)
        self.values.append(value)
        self.log_probs.append(log_prob)
        self.dones.append(done)

    def compute_gae(self, rewards, values, dones, next_value):
        """Generalized Advantage Estimation 계산"""
        advantages = []
        gae = 0

        for t in reversed(range(len(rewards))):
            if t == len(rewards) - 1:
                next_val = next_value
            else:
                next_val = values[t + 1]

            delta = rewards[t] + self.config.gamma * next_val * (1 - dones[t]) - values[t]
            gae = delta + self.config.gamma * self.config.gae_lambda * (1 - dones[t]) * gae
            advantages.insert(0, gae)

        returns = [adv + val for adv, val in zip(advantages, values)]

        return advantages, returns

    def update(self):
        """PPO 업데이트 수행"""
        if len(self.rewards) == 0 or len(self.states) == 0:
            return {}

        # Ensure all buffers have same length
        min_len = min(len(self.states), len(self.rewards), len(self.values),
                      len(self.log_probs), len(self.actions), len(self.dones))
        if min_len == 0:
            return {}

        # GAE 계산
        with torch.no_grad():
            # 마지막 상태의 가치 추정
            last_state = self.states[-1]
            _, next_value = self.model.forward(
                last_state['workload'],
                last_state['pipeline'],
                last_state['cluster']
            )
            next_value = next_value.item()

        advantages, returns = self.compute_gae(
            self.rewards, self.values, self.dones, next_value
        )

        # Tensor 변환
        advantages = torch.tensor(advantages, dtype=torch.float32)
        returns = torch.tensor(returns, dtype=torch.float32)
        old_log_probs = torch.stack(self.log_probs)
        actions = torch.stack(self.actions)

        # 정규화
        advantages = (advantages - advantages.mean()) / (advantages.std() + 1e-8)

        # 여러 epoch 학습
        metrics = {'policy_loss': 0, 'value_loss': 0, 'entropy': 0}

        for _ in range(self.config.epochs_per_update):
            for i in range(len(self.states)):
                state = self.states[i]

                log_prob, value, entropy = self.model.evaluate_actions(
                    state['workload'],
                    state['pipeline'],
                    state['cluster'],
                    actions[i:i+1]
                )

                # Policy Loss (PPO Clipping)
                ratio = torch.exp(log_prob - old_log_probs[i])
                surr1 = ratio * advantages[i]
                surr2 = torch.clamp(ratio,
                                   1 - self.config.clip_epsilon,
                                   1 + self.config.clip_epsilon) * advantages[i]
                policy_loss = -torch.min(surr1, surr2)

                # Value Loss
                value_squeezed = value.squeeze()
                return_target = returns[i]
                if value_squeezed.dim() == 0:
                    value_squeezed = value_squeezed.unsqueeze(0)
                    return_target = returns[i:i+1]
                value_loss = F.mse_loss(value_squeezed, return_target)

                # Total Loss
                loss = (policy_loss +
                       self.config.value_coef * value_loss -
                       self.config.entropy_coef * entropy)

                self.optimizer.zero_grad()
                loss.backward()
                torch.nn.utils.clip_grad_norm_(
                    self.model.parameters(),
                    self.config.max_grad_norm
                )
                self.optimizer.step()

                metrics['policy_loss'] += policy_loss.item()
                metrics['value_loss'] += value_loss.item()
                metrics['entropy'] += entropy.item()

        # Buffer 초기화
        self.states.clear()
        self.actions.clear()
        self.rewards.clear()
        self.values.clear()
        self.log_probs.clear()
        self.dones.clear()

        # 평균 계산
        n = len(self.states) * self.config.epochs_per_update
        if n > 0:
            for k in metrics:
                metrics[k] /= n

        return metrics

    def save(self, path: str):
        """모델 저장"""
        torch.save({
            'model_state_dict': self.model.state_dict(),
            'optimizer_state_dict': self.optimizer.state_dict(),
            'config': self.config
        }, path)

    def load(self, path: str):
        """모델 로드"""
        checkpoint = torch.load(path)
        self.model.load_state_dict(checkpoint['model_state_dict'])
        self.optimizer.load_state_dict(checkpoint['optimizer_state_dict'])


def calculate_reward(execution_result: Dict) -> float:
    """
    실행 결과를 기반으로 보상 계산

    목표:
    - 스케줄링 지연 최소화
    - 데이터 전송 최소화 (지역성 향상)
    - 리소스 활용률 최대화
    - I/O 대기 최소화
    """
    # 가중치
    W_LATENCY = 0.2
    W_TRANSFER = 0.3
    W_UTILIZATION = 0.25
    W_IO_WAIT = 0.25

    # 정규화된 점수 (0-1, 높을수록 좋음)
    latency_score = max(0, 1 - execution_result.get('scheduling_latency_ms', 500) / 1000)
    transfer_score = max(0, 1 - execution_result.get('data_transfer_bytes', 0) / (10 * 1024**3))
    utilization_score = execution_result.get('resource_utilization', 0.5)
    io_wait_score = max(0, 1 - execution_result.get('io_wait_ratio', 0.3))

    # 가중 합계
    reward = (
        W_LATENCY * latency_score +
        W_TRANSFER * transfer_score +
        W_UTILIZATION * utilization_score +
        W_IO_WAIT * io_wait_score
    )

    # 실패 시 페널티
    if not execution_result.get('successful', True):
        reward *= 0.1

    return reward
