"""
============================================
Node Resource Forecaster gRPC Server
노드 리소스 예측 gRPC 서버 (LSTM-Attention + Multi-LightGBM)
============================================

발표자료 기반 구현:
- LSTM-Attention: 15/30/60/120분 리소스 예측
- Multi-LightGBM: 7개 오케스트레이션 정책 결정
"""

import grpc
from concurrent import futures
import logging
import time
import numpy as np
from typing import Dict, Optional, List, Any
from collections import defaultdict
import threading
from datetime import datetime, timedelta
import sys
import os
import json

# Add proto directory to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'proto'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

# Import generated proto modules
try:
    import forecaster_pb2
    import forecaster_pb2_grpc
    PROTO_AVAILABLE = True
except ImportError:
    PROTO_AVAILABLE = False
    logging.warning("Proto files not generated yet. Run protoc to generate.")

# Import models - LSTM-Attention (primary) with LSTM fallback
try:
    from models.lstm_attention import (
        LSTMAttentionForecaster, LSTMAttentionConfig, LSTMAttentionTrainer,
        create_lstm_attention_forecaster
    )
    HAS_LSTM_ATTENTION = True
except ImportError:
    HAS_LSTM_ATTENTION = False
    logging.warning("LSTM-Attention not available, using basic LSTM")

from models.lstm_forecaster import (
    LSTMForecaster, ForecastConfig, ForecastTrainer,
    PeakIdlePredictor, create_forecaster
)

# Import Multi-LightGBM Policy Engine
try:
    from models.multi_lightgbm import (
        MultiLightGBMPolicyEngine, PolicyDecision, OrchestrationDecisions,
        create_policy_engine
    )
    HAS_LIGHTGBM_ENGINE = True
except ImportError:
    HAS_LIGHTGBM_ENGINE = False
    logging.warning("Multi-LightGBM engine not available")

# Kubernetes client for real cluster state
try:
    from kubernetes import client, config as k8s_config
    HAS_K8S_CLIENT = True
except ImportError:
    HAS_K8S_CLIENT = False
    logging.warning("Kubernetes client not available")

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


