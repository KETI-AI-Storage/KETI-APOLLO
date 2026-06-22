"""
============================================
Multi-LightGBM Orchestration Policy Engine
============================================

발표자료 기반 구현:
- 7개 독립 LightGBM 모델
- LSTM 예측 결과를 입력받아 각 오케스트레이션 정책 결정
- 실제 클러스터 상태 기반 정책 판단

Tasks (7개 모델):
1. Node Health: NORMAL / STRESSED / CRITICAL
2. Autoscale: YES / NO
3. Migration: YES / NO (+ urgency)
4. Caching: YES / NO (+ priority)
5. Load Balancing: YES / NO
6. Provisioning: YES / NO
7. Storage Tiering: YES / NO (+ tier recommendation)

Input Features:
- LSTM 예측값 (cpu, memory, gpu, pending at 15/30/60/120min)
- 현재 클러스터 상태
- 워크로드 특성
"""

import numpy as np
from typing import Dict, List, Optional, Tuple, Any
from dataclasses import dataclass, field
import logging
import json
import os

# LightGBM import (graceful fallback)
try:
    import lightgbm as lgb
    HAS_LIGHTGBM = True
except ImportError:
    HAS_LIGHTGBM = False
    lgb = None

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


@dataclass
class PolicyDecision:
    """단일 정책 결정 결과"""
    task_name: str
    decision: str  # e.g., "YES", "NO", "STRESSED"
    probability: float  # 0.0 ~ 1.0
    urgency: str  # LOW, MEDIUM, HIGH, CRITICAL
    confidence: float
    reason: str
    parameters: Dict[str, Any] = field(default_factory=dict)


@dataclass
class OrchestrationDecisions:
    """전체 오케스트레이션 결정"""
    node_name: str
    timestamp: float
    decisions: List[PolicyDecision]

    # 예측 기반 정보
    predicted_15min: Dict[str, float] = field(default_factory=dict)
    predicted_30min: Dict[str, float] = field(default_factory=dict)
    predicted_60min: Dict[str, float] = field(default_factory=dict)

    def to_dict(self) -> Dict:
        return {
            'node_name': self.node_name,
            'timestamp': self.timestamp,
            'decisions': [
                {
                    'task': d.task_name,
                    'decision': d.decision,
                    'probability': d.probability,
                    'urgency': d.urgency,
                    'confidence': d.confidence,
                    'reason': d.reason,
                    'parameters': d.parameters
                }
                for d in self.decisions
            ],
            'predictions': {
                '15min': self.predicted_15min,
                '30min': self.predicted_30min,
                '60min': self.predicted_60min,
            }
        }