class ClusterStateManager:
    """실시간 클러스터 상태 관리"""

    def __init__(self, refresh_interval: int = 30):
        self.refresh_interval = refresh_interval
        self._state_cache: Dict[str, Dict] = {}
        self._last_refresh = None
        self._lock = threading.Lock()

        # DCGM Exporter 엔드포인트 (노드별 GPU 메트릭 수집용)
        self.dcgm_endpoint = os.environ.get(
            'DCGM_EXPORTER_ENDPOINT',
            'http://dcgm-exporter.gpu-monitoring.svc.cluster.local:9400/metrics'
        )
        self._dcgm_cache: Dict[str, float] = {}   # node_name → avg gpu_util [0,1]
        self._dcgm_last_refresh = None
        self._dcgm_refresh_interval = 30  # seconds

        # Kubernetes 클라이언트 초기화
        self.core_api = None
        self.custom_api = None
        if HAS_K8S_CLIENT:
            try:
                k8s_config.load_incluster_config()
                self.core_api = client.CoreV1Api()
                self.custom_api = client.CustomObjectsApi()
                logger.info("Kubernetes client initialized (in-cluster)")
            except Exception:
                try:
                    k8s_config.load_kube_config()
                    self.core_api = client.CoreV1Api()
                    self.custom_api = client.CustomObjectsApi()
                    logger.info("Kubernetes client initialized (kubeconfig)")
                except Exception as e:
                    logger.warning(f"Failed to initialize K8s client: {e}")

    def get_node_state(self, node_name: str) -> Dict[str, Any]:
        """노드 상태 조회"""
        self._maybe_refresh()
        with self._lock:
            return self._state_cache.get(node_name, self._default_state())

    def get_all_nodes_state(self) -> Dict[str, Dict]:
        """전체 노드 상태 조회"""
        self._maybe_refresh()
        with self._lock:
            return dict(self._state_cache)

    def get_cluster_average(self) -> Dict[str, float]:
        """클러스터 평균 상태"""
        states = self.get_all_nodes_state()
        if not states:
            return self._default_state()

        avg = defaultdict(float)
        for state in states.values():
            for key, value in state.items():
                if isinstance(value, (int, float)):
                    avg[key] += value

        for key in avg:
            avg[key] /= len(states)

        return dict(avg)

    def _maybe_refresh(self):
        """필요시 상태 갱신"""
        now = datetime.now()
        if self._last_refresh is None or (now - self._last_refresh).seconds > self.refresh_interval:
            self._refresh_cluster_state()
            self._last_refresh = now

    def _refresh_cluster_state(self):
        """클러스터 상태 갱신"""
        if not self.core_api:
            return

        try:
            nodes = self.core_api.list_node()
            pods = self.core_api.list_pod_for_all_namespaces(field_selector='status.phase=Running')

            # 노드별 Pod 리소스 사용량 집계
            node_pod_resources = defaultdict(lambda: {'cpu_requests': 0, 'mem_requests': 0})
            for pod in pods.items:
                node_name = pod.spec.node_name
                if not node_name:
                    continue

                for container in pod.spec.containers:
                    if container.resources and container.resources.requests:
                        cpu = container.resources.requests.get('cpu', '0')
                        mem = container.resources.requests.get('memory', '0')
                        node_pod_resources[node_name]['cpu_requests'] += self._parse_cpu(cpu)
                        node_pod_resources[node_name]['mem_requests'] += self._parse_memory(mem)

            with self._lock:
                self._state_cache.clear()
                for node in nodes.items:
                    node_name = node.metadata.name

                    # 노드 용량
                    capacity = node.status.capacity
                    allocatable = node.status.allocatable

                    cpu_capacity = self._parse_cpu(capacity.get('cpu', '1'))
                    mem_capacity = self._parse_memory(capacity.get('memory', '1Gi'))
                    gpu_capacity = int(capacity.get('nvidia.com/gpu', 0))

                    # 사용량 계산
                    used = node_pod_resources.get(node_name, {'cpu_requests': 0, 'mem_requests': 0})
                    cpu_util = used['cpu_requests'] / max(cpu_capacity, 1)
                    mem_util = used['mem_requests'] / max(mem_capacity, 1)

                    # 노드 타입
                    labels = node.metadata.labels or {}
                    node_type = 'compute'
                    if gpu_capacity > 0 or 'gpu' in labels.get('node-role.kubernetes.io/worker', ''):
                        node_type = 'gpu'
                    elif 'storage' in labels.get('layer', ''):
                        node_type = 'storage'

                    # GPU utilization: DCGM scraping
                    gpu_util = self._get_dcgm_gpu_util(node_name) if gpu_capacity > 0 else 0.0

                    self._state_cache[node_name] = {
                        'cpu_util': min(cpu_util, 1.0),
                        'memory_util': min(mem_util, 1.0),
                        'gpu_util': gpu_util,
                        'storage_io_util': 0.0,  # scope collector이 담당 (ClusterInsight 경유)
                        'pending_pods': 0,
                        'node_type': node_type,
                        'cpu_capacity': cpu_capacity,
                        'memory_capacity': mem_capacity,
                        'gpu_capacity': gpu_capacity,
                    }

            logger.debug(f"Refreshed cluster state: {len(self._state_cache)} nodes")

        except Exception as e:
            logger.error(f"Failed to refresh cluster state: {e}")

    def _get_dcgm_gpu_util(self, node_name: str) -> float:
        """DCGM Exporter에서 특정 노드의 평균 GPU 사용률을 반환합니다 [0.0, 1.0].

        DCGM_FI_DEV_GPU_UTIL 메트릭을 파싱하며 node 레이블로 필터링합니다.
        DCGM에 접근할 수 없는 경우 0.0을 반환합니다 (non-GPU 노드 포함).
        """
        self._maybe_refresh_dcgm()
        return self._dcgm_cache.get(node_name, 0.0)

    def _maybe_refresh_dcgm(self):
        """DCGM 캐시가 만료됐으면 새로 수집합니다."""
        now = datetime.now()
        if (self._dcgm_last_refresh is not None and
                (now - self._dcgm_last_refresh).total_seconds() < self._dcgm_refresh_interval):
            return
        self._refresh_dcgm_cache()
        self._dcgm_last_refresh = now

    def _refresh_dcgm_cache(self):
        """DCGM Exporter HTTP endpoint를 스크래핑해 노드별 GPU 사용률을 캐시합니다."""
        try:
            import urllib.request
            import urllib.error

            req = urllib.request.urlopen(self.dcgm_endpoint, timeout=5)
            body = req.read().decode('utf-8')
        except Exception as e:
            logger.debug(f"[ClusterState] DCGM not reachable ({self.dcgm_endpoint}): {e}")
            return

        # node → [util values] 집계
        node_utils: Dict[str, list] = {}

        for line in body.splitlines():
            if line.startswith('#') or not line.strip():
                continue
            if not line.startswith('DCGM_FI_DEV_GPU_UTIL'):
                continue

            # 레이블 파싱: DCGM_FI_DEV_GPU_UTIL{...,node="worker-1",...} 72
            node_name = None
            lbrace = line.find('{')
            rbrace = line.find('}')
            if lbrace != -1 and rbrace != -1:
                labels_str = line[lbrace + 1:rbrace]
                for kv in labels_str.split(','):
                    kv = kv.strip()
                    if kv.startswith('node='):
                        node_name = kv.split('=', 1)[1].strip('"')
                        break

            # 값 파싱 (중괄호 뒤 마지막 필드)
            parts = line.split()
            if not parts:
                continue
            try:
                val = float(parts[-1])
            except ValueError:
                continue

            if node_name:
                node_utils.setdefault(node_name, []).append(val)

        # 평균 계산 후 캐시 업데이트 (0–100 → 0.0–1.0)
        with self._lock:
            for node, vals in node_utils.items():
                self._dcgm_cache[node] = sum(vals) / len(vals) / 100.0

        logger.debug(f"[ClusterState] DCGM cache refreshed: {list(self._dcgm_cache.keys())}")

    def _parse_cpu(self, cpu_str: str) -> float:
        """CPU 문자열 파싱 (cores)"""
        if isinstance(cpu_str, (int, float)):
            return float(cpu_str)
        cpu_str = str(cpu_str)
        if cpu_str.endswith('m'):
            return float(cpu_str[:-1]) / 1000
        return float(cpu_str)

    def _parse_memory(self, mem_str: str) -> float:
        """메모리 문자열 파싱 (bytes)"""
        if isinstance(mem_str, (int, float)):
            return float(mem_str)
        mem_str = str(mem_str)
        units = {'Ki': 1024, 'Mi': 1024**2, 'Gi': 1024**3, 'Ti': 1024**4,
                 'K': 1000, 'M': 1000**2, 'G': 1000**3, 'T': 1000**4}
        for suffix, multiplier in units.items():
            if mem_str.endswith(suffix):
                return float(mem_str[:-len(suffix)]) * multiplier
        return float(mem_str)

    def _default_state(self) -> Dict[str, Any]:
        """기본 상태"""
        return {
            'cpu_util': 0.5,
            'memory_util': 0.5,
            'gpu_util': 0.0,
            'storage_io_util': 0.0,
            'pending_pods': 0,
            'node_type': 'compute',
        }


class NodeResourceForecasterServicer(forecaster_pb2_grpc.NodeResourceForecasterServiceServicer if PROTO_AVAILABLE else object):
    """
    Node Resource Forecaster gRPC Servicer

    발표자료 기반:
    - LSTM-Attention 모델로 리소스 예측
    - Multi-LightGBM으로 오케스트레이션 정책 결정
    """

    def __init__(self, model_path: Optional[str] = None, policy_model_dir: Optional[str] = None):
        # ============ 모델 초기화 ============
        # Primary: LSTM-Attention (발표자료 구현)
        if HAS_LSTM_ATTENTION:
            self.attention_config = LSTMAttentionConfig()
            self.attention_model, self.attention_trainer = create_lstm_attention_forecaster(self.attention_config)
            self.use_attention = True
            logger.info("Using LSTM-Attention model (발표자료 구현)")
        else:
            self.use_attention = False

        # Fallback: Basic LSTM
        self.basic_config = ForecastConfig()
        self.basic_model, self.basic_trainer = create_forecaster(self.basic_config)

        # Peak/Idle 예측기
        self.peak_idle_predictor = PeakIdlePredictor(self.basic_model)

        # ============ Multi-LightGBM 정책 엔진 ============
        if HAS_LIGHTGBM_ENGINE:
            self.policy_engine = create_policy_engine(policy_model_dir)
            logger.info("Multi-LightGBM policy engine initialized (7 models)")
        else:
            self.policy_engine = None
            logger.warning("Policy engine not available")

        # ============ 클러스터 상태 관리자 ============
        self.cluster_state = ClusterStateManager(refresh_interval=30)

        # 모델 로드
        if model_path and os.path.exists(model_path):
            try:
                if self.use_attention:
                    self.attention_trainer.load(model_path)
                else:
                    self.basic_trainer.load(model_path)
                logger.info(f"Model loaded from {model_path}")
            except Exception as e:
                logger.warning(f"Failed to load model: {e}, using random initialization")

        # 노드별 히스토리 데이터 저장
        self.node_history: Dict[str, List[Dict]] = defaultdict(list)
        self.history_lock = threading.Lock()

        # 설정
        self.max_history_size = 1440  # 24시간 (분 단위)
        self.min_history_for_prediction = 60  # 최소 60분 데이터 필요

        # 통계
        self.prediction_count = 0
        self.policy_decision_count = 0
        self.last_training_time = None
        self.model_ready = False

        # Training synchronization
        self.training_in_progress = False
        self.training_lock = threading.Lock()
        self.model_lock = threading.RLock()

        logger.info("Node Resource Forecaster initialized (LSTM-Attention + Multi-LightGBM)")

        # Pull history from Insight Hub when INSIGHT_HUB_ADDR is set (insight-scope → hub → forecaster)
        try:
            from hub_sync import start_hub_sync_if_configured
            self._hub_sync = start_hub_sync_if_configured(
                self.node_history,
                self.history_lock,
                self.max_history_size,
            )
        except Exception as e:
            logger.warning("Insight Hub sync not started: %s", e)
            self._hub_sync = None

    def ForecastNodeResources(self, request, context):
        """노드 리소스 예측"""
        node_name = request.node_name
        horizons = list(request.forecast_horizons_minutes) or [15, 30, 60, 120]

        logger.info(f"[Forecaster] ForecastNodeResources: node={node_name}, horizons={horizons}")

        try:
            # 노드 히스토리 데이터 조회
            with self.history_lock:
                history = self.node_history.get(node_name, [])

            if len(history) < self.min_history_for_prediction:
                logger.warning(f"[Forecaster] Insufficient history for {node_name}: "
                             f"{len(history)}/{self.min_history_for_prediction}")

                # 실제 클러스터 상태 기반 시뮬레이션
                current_state = self.cluster_state.get_node_state(node_name)
                return self._create_simulated_forecast_response(node_name, horizons, current_state)

            # 히스토리 데이터를 numpy 배열로 변환
            history_array = self._history_to_array(history[-60:])  # 최근 60분

            # 예측 수행
            with self.model_lock:
                if self.use_attention:
                    predictions = self.attention_model.predict(sequence=history_array)
                else:
                    predictions = self.basic_model.predict(
                        sequence=history_array,
                        horizons=horizons
                    )

            # 응답 생성
            response = self._create_forecast_response(node_name, predictions)
            self.prediction_count += 1

            logger.info(f"[Forecaster] Forecast completed for {node_name}")
            return response

        except Exception as e:
            logger.error(f"[Forecaster] Error forecasting for {node_name}: {e}")
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return forecaster_pb2.ForecastNodeResponse()

    def ForecastClusterResources(self, request, context):
        """클러스터 전체 리소스 예측"""
        horizons = list(request.forecast_horizons_minutes) or [15, 30, 60, 120]
        include_breakdown = request.include_node_breakdown

        logger.info(f"[Forecaster] ForecastClusterResources: horizons={horizons}")

        try:
            with self.history_lock:
                all_nodes = list(self.node_history.keys())

            if not all_nodes:
                # 실제 클러스터 상태 기반 시뮬레이션
                return self._create_simulated_cluster_response(horizons)

            # 각 노드별 예측
            node_forecasts = {}
            cluster_predictions = defaultdict(list)

            for node_name in all_nodes:
                history = self.node_history.get(node_name, [])
                if len(history) < self.min_history_for_prediction:
                    continue

                history_array = self._history_to_array(history[-60:])
                with self.model_lock:
                    if self.use_attention:
                        predictions = self.attention_model.predict(sequence=history_array)
                    else:
                        predictions = self.basic_model.predict(sequence=history_array, horizons=horizons)

                if include_breakdown:
                    node_forecasts[node_name] = self._create_node_forecast_result(predictions)

                # 클러스터 평균 계산용
                for h, pred in predictions.items():
                    cluster_predictions[h].append(pred)

            # 클러스터 평균 예측
            cluster_forecasts = []
            for h in horizons:
                preds = cluster_predictions.get(h, [])
                if preds:
                    forecast = forecaster_pb2.ResourceForecast(
                        horizon_minutes=h,
                        predicted_cpu_utilization=np.mean([p['predicted_cpu'] for p in preds]),
                        predicted_memory_utilization=np.mean([p['predicted_memory'] for p in preds]),
                        predicted_gpu_utilization=np.mean([p.get('predicted_gpu', 0) for p in preds]),
                        predicted_storage_io_utilization=np.mean([p.get('predicted_storage_io', 0.3) for p in preds]),
                        confidence_interval_lower=0.3,
                        confidence_interval_upper=0.7
                    )
                    cluster_forecasts.append(forecast)

            return forecaster_pb2.ForecastClusterResponse(
                cluster_forecasts=cluster_forecasts,
                node_forecasts=node_forecasts
            )

        except Exception as e:
            logger.error(f"[Forecaster] Error forecasting cluster: {e}")
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return forecaster_pb2.ForecastClusterResponse()

    def GetOrchestrationPolicy(self, request, context):
        """
        오케스트레이션 정책 결정 (Multi-LightGBM)

        7가지 정책 결정:
        1. node_health: NORMAL / STRESSED / CRITICAL
        2. autoscale: YES / NO
        3. migration: YES / NO
        4. caching: YES / NO
        5. load_balancing: YES / NO
        6. provisioning: YES / NO
        7. storage_tiering: YES / NO
        """
        node_name = request.node_name

        logger.info(f"[PolicyEngine] GetOrchestrationPolicy: node={node_name}")

        try:
            if not self.policy_engine:
                # Policy engine 없을 때 기본 응답
                return self._create_default_policy_response(node_name)

            # 현재 노드 상태 조회
            current_state = self.cluster_state.get_node_state(node_name)
            cluster_avg = self.cluster_state.get_cluster_average()
            current_state['cluster_avg_cpu'] = cluster_avg.get('cpu_util', 0.5)

            # LSTM 예측 결과 가져오기
            with self.history_lock:
                history = self.node_history.get(node_name, [])

            if len(history) >= self.min_history_for_prediction:
                history_array = self._history_to_array(history[-60:])
                with self.model_lock:
                    if self.use_attention:
                        predictions = self.attention_model.predict(sequence=history_array)
                    else:
                        predictions = self.basic_model.predict(sequence=history_array)
            else:
                # 히스토리 부족 시 현재 상태 기반 예측 시뮬레이션
                predictions = self._simulate_predictions_from_current(current_state)

            # 노드 추가 정보
            node_info = {
                'node_name': node_name,
                'node_type': current_state.get('node_type', 'compute'),
                'queue_status': {'pending': 0, 'admitted': 0}
            }

            # Multi-LightGBM 정책 결정
            decisions = self.policy_engine.predict(current_state, predictions, node_info)
            self.policy_decision_count += 1

            # Proto 응답 생성
            return self._create_policy_response(decisions)

        except Exception as e:
            logger.error(f"[PolicyEngine] Error getting policy for {node_name}: {e}")
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(str(e))
            return self._create_default_policy_response(node_name)

    def GetPeakIdlePrediction(self, request, context):
        """피크/유휴 시간대 예측"""
        lookahead_hours = request.lookahead_hours or 1
        node_name = request.node_name

        logger.info(f"[Forecaster] GetPeakIdlePrediction: node={node_name or 'cluster'}, "
                   f"lookahead={lookahead_hours}h")

        try:
            with self.history_lock:
                if node_name:
                    history = self.node_history.get(node_name, [])
                else:
                    all_histories = [h for h in self.node_history.values() if h]
                    if all_histories:
                        min_len = min(len(h) for h in all_histories)
                        history = []
                        for i in range(min_len):
                            avg_snapshot = {
                                'cpu': np.mean([h[i]['cpu'] for h in all_histories]),
                                'memory': np.mean([h[i]['memory'] for h in all_histories]),
                                'gpu': np.mean([h[i].get('gpu', 0) for h in all_histories]),
                                'storage_io': np.mean([h[i].get('storage_io', 0) for h in all_histories])
                            }
                            history.append(avg_snapshot)
                    else:
                        history = []

            if len(history) < self.min_history_for_prediction:
                return self._create_simulated_peak_idle_response()

            history_array = self._history_to_array(history[-60:])

            with self.model_lock:
                result = self.peak_idle_predictor.predict_periods(
                    historical_data=history_array,
                    lookahead_steps=lookahead_hours * 60
                )

            return self._convert_peak_idle_to_proto(result)

        except Exception as e:
            logger.error(f"[Forecaster] Error predicting peak/idle: {e}")
            return self._create_simulated_peak_idle_response()

    def SubmitHistoryData(self, request, context):
        """히스토리 데이터 제출 (학습용)"""
        node_name = request.node_name
        snapshots = request.snapshots

        logger.info(f"[Forecaster] SubmitHistoryData: node={node_name}, "
                   f"snapshots={len(snapshots)}")

        # When using Insight Hub, insight-scope writes there; forecaster fills history via pull sync.
        if os.environ.get("INSIGHT_HUB_ADDR", "").strip():
            logger.info("[Forecaster] SubmitHistoryData accepted but not stored (using Insight Hub pull sync)")
            return forecaster_pb2.SubmitHistoryResponse(
                accepted=True,
                snapshots_processed=len(snapshots),
            )

        try:
            with self.history_lock:
                for snapshot in snapshots:
                    self.node_history[node_name].append({
                        'timestamp': snapshot.timestamp.ToDatetime() if snapshot.timestamp else datetime.now(),
                        'cpu': snapshot.cpu_utilization,
                        'memory': snapshot.memory_utilization,
                        'gpu': snapshot.gpu_utilization,
                        'storage_io': snapshot.storage_io_utilization
                    })

                # 히스토리 크기 제한
                if len(self.node_history[node_name]) > self.max_history_size:
                    self.node_history[node_name] = \
                        self.node_history[node_name][-self.max_history_size:]

            # 주기적 학습 트리거
            self._maybe_trigger_training()

            return forecaster_pb2.SubmitHistoryResponse(
                accepted=True,
                snapshots_processed=len(snapshots)
            )

        except Exception as e:
            logger.error(f"[Forecaster] Error submitting history: {e}")
            return forecaster_pb2.SubmitHistoryResponse(
                accepted=False,
                snapshots_processed=0
            )

    def GetLatestClusterMetrics(self, request, context):
        """
        최신 클러스터 메트릭 스냅샷 반환 (Policy-Engine RL state용).
        역할 최소화: node_history 집계 + K8s pending 수 조회만 수행.
        """
        try:
            with self.history_lock:
                node_names = list(self.node_history.keys())

            if not node_names:
                return forecaster_pb2.ClusterMetricsSnapshot(
                    avg_cpu_utilization=0.5,
                    avg_memory_utilization=0.5,
                    avg_gpu_utilization=0.0,
                    avg_storage_io_utilization=0.0,
                    pending_pods_count=0,
                    node_imbalance=0.0,
                )

            cpu_utils = []
            mem_utils = []
            gpu_utils = []
            storage_io_utils = []
            node_metrics_map = {}

            with self.history_lock:
                for node_name in node_names:
                    history = self.node_history.get(node_name, [])
                    if not history:
                        continue
                    latest = history[-1]
                    cpu = latest.get('cpu', 0.5)
                    mem = latest.get('memory', 0.5)
                    gpu = latest.get('gpu', 0.0)
                    storage_io = latest.get('storage_io', 0.0)

                    cpu_utils.append(cpu)
                    mem_utils.append(mem)
                    gpu_utils.append(gpu)
                    storage_io_utils.append(storage_io)

                    node_metrics_map[node_name] = forecaster_pb2.NodeMetricsSnapshot(
                        cpu_utilization=cpu,
                        memory_utilization=mem,
                        gpu_utilization=gpu,
                        storage_io_utilization=storage_io,
                    )

            n = len(cpu_utils)
            avg_cpu = sum(cpu_utils) / n if n else 0.5
            avg_mem = sum(mem_utils) / n if n else 0.5
            avg_gpu = sum(gpu_utils) / n if n else 0.0
            avg_storage_io = sum(storage_io_utils) / n if n else 0.0

            # node_imbalance: cpu utilization variance across nodes
            node_imbalance = float(np.var(cpu_utils)) if n > 1 else 0.0

            # pending_pods: K8s API로 Pending Pod 수 조회
            pending_pods = 0
            if HAS_K8S_CLIENT and self.cluster_state.core_api:
                try:
                    pods = self.cluster_state.core_api.list_pod_for_all_namespaces(
                        field_selector='status.phase=Pending'
                    )
                    pending_pods = len(pods.items)
                except Exception as e:
                    logger.debug(f"[Forecaster] GetLatestClusterMetrics: pending pods fetch failed: {e}")

            return forecaster_pb2.ClusterMetricsSnapshot(
                avg_cpu_utilization=avg_cpu,
                avg_memory_utilization=avg_mem,
                avg_gpu_utilization=avg_gpu,
                avg_storage_io_utilization=avg_storage_io,
                pending_pods_count=pending_pods,
                node_imbalance=node_imbalance,
                node_metrics=node_metrics_map,
            )

        except Exception as e:
            logger.error(f"[Forecaster] GetLatestClusterMetrics error: {e}")
            return forecaster_pb2.ClusterMetricsSnapshot(
                avg_cpu_utilization=0.5,
                avg_memory_utilization=0.5,
                avg_gpu_utilization=0.0,
                avg_storage_io_utilization=0.0,
                pending_pods_count=0,
                node_imbalance=0.0,
            )

    def HealthCheck(self, request, context):
        """헬스 체크"""
        from google.protobuf.timestamp_pb2 import Timestamp

        response = forecaster_pb2.HealthCheckResponse(
            healthy=True,
            version="2.0.0-attention",  # LSTM-Attention 버전
            model_ready=1 if self.model_ready else 0,
            total_predictions=self.prediction_count
        )

        if self.last_training_time:
            ts = Timestamp()
            ts.FromDatetime(self.last_training_time)
            response.last_training_time.CopyFrom(ts)

        return response

    # ============ Helper Methods ============

    def _history_to_array(self, history: List[Dict]) -> np.ndarray:
        """히스토리 데이터를 numpy 배열로 변환"""
        # LSTM-Attention은 12개 특성, 기본 LSTM은 4개
        if self.use_attention:
            return np.array([
                [
                    h.get('cpu', 0.5),
                    h.get('memory', 0.5),
                    h.get('net_in', 0),
                    h.get('net_out', 0),
                    h.get('available_cores', 4),
                    h.get('load_average', 0.5),
                    h.get('workload_phase', 0),
                    h.get('storage_io', 0.3),
                    h.get('total_iops', 1000),
                    h.get('throughput', 100),
                    h.get('cache_hit_ratio', 0.7),
                    h.get('gpu', 0.0),
                ]
                for h in history
            ], dtype=np.float32)
        else:
            return np.array([
                [h['cpu'], h['memory'], h.get('gpu', 0), h.get('storage_io', 0)]
                for h in history
            ], dtype=np.float32)

    def _simulate_predictions_from_current(self, current_state: Dict) -> Dict[int, Dict]:
        """현재 상태 기반 예측 시뮬레이션"""
        cpu = current_state.get('cpu_util', 0.5)
        mem = current_state.get('memory_util', 0.5)
        gpu = current_state.get('gpu_util', 0.0)
        pending = current_state.get('pending_pods', 0)

        predictions = {}
        for horizon in [15, 30, 60, 120]:
            # 시간이 지날수록 약간의 변화 가정
            drift = np.random.randn() * 0.05 * (horizon / 30)
            predictions[horizon] = {
                'predicted_cpu': np.clip(cpu + drift, 0, 1),
                'predicted_memory': np.clip(mem + drift * 0.8, 0, 1),
                'predicted_gpu': np.clip(gpu + drift * 0.5, 0, 1),
                'predicted_pending_pods': max(0, pending + int(drift * 5)),
                'confidence': max(0.5, 0.9 - horizon * 0.003),  # 먼 미래일수록 confidence 낮음
            }
        return predictions

    def _create_forecast_response(self, node_name: str, predictions: Dict) -> 'forecaster_pb2.ForecastNodeResponse':
        """예측 응답 생성"""
        forecasts = []
        for horizon, pred in predictions.items():
            forecast = forecaster_pb2.ResourceForecast(
                horizon_minutes=horizon,
                predicted_cpu_utilization=pred['predicted_cpu'],
                predicted_memory_utilization=pred['predicted_memory'],
                predicted_gpu_utilization=pred.get('predicted_gpu', 0),
                predicted_storage_io_utilization=pred.get('predicted_storage_io', 0.3),
                confidence_interval_lower=pred.get('lower_bound', [0])[0] if pred.get('lower_bound') else 0,
                confidence_interval_upper=pred.get('upper_bound', [1])[0] if pred.get('upper_bound') else 1
            )
            forecasts.append(forecast)

        avg_confidence = np.mean([pred['confidence'] for pred in predictions.values()])

        return forecaster_pb2.ForecastNodeResponse(
            node_name=node_name,
            forecasts=forecasts,
            confidence=avg_confidence
        )

    def _create_node_forecast_result(self, predictions: Dict) -> 'forecaster_pb2.NodeForecastResult':
        """노드별 예측 결과 생성"""
        forecasts = []
        for horizon, pred in predictions.items():
            forecast = forecaster_pb2.ResourceForecast(
                horizon_minutes=horizon,
                predicted_cpu_utilization=pred['predicted_cpu'],
                predicted_memory_utilization=pred['predicted_memory'],
                predicted_gpu_utilization=pred.get('predicted_gpu', 0),
                predicted_storage_io_utilization=pred.get('predicted_storage_io', 0.3)
            )
            forecasts.append(forecast)

        avg_confidence = np.mean([pred['confidence'] for pred in predictions.values()])
        return forecaster_pb2.NodeForecastResult(forecasts=forecasts, confidence=avg_confidence)

    def _create_simulated_forecast_response(self, node_name: str, horizons: List[int], current_state: Dict = None) -> 'forecaster_pb2.ForecastNodeResponse':
        """실제 상태 기반 시뮬레이션 예측 생성"""
        current_state = current_state or {}
        base_cpu = current_state.get('cpu_util', 0.5)
        base_mem = current_state.get('memory_util', 0.5)
        base_gpu = current_state.get('gpu_util', 0.0)
        base_io = current_state.get('storage_io_util', 0.3)

        forecasts = []
        for h in horizons:
            drift = np.random.randn() * 0.05 * (h / 30)
            forecast = forecaster_pb2.ResourceForecast(
                horizon_minutes=h,
                predicted_cpu_utilization=np.clip(base_cpu + drift, 0, 1),
                predicted_memory_utilization=np.clip(base_mem + drift * 0.8, 0, 1),
                predicted_gpu_utilization=np.clip(base_gpu + drift * 0.5, 0, 1),
                predicted_storage_io_utilization=np.clip(base_io + drift * 0.3, 0, 1),
                confidence_interval_lower=max(0, base_cpu - 0.15),
                confidence_interval_upper=min(1, base_cpu + 0.15)
            )
            forecasts.append(forecast)

        return forecaster_pb2.ForecastNodeResponse(
            node_name=node_name,
            forecasts=forecasts,
            confidence=0.6
        )

    def _create_simulated_cluster_response(self, horizons: List[int]) -> 'forecaster_pb2.ForecastClusterResponse':
        """실제 클러스터 상태 기반 시뮬레이션"""
        avg_state = self.cluster_state.get_cluster_average()

        forecasts = []
        for h in horizons:
            drift = np.random.randn() * 0.03 * (h / 30)
            forecast = forecaster_pb2.ResourceForecast(
                horizon_minutes=h,
                predicted_cpu_utilization=np.clip(avg_state.get('cpu_util', 0.45) + drift, 0, 1),
                predicted_memory_utilization=np.clip(avg_state.get('memory_util', 0.50) + drift, 0, 1),
                predicted_gpu_utilization=np.clip(avg_state.get('gpu_util', 0.35) + drift, 0, 1),
                predicted_storage_io_utilization=np.clip(avg_state.get('storage_io_util', 0.30) + drift, 0, 1)
            )
            forecasts.append(forecast)

        return forecaster_pb2.ForecastClusterResponse(cluster_forecasts=forecasts)

    def _create_simulated_peak_idle_response(self) -> 'forecaster_pb2.PeakIdleResponse':
        """피크/유휴 예측"""
        avg_state = self.cluster_state.get_cluster_average()
        avg_util = (avg_state.get('cpu_util', 0.5) + avg_state.get('memory_util', 0.5)) / 2

        # 현재 utilization 기반 피크/유휴 예측
        if avg_util > 0.7:
            peak_periods = [forecaster_pb2.PeakIdlePeriod(horizon_minutes=15, avg_utilization=avg_util, confidence=0.7)]
            idle_periods = [forecaster_pb2.PeakIdlePeriod(horizon_minutes=60, avg_utilization=0.3, confidence=0.5)]
        else:
            peak_periods = [forecaster_pb2.PeakIdlePeriod(horizon_minutes=30, avg_utilization=0.75, confidence=0.5)]
            idle_periods = [forecaster_pb2.PeakIdlePeriod(horizon_minutes=15, avg_utilization=avg_util, confidence=0.7)]

        return forecaster_pb2.PeakIdleResponse(
            peak_periods=peak_periods,
            idle_periods=idle_periods,
            recommended_batch_window=forecaster_pb2.BatchWindowRecommendation(
                start_offset_minutes=55,
                end_offset_minutes=65,
                expected_utilization=0.25
            )
        )

    def _convert_peak_idle_to_proto(self, result: Dict) -> 'forecaster_pb2.PeakIdleResponse':
        """피크/유휴 결과를 proto로 변환"""
        peak_periods = [
            forecaster_pb2.PeakIdlePeriod(
                horizon_minutes=p.get('horizon_minutes', 0),
                avg_utilization=p.get('avg_utilization', 0),
                confidence=p.get('confidence', 0.5)
            ) for p in result.get('peak_periods', [])
        ]

        idle_periods = [
            forecaster_pb2.PeakIdlePeriod(
                horizon_minutes=p.get('horizon_minutes', 0),
                avg_utilization=p.get('avg_utilization', 0),
                confidence=p.get('confidence', 0.5)
            ) for p in result.get('idle_periods', [])
        ]

        batch_window = result.get('recommended_batch_window', {})

        return forecaster_pb2.PeakIdleResponse(
            peak_periods=peak_periods,
            idle_periods=idle_periods,
            recommended_batch_window=forecaster_pb2.BatchWindowRecommendation(
                start_offset_minutes=batch_window.get('start_offset_minutes', 0),
                end_offset_minutes=batch_window.get('end_offset_minutes', 0),
                expected_utilization=batch_window.get('expected_utilization', 0)
            )
        )

    def _create_policy_response(self, decisions: 'OrchestrationDecisions'):
        """정책 결정 Proto 응답 생성"""
        policy_decisions = []
        for d in decisions.decisions:
            policy_decisions.append(forecaster_pb2.PolicyDecision(
                task_name=d.task_name,
                decision=d.decision,
                probability=d.probability,
                urgency=d.urgency,
                confidence=d.confidence,
                reason=d.reason,
                parameters=json.dumps(d.parameters)
            ))

        return forecaster_pb2.OrchestrationPolicyResponse(
            node_name=decisions.node_name,
            timestamp=int(decisions.timestamp),
            decisions=policy_decisions,
            predicted_15min=json.dumps(decisions.predicted_15min),
            predicted_30min=json.dumps(decisions.predicted_30min),
            predicted_60min=json.dumps(decisions.predicted_60min),
        )

    def _create_default_policy_response(self, node_name: str):
        """기본 정책 응답"""
        current_state = self.cluster_state.get_node_state(node_name)

        default_decisions = [
            forecaster_pb2.PolicyDecision(
                task_name='node_health',
                decision='NORMAL' if current_state.get('cpu_util', 0.5) < 0.7 else 'STRESSED',
                probability=0.7,
                urgency='LOW',
                confidence=0.6,
                reason='Rule-based fallback (no model)',
                parameters='{}'
            ),
        ]

        return forecaster_pb2.OrchestrationPolicyResponse(
            node_name=node_name,
            timestamp=int(time.time()),
            decisions=default_decisions,
        )

    def _maybe_trigger_training(self):
        """필요시 모델 재학습 트리거"""
        with self.training_lock:
            if self.training_in_progress:
                return

        if self.last_training_time is None:
            should_train = True
        else:
            elapsed = datetime.now() - self.last_training_time
            should_train = elapsed > timedelta(hours=1)

        if not should_train:
            return

        total_samples = sum(len(h) for h in self.node_history.values())
        if total_samples < 1000:
            return

        def train_background():
            try:
                with self.training_lock:
                    if self.training_in_progress:
                        return
                    self.training_in_progress = True

                logger.info("[Forecaster] Starting background training...")

                with self.history_lock:
                    all_data = []
                    for history in self.node_history.values():
                        if len(history) >= 60:
                            all_data.extend(self._history_to_array(history))

                if len(all_data) < 500:
                    logger.info("[Forecaster] Not enough data for training")
                    return

                train_data = np.array(all_data, dtype=np.float32)

                with self.model_lock:
                    if self.use_attention:
                        # LSTM-Attention training
                        self.attention_trainer.model.train()
                        # TODO: Implement proper training loop
                    else:
                        self.basic_trainer.train(train_data, epochs=10)

                self.last_training_time = datetime.now()
                self.model_ready = True

                logger.info("[Forecaster] Background training completed")

            except Exception as e:
                logger.error(f"[Forecaster] Background training failed: {e}")
            finally:
                with self.training_lock:
                    self.training_in_progress = False

        threading.Thread(target=train_background, daemon=True).start()