class MultiLightGBMPolicyEngine:
    """
    Multi-LightGBM 오케스트레이션 정책 엔진

    7개의 독립적인 LightGBM 모델로 각 정책 결정
    LightGBM이 없으면 규칙 기반 fallback 사용
    """

    # 임계값 설정
    THRESHOLDS = {
        'cpu_warning': 0.70,
        'cpu_critical': 0.85,
        'memory_warning': 0.75,
        'memory_critical': 0.90,
        'gpu_warning': 0.80,
        'gpu_critical': 0.95,
        'pending_warning': 10,
        'pending_critical': 50,
    }

    def __init__(self, model_dir: Optional[str] = None):
        """
        Args:
            model_dir: LightGBM 모델 파일 디렉토리 (없으면 규칙 기반)
        """
        self.model_dir = model_dir
        self.models: Dict[str, Any] = {}

        # 7개 Task 정의
        self.task_names = [
            'node_health',      # Task 1
            'autoscale',        # Task 2
            'migration',        # Task 3
            'caching',          # Task 4
            'load_balancing',   # Task 5
            'provisioning',     # Task 6
            'storage_tiering',  # Task 7
        ]

        # Feature 이름 정의 (모델 입력)
        self.feature_names = [
            # 현재 상태
            'current_cpu_util',
            'current_memory_util',
            'current_gpu_util',
            'current_pending_pods',
            # 15분 후 예측
            'pred_15_cpu',
            'pred_15_memory',
            'pred_15_gpu',
            'pred_15_pending',
            # 30분 후 예측
            'pred_30_cpu',
            'pred_30_memory',
            'pred_30_gpu',
            'pred_30_pending',
            # 60분 후 예측
            'pred_60_cpu',
            'pred_60_memory',
            'pred_60_gpu',
            'pred_60_pending',
            # 120분 후 예측
            'pred_120_cpu',
            'pred_120_memory',
            'pred_120_gpu',
            'pred_120_pending',
            # 추가 특성
            'cpu_trend',        # 증가율
            'memory_trend',
            'node_type',        # 0=compute, 1=gpu, 2=storage
            'queue_pending',
            'queue_admitted',
        ]

        self._load_models()

    def _load_models(self):
        """LightGBM 모델 로드 (있으면)"""
        if not HAS_LIGHTGBM:
            logger.warning("LightGBM not installed, using rule-based fallback")
            return

        if not self.model_dir or not os.path.exists(self.model_dir):
            logger.info("No model directory specified, using rule-based fallback")
            return

        for task in self.task_names:
            model_path = os.path.join(self.model_dir, f"{task}_model.txt")
            if os.path.exists(model_path):
                try:
                    self.models[task] = lgb.Booster(model_file=model_path)
                    logger.info(f"Loaded LightGBM model for {task}")
                except Exception as e:
                    logger.warning(f"Failed to load model for {task}: {e}")

    def predict(
        self,
        current_state: Dict[str, float],
        lstm_predictions: Dict[int, Dict[str, float]],
        node_info: Optional[Dict] = None
    ) -> OrchestrationDecisions:
        """
        전체 오케스트레이션 정책 결정

        Args:
            current_state: 현재 노드 상태
                - cpu_util, memory_util, gpu_util, pending_pods
            lstm_predictions: LSTM 예측 결과
                - {15: {cpu, memory, gpu, pending}, 30: {...}, ...}
            node_info: 노드 추가 정보
                - node_name, node_type, queue_status

        Returns:
            OrchestrationDecisions with 7 policy decisions
        """
        import time

        node_info = node_info or {}
        node_name = node_info.get('node_name', 'unknown')

        # Feature 벡터 구성
        features = self._build_features(current_state, lstm_predictions, node_info)

        # 7개 Task 결정
        decisions = []

        # Task 1: Node Health
        decisions.append(self._decide_node_health(features, current_state, lstm_predictions))

        # Task 2: Autoscale
        decisions.append(self._decide_autoscale(features, current_state, lstm_predictions))

        # Task 3: Migration
        decisions.append(self._decide_migration(features, current_state, lstm_predictions))

        # Task 4: Caching
        decisions.append(self._decide_caching(features, current_state, lstm_predictions))

        # Task 5: Load Balancing
        decisions.append(self._decide_load_balancing(features, current_state, lstm_predictions))

        # Task 6: Provisioning
        decisions.append(self._decide_provisioning(features, current_state, lstm_predictions))

        # Task 7: Storage Tiering
        decisions.append(self._decide_storage_tiering(features, current_state, lstm_predictions))

        return OrchestrationDecisions(
            node_name=node_name,
            timestamp=time.time(),
            decisions=decisions,
            predicted_15min=lstm_predictions.get(15, {}),
            predicted_30min=lstm_predictions.get(30, {}),
            predicted_60min=lstm_predictions.get(60, {}),
        )

    def _build_features(
        self,
        current_state: Dict[str, float],
        lstm_predictions: Dict[int, Dict[str, float]],
        node_info: Dict
    ) -> np.ndarray:
        """Feature 벡터 구성"""
        features = []

        # 현재 상태
        features.extend([
            current_state.get('cpu_util', 0.5),
            current_state.get('memory_util', 0.5),
            current_state.get('gpu_util', 0.0),
            current_state.get('pending_pods', 0),
        ])

        # 예측값 (15, 30, 60, 120분)
        for horizon in [15, 30, 60, 120]:
            pred = lstm_predictions.get(horizon, {})
            features.extend([
                pred.get('predicted_cpu', 0.5),
                pred.get('predicted_memory', 0.5),
                pred.get('predicted_gpu', 0.0),
                pred.get('predicted_pending_pods', 0),
            ])

        # 트렌드 계산 (현재 → 60분 예측)
        pred_60 = lstm_predictions.get(60, {})
        cpu_trend = pred_60.get('predicted_cpu', 0.5) - current_state.get('cpu_util', 0.5)
        memory_trend = pred_60.get('predicted_memory', 0.5) - current_state.get('memory_util', 0.5)
        features.extend([cpu_trend, memory_trend])

        # 노드 타입
        node_type_map = {'compute': 0, 'gpu': 1, 'storage': 2}
        node_type = node_type_map.get(node_info.get('node_type', 'compute'), 0)
        features.append(node_type)

        # 큐 상태
        queue = node_info.get('queue_status', {})
        features.extend([
            queue.get('pending', 0),
            queue.get('admitted', 0),
        ])

        return np.array(features, dtype=np.float32).reshape(1, -1)

    def _decide_node_health(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 1: Node Health 판단
        Output: NORMAL / STRESSED / CRITICAL
        """
        # LightGBM 모델 있으면 사용
        if 'node_health' in self.models:
            proba = self.models['node_health'].predict(features)[0]
            # proba: [normal_prob, stressed_prob, critical_prob]
            decision_idx = np.argmax(proba)
            decisions = ['NORMAL', 'STRESSED', 'CRITICAL']
            decision = decisions[decision_idx]
            probability = float(proba[decision_idx])
        else:
            # 규칙 기반 fallback
            cpu = current.get('cpu_util', 0.5)
            mem = current.get('memory_util', 0.5)
            pred_15_cpu = predictions.get(15, {}).get('predicted_cpu', cpu)
            pred_15_mem = predictions.get(15, {}).get('predicted_memory', mem)

            max_util = max(cpu, mem, pred_15_cpu, pred_15_mem)

            if max_util >= self.THRESHOLDS['cpu_critical']:
                decision = 'CRITICAL'
                probability = min(1.0, max_util + 0.1)
            elif max_util >= self.THRESHOLDS['cpu_warning']:
                decision = 'STRESSED'
                probability = max_util
            else:
                decision = 'NORMAL'
                probability = 1.0 - max_util

        urgency = {'CRITICAL': 'CRITICAL', 'STRESSED': 'HIGH', 'NORMAL': 'LOW'}[decision]

        return PolicyDecision(
            task_name='node_health',
            decision=decision,
            probability=probability,
            urgency=urgency,
            confidence=0.85 if 'node_health' in self.models else 0.70,
            reason=f"Node health: {decision} (CPU={current.get('cpu_util', 0):.1%}, Mem={current.get('memory_util', 0):.1%})",
            parameters={'predicted_15min': predictions.get(15, {})}
        )

    def _decide_autoscale(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 2: Autoscale 필요 여부
        Output: YES / NO
        """
        if 'autoscale' in self.models:
            proba = self.models['autoscale'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # 30분 후 예측 기반 판단
            pred_30 = predictions.get(30, {})
            pred_cpu = pred_30.get('predicted_cpu', 0.5)
            pred_mem = pred_30.get('predicted_memory', 0.5)
            pred_pending = pred_30.get('predicted_pending_pods', 0)

            # Pending pods 증가 또는 리소스 부족 예측 시 스케일링
            scale_score = 0.0
            if pred_cpu >= self.THRESHOLDS['cpu_warning']:
                scale_score += 0.3
            if pred_mem >= self.THRESHOLDS['memory_warning']:
                scale_score += 0.3
            if pred_pending >= self.THRESHOLDS['pending_warning']:
                scale_score += 0.4

            decision = 'YES' if scale_score >= 0.5 else 'NO'
            probability = scale_score if decision == 'YES' else 1 - scale_score

        urgency = 'HIGH' if decision == 'YES' and probability > 0.7 else 'MEDIUM'

        return PolicyDecision(
            task_name='autoscale',
            decision=decision,
            probability=probability,
            urgency=urgency,
            confidence=0.80 if 'autoscale' in self.models else 0.68,
            reason=f"Autoscale {decision}: 30min prediction indicates {'resource pressure' if decision == 'YES' else 'sufficient capacity'}",
            parameters={'scale_direction': 'up' if decision == 'YES' else 'none'}
        )

    def _decide_migration(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 3: Migration 필요 여부
        Output: YES / NO
        """
        if 'migration' in self.models:
            proba = self.models['migration'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # 60분 후 예측 기반 (마이그레이션은 시간이 걸림)
            pred_60 = predictions.get(60, {})
            current_util = max(current.get('cpu_util', 0), current.get('memory_util', 0))
            pred_util = max(
                pred_60.get('predicted_cpu', 0.5),
                pred_60.get('predicted_memory', 0.5)
            )

            # 현재 높고 예측도 높으면 마이그레이션 필요
            if current_util >= self.THRESHOLDS['cpu_critical'] or pred_util >= self.THRESHOLDS['cpu_critical']:
                decision = 'YES'
                probability = max(current_util, pred_util)
            elif current_util >= self.THRESHOLDS['cpu_warning'] and pred_util >= self.THRESHOLDS['cpu_warning']:
                decision = 'YES'
                probability = (current_util + pred_util) / 2
            else:
                decision = 'NO'
                probability = 1 - max(current_util, pred_util)

        urgency = 'CRITICAL' if probability > 0.85 else ('HIGH' if probability > 0.7 else 'MEDIUM')

        return PolicyDecision(
            task_name='migration',
            decision=decision,
            probability=probability,
            urgency=urgency,
            confidence=0.75 if 'migration' in self.models else 0.61,
            reason=f"Migration {decision}: {'persistent high utilization predicted' if decision == 'YES' else 'utilization within limits'}",
            parameters={'priority': urgency}
        )

    def _decide_caching(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 4: Caching 활성화 여부
        Output: YES / NO
        """
        if 'caching' in self.models:
            proba = self.models['caching'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # I/O 부하 예측 기반 (storage_io 또는 메모리 압박)
            pred_15 = predictions.get(15, {})
            io_util = current.get('storage_io_util', 0.3)
            memory_pressure = current.get('memory_util', 0.5) > 0.7

            # 캐싱은 I/O 부하 완화에 도움
            cache_score = io_util * 0.6 + (0.4 if memory_pressure else 0.0)
            decision = 'YES' if cache_score >= 0.4 else 'NO'
            probability = cache_score if decision == 'YES' else 1 - cache_score

        return PolicyDecision(
            task_name='caching',
            decision=decision,
            probability=probability,
            urgency='MEDIUM' if decision == 'YES' else 'LOW',
            confidence=0.78 if 'caching' in self.models else 0.65,
            reason=f"Caching {decision}: {'I/O optimization recommended' if decision == 'YES' else 'current I/O levels acceptable'}",
            parameters={'cache_tier': 'memory' if decision == 'YES' else 'none'}
        )

    def _decide_load_balancing(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 5: Load Balancing 필요 여부
        Output: YES / NO
        """
        if 'load_balancing' in self.models:
            proba = self.models['load_balancing'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # 불균형 감지: 현재 노드가 과부하인데 다른 노드에 여유가 있을 때
            cpu = current.get('cpu_util', 0.5)
            cluster_avg_cpu = current.get('cluster_avg_cpu', 0.5)

            # 현재 노드가 평균보다 20% 이상 높으면 로드밸런싱 필요
            imbalance = cpu - cluster_avg_cpu
            if imbalance > 0.2 and cpu > self.THRESHOLDS['cpu_warning']:
                decision = 'YES'
                probability = min(1.0, imbalance + 0.5)
            else:
                decision = 'NO'
                probability = 1 - abs(imbalance)

        return PolicyDecision(
            task_name='load_balancing',
            decision=decision,
            probability=probability,
            urgency='HIGH' if decision == 'YES' else 'LOW',
            confidence=0.72 if 'load_balancing' in self.models else 0.60,
            reason=f"Load Balancing {decision}: {'workload imbalance detected' if decision == 'YES' else 'load distribution balanced'}",
            parameters={}
        )

    def _decide_provisioning(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 6: Pre-provisioning 필요 여부
        Output: YES / NO
        """
        if 'provisioning' in self.models:
            proba = self.models['provisioning'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # 120분 후 예측 기반 (사전 프로비저닝은 미리 준비)
            pred_120 = predictions.get(120, {})
            pred_cpu = pred_120.get('predicted_cpu', 0.5)
            pred_mem = pred_120.get('predicted_memory', 0.5)
            pred_pending = pred_120.get('predicted_pending_pods', 0)

            # 장기 예측에서 리소스 부족이 예상되면 사전 프로비저닝
            provision_score = 0.0
            if pred_cpu >= self.THRESHOLDS['cpu_warning']:
                provision_score += 0.35
            if pred_mem >= self.THRESHOLDS['memory_warning']:
                provision_score += 0.35
            if pred_pending >= self.THRESHOLDS['pending_warning']:
                provision_score += 0.30

            decision = 'YES' if provision_score >= 0.5 else 'NO'
            probability = provision_score if decision == 'YES' else 1 - provision_score

        return PolicyDecision(
            task_name='provisioning',
            decision=decision,
            probability=probability,
            urgency='MEDIUM',  # 프로비저닝은 사전 작업이므로 중간
            confidence=0.70 if 'provisioning' in self.models else 0.55,
            reason=f"Pre-provisioning {decision}: 120min forecast indicates {'capacity expansion needed' if decision == 'YES' else 'sufficient capacity'}",
            parameters={'horizon_minutes': 120}
        )

    def _decide_storage_tiering(
        self,
        features: np.ndarray,
        current: Dict,
        predictions: Dict
    ) -> PolicyDecision:
        """
        Task 7: Storage Tiering 조정 여부
        Output: YES / NO (+ tier recommendation)
        """
        if 'storage_tiering' in self.models:
            proba = self.models['storage_tiering'].predict(features)[0]
            decision = 'YES' if proba > 0.5 else 'NO'
            probability = float(proba) if decision == 'YES' else float(1 - proba)
        else:
            # Storage I/O 패턴 기반
            io_util = current.get('storage_io_util', 0.3)
            cache_hit = current.get('cache_hit_ratio', 0.7)

            # Cache hit ratio 낮거나 I/O 부하 높으면 티어링 조정 필요
            if io_util > 0.7 or cache_hit < 0.5:
                decision = 'YES'
                probability = max(io_util, 1 - cache_hit)
            else:
                decision = 'NO'
                probability = cache_hit

        # 티어 추천
        tier_recommendation = 'nvme' if probability > 0.8 else ('ssd' if probability > 0.5 else 'hdd')

        return PolicyDecision(
            task_name='storage_tiering',
            decision=decision,
            probability=probability,
            urgency='MEDIUM' if decision == 'YES' else 'LOW',
            confidence=0.71 if 'storage_tiering' in self.models else 0.58,
            reason=f"Storage Tiering {decision}: {'tier adjustment recommended' if decision == 'YES' else 'current tier optimal'}",
            parameters={'recommended_tier': tier_recommendation}
        )

    def train_models(self, training_data: Dict[str, np.ndarray], labels: Dict[str, np.ndarray]):
        """
        LightGBM 모델 학습

        Args:
            training_data: {task_name: feature_array}
            labels: {task_name: label_array}
        """
        if not HAS_LIGHTGBM:
            logger.error("LightGBM not installed, cannot train models")
            return

        for task in self.task_names:
            if task not in training_data or task not in labels:
                continue

            X = training_data[task]
            y = labels[task]

            # LightGBM 파라미터
            if task == 'node_health':
                # 3-class classification
                params = {
                    'objective': 'multiclass',
                    'num_class': 3,
                    'metric': 'multi_logloss',
                    'boosting_type': 'gbdt',
                    'num_leaves': 31,
                    'learning_rate': 0.05,
                    'feature_fraction': 0.9,
                }
            else:
                # Binary classification
                params = {
                    'objective': 'binary',
                    'metric': 'binary_logloss',
                    'boosting_type': 'gbdt',
                    'num_leaves': 31,
                    'learning_rate': 0.05,
                    'feature_fraction': 0.9,
                }

            train_data = lgb.Dataset(X, label=y, feature_name=self.feature_names[:X.shape[1]])
            model = lgb.train(params, train_data, num_boost_round=100)
            self.models[task] = model

            logger.info(f"Trained LightGBM model for {task}")

    def save_models(self, output_dir: str):
        """모델 저장"""
        os.makedirs(output_dir, exist_ok=True)

        for task, model in self.models.items():
            if model is not None:
                path = os.path.join(output_dir, f"{task}_model.txt")
                model.save_model(path)
                logger.info(f"Saved {task} model to {path}")


# Factory function
def create_policy_engine(model_dir: Optional[str] = None) -> MultiLightGBMPolicyEngine:
    """Multi-LightGBM 정책 엔진 생성"""
    return MultiLightGBMPolicyEngine(model_dir)