def serve(port: int = 50055, http_port: int = 8080, model_path: Optional[str] = None, policy_model_dir: Optional[str] = None):
    """gRPC 서버 시작 (HTTP REST API 포함)"""
    if not PROTO_AVAILABLE:
        logger.error("Proto files not available. Cannot start gRPC server.")
        return

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))

    servicer = NodeResourceForecasterServicer(model_path, policy_model_dir)

    # Proto 서비스 등록
    forecaster_pb2_grpc.add_NodeResourceForecasterServiceServicer_to_server(
        servicer, server
    )

    server.add_insecure_port(f'[::]:{port}')
    server.start()

    logger.info(f"Node Resource Forecaster gRPC server started on port {port}")
    logger.info(f"  - LSTM-Attention: {'enabled' if servicer.use_attention else 'disabled (fallback to basic LSTM)'}")
    logger.info(f"  - Multi-LightGBM: {'enabled' if servicer.policy_engine else 'disabled'}")

    # HTTP REST API 서버 시작
    try:
        from server.http_server import set_servicer, start_http_server_thread
        set_servicer(servicer)
        start_http_server_thread(http_port)
        logger.info(f"Node Resource Forecaster HTTP server started on port {http_port}")
    except Exception as e:
        logger.warning(f"Failed to start HTTP server: {e}")

    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        logger.info("Shutting down server...")
        server.stop(grace=5)


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--port', type=int, default=50055)
    parser.add_argument('--http-port', type=int, default=8080)
    parser.add_argument('--model', type=str, default=None, help='Path to LSTM model checkpoint')
    parser.add_argument('--policy-models', type=str, default=None, help='Directory with LightGBM policy models')
    args = parser.parse_args()

    serve(port=args.port, http_port=args.http_port, model_path=args.model, policy_model_dir=args.policy_models)
