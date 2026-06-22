"""
============================================
Node Resource Forecaster HTTP REST API Server
Policy Generator가 사용하는 HTTP REST 엔드포인트 제공

발표자료 기반:
- LSTM-Attention: 리소스 예측
- Multi-LightGBM: 7개 오케스트레이션 정책 결정
============================================
"""

import logging
import os
from flask import Flask, jsonify, request
from typing import Optional, List, Dict, Any
import threading
import numpy as np
import json

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Reference to the gRPC servicer (will be set from main)
servicer = None


def set_servicer(grpc_servicer):
    """Set the gRPC servicer reference"""
    global servicer
    servicer = grpc_servicer
    logger.info("[HTTP] Servicer reference set")


@app.route('/health', methods=['GET'])
def health_check():
    """Health check endpoint"""
    if servicer is None:
        return jsonify({"healthy": False, "error": "Servicer not initialized"}), 503

    return jsonify({
        "healthy": True,
        "version": "2.0.0-attention",  # LSTM-Attention + Multi-LightGBM
        "model_ready": servicer.model_ready,
        "total_predictions": servicer.prediction_count,
        "policy_decisions": getattr(servicer, 'policy_decision_count', 0),
        "lstm_attention_enabled": getattr(servicer, 'use_attention', False),
        "policy_engine_enabled": servicer.policy_engine is not None if hasattr(servicer, 'policy_engine') else False
    })


@app.route('/api/v1/forecast/node/<node_name>', methods=['GET'])
def forecast_node(node_name: str):
    """노드 리소스 예측 API (LSTM-Attention)"""
    if servicer is None:
        return jsonify({"error": "Servicer not initialized"}), 503

    logger.info(f"[HTTP] ForecastNode request: node={node_name}")

    try:
        # Get horizons from query params or use defaults (발표자료: 15, 30, 60, 120분)
        horizons_param = request.args.get('horizons', '15,30,60,120')
        horizons = [int(h) for h in horizons_param.split(',')]

        # Get node history
        with servicer.history_lock:
            history = servicer.node_history.get(node_name, [])

        if len(history) < servicer.min_history_for_prediction:
            # Use real cluster state for simulated forecast
            current_state = servicer.cluster_state.get_node_state(node_name)
            logger.warning(f"[HTTP] Insufficient history for {node_name}: {len(history)}/{servicer.min_history_for_prediction}")

            forecasts = []
            for h in horizons:
                # Base on real cluster state instead of hardcoded values
                base_cpu = current_state.get('cpu_util', 0.5)
                base_mem = current_state.get('memory_util', 0.5)
                base_gpu = current_state.get('gpu_util', 0.0)
                base_io = current_state.get('storage_io_util', 0.3)

                drift = np.random.randn() * 0.05 * (h / 30)
                forecasts.append({
                    "horizon_minutes": h,
                    "predicted_cpu_utilization": float(np.clip(base_cpu + drift, 0, 1)),
                    "predicted_memory_utilization": float(np.clip(base_mem + drift * 0.8, 0, 1)),
                    "predicted_gpu_utilization": float(np.clip(base_gpu + drift * 0.5, 0, 1)),
                    "predicted_storage_io_utilization": float(np.clip(base_io + drift * 0.3, 0, 1)),
                    "confidence_interval_lower": max(0, base_cpu - 0.15),
                    "confidence_interval_upper": min(1, base_cpu + 0.15)
                })
            return jsonify({
                "node_name": node_name,
                "forecasts": forecasts,
                "confidence": 0.6
            })

        # Get prediction from model (LSTM-Attention or basic LSTM)
        history_array = servicer._history_to_array(history[-60:])

        with servicer.model_lock:
            if getattr(servicer, 'use_attention', False):
                predictions = servicer.attention_model.predict(sequence=history_array)
            else:
                predictions = servicer.basic_model.predict(sequence=history_array, horizons=horizons)

        servicer.prediction_count += 1

        # Format response
        forecasts = []
        for h, pred in predictions.items():
            forecasts.append({
                "horizon_minutes": h,
                "predicted_cpu_utilization": pred['predicted_cpu'],
                "predicted_memory_utilization": pred['predicted_memory'],
                "predicted_gpu_utilization": pred.get('predicted_gpu', 0),
                "predicted_storage_io_utilization": pred.get('predicted_storage_io', 0.3),
                "confidence_interval_lower": pred.get('lower_bound', [0])[0] if pred.get('lower_bound') else 0,
                "confidence_interval_upper": pred.get('upper_bound', [1])[0] if pred.get('upper_bound') else 1
            })

        avg_confidence = np.mean([pred['confidence'] for pred in predictions.values()])

        logger.info(f"[HTTP] Forecast completed for {node_name}")

        return jsonify({
            "node_name": node_name,
            "forecasts": forecasts,
            "confidence": float(avg_confidence)
        })

    except Exception as e:
        logger.error(f"[HTTP] Error forecasting for {node_name}: {e}")
        return jsonify({"error": str(e)}), 500


@app.route('/api/v1/forecast/cluster', methods=['GET'])
def forecast_cluster():
    """클러스터 전체 리소스 예측 API"""
    if servicer is None:
        return jsonify({"error": "Servicer not initialized"}), 503

    logger.info("[HTTP] ForecastCluster request")

    try:
        include_nodes = request.args.get('include_nodes', 'false').lower() == 'true'
        horizons_param = request.args.get('horizons', '15,30,60,120')
        horizons = [int(h) for h in horizons_param.split(',')]

        with servicer.history_lock:
            all_nodes = list(servicer.node_history.keys())

        if not all_nodes:
            # Use real cluster state
            avg_state = servicer.cluster_state.get_cluster_average()
            forecasts = []
            for h in horizons:
                drift = np.random.randn() * 0.03 * (h / 30)
                forecasts.append({
                    "horizon_minutes": h,
                    "predicted_cpu_utilization": float(np.clip(avg_state.get('cpu_util', 0.45) + drift, 0, 1)),
                    "predicted_memory_utilization": float(np.clip(avg_state.get('memory_util', 0.50) + drift, 0, 1)),
                    "predicted_gpu_utilization": float(np.clip(avg_state.get('gpu_util', 0.35) + drift, 0, 1)),
                    "predicted_storage_io_utilization": float(np.clip(avg_state.get('storage_io_util', 0.30) + drift, 0, 1))
                })
            return jsonify({"cluster_forecasts": forecasts, "node_forecasts": {}})

        node_forecasts = {}
        cluster_predictions = {h: [] for h in horizons}

        for node_name in all_nodes:
            history = servicer.node_history.get(node_name, [])
            if len(history) < servicer.min_history_for_prediction:
                continue

            history_array = servicer._history_to_array(history[-60:])
            with servicer.model_lock:
                if getattr(servicer, 'use_attention', False):
                    predictions = servicer.attention_model.predict(sequence=history_array)
                else:
                    predictions = servicer.basic_model.predict(sequence=history_array, horizons=horizons)

            if include_nodes:
                node_forecasts[node_name] = {
                    "forecasts": [
                        {
                            "horizon_minutes": h,
                            "predicted_cpu_utilization": pred['predicted_cpu'],
                            "predicted_memory_utilization": pred['predicted_memory'],
                            "predicted_gpu_utilization": pred.get('predicted_gpu', 0),
                            "predicted_storage_io_utilization": pred.get('predicted_storage_io', 0.3)
                        }
                        for h, pred in predictions.items()
                    ],
                    "confidence": float(np.mean([pred['confidence'] for pred in predictions.values()]))
                }

            for h, pred in predictions.items():
                cluster_predictions[h].append(pred)

        # Calculate cluster averages
        cluster_forecasts = []
        for h in horizons:
            preds = cluster_predictions.get(h, [])
            if preds:
                cluster_forecasts.append({
                    "horizon_minutes": h,
                    "predicted_cpu_utilization": float(np.mean([p['predicted_cpu'] for p in preds])),
                    "predicted_memory_utilization": float(np.mean([p['predicted_memory'] for p in preds])),
                    "predicted_gpu_utilization": float(np.mean([p.get('predicted_gpu', 0) for p in preds])),
                    "predicted_storage_io_utilization": float(np.mean([p.get('predicted_storage_io', 0.3) for p in preds]))
                })

        return jsonify({
            "cluster_forecasts": cluster_forecasts,
            "node_forecasts": node_forecasts
        })

    except Exception as e:
        logger.error(f"[HTTP] Error forecasting cluster: {e}")
        return jsonify({"error": str(e)}), 500


@app.route('/api/v1/policy/<node_name>', methods=['GET'])
def get_orchestration_policy(node_name: str):
    """
    오케스트레이션 정책 결정 API (Multi-LightGBM - 7개 모델)

    발표자료 기반 7가지 정책 결정:
    1. node_health: NORMAL / STRESSED / CRITICAL
    2. autoscale: YES / NO
    3. migration: YES / NO
    4. caching: YES / NO
    5. load_balancing: YES / NO
    6. provisioning: YES / NO
    7. storage_tiering: YES / NO

    Returns:
        OrchestrationPolicy decisions with LSTM predictions
    """
    if servicer is None:
        return jsonify({"error": "Servicer not initialized"}), 503

    logger.info(f"[HTTP] GetOrchestrationPolicy request: node={node_name}")

    try:
        # Check if policy engine is available
        if not hasattr(servicer, 'policy_engine') or servicer.policy_engine is None:
            return jsonify({
                "error": "Policy engine not available",
                "fallback": True,
                "node_name": node_name,
                "decisions": [
                    {
                        "task_name": "node_health",
                        "decision": "NORMAL",
                        "probability": 0.7,
                        "urgency": "LOW",
                        "confidence": 0.5,
                        "reason": "Policy engine not available - rule-based fallback",
                        "parameters": {}
                    }
                ]
            }), 200

        # Get current node state from cluster state manager
        current_state = servicer.cluster_state.get_node_state(node_name)
        cluster_avg = servicer.cluster_state.get_cluster_average()
        current_state['cluster_avg_cpu'] = cluster_avg.get('cpu_util', 0.5)

        # Get LSTM predictions
        with servicer.history_lock:
            history = servicer.node_history.get(node_name, [])

        if len(history) >= servicer.min_history_for_prediction:
            history_array = servicer._history_to_array(history[-60:])
            with servicer.model_lock:
                if getattr(servicer, 'use_attention', False):
                    predictions = servicer.attention_model.predict(sequence=history_array)
                else:
                    predictions = servicer.basic_model.predict(sequence=history_array)
        else:
            # Simulate predictions from current state
            predictions = servicer._simulate_predictions_from_current(current_state)

        # Node info
        node_info = {
            'node_name': node_name,
            'node_type': current_state.get('node_type', 'compute'),
            'queue_status': {'pending': 0, 'admitted': 0}
        }

        # Get Multi-LightGBM policy decisions
        decisions = servicer.policy_engine.predict(current_state, predictions, node_info)
        servicer.policy_decision_count += 1

        # Convert to JSON response
        response = {
            "node_name": decisions.node_name,
            "timestamp": decisions.timestamp,
            "decisions": [
                {
                    "task_name": d.task_name,
                    "decision": d.decision,
                    "probability": d.probability,
                    "urgency": d.urgency,
                    "confidence": d.confidence,
                    "reason": d.reason,
                    "parameters": d.parameters
                }
                for d in decisions.decisions
            ],
            "predictions": {
                "15min": decisions.predicted_15min,
                "30min": decisions.predicted_30min,
                "60min": decisions.predicted_60min,
            }
        }

        logger.info(f"[HTTP] Policy decisions generated for {node_name}: {len(decisions.decisions)} decisions")

        return jsonify(response)

    except Exception as e:
        logger.error(f"[HTTP] Error getting policy for {node_name}: {e}")
        return jsonify({"error": str(e)}), 500


@app.route('/api/v1/policy/recommendations/<node_name>', methods=['GET'])
def get_policy_recommendations(node_name: str):
    """
    정책 권장 사항 API - Policy Generator 호환 포맷

    Multi-LightGBM 결정을 PolicyRecommendation 포맷으로 변환
    기존 orchestration-policy-engine/internal/forecaster/client.go와 호환
    """
    if servicer is None:
        return jsonify({"error": "Servicer not initialized"}), 503

    logger.info(f"[HTTP] GetPolicyRecommendations request: node={node_name}")

    try:
        # Get policy decisions
        if not hasattr(servicer, 'policy_engine') or servicer.policy_engine is None:
            # Fallback to threshold-based recommendations
            return _generate_threshold_based_recommendations(node_name)

        # Get current node state
        current_state = servicer.cluster_state.get_node_state(node_name)
        cluster_avg = servicer.cluster_state.get_cluster_average()
        current_state['cluster_avg_cpu'] = cluster_avg.get('cpu_util', 0.5)

        # Get LSTM predictions
        with servicer.history_lock:
            history = servicer.node_history.get(node_name, [])

        if len(history) >= servicer.min_history_for_prediction:
            history_array = servicer._history_to_array(history[-60:])
            with servicer.model_lock:
                if getattr(servicer, 'use_attention', False):
                    predictions = servicer.attention_model.predict(sequence=history_array)
                else:
                    predictions = servicer.basic_model.predict(sequence=history_array)
        else:
            predictions = servicer._simulate_predictions_from_current(current_state)

        # Node info
        node_info = {
            'node_name': node_name,
            'node_type': current_state.get('node_type', 'compute'),
            'queue_status': {'pending': 0, 'admitted': 0}
        }

        # Get Multi-LightGBM decisions
        decisions = servicer.policy_engine.predict(current_state, predictions, node_info)

        # Convert to PolicyRecommendation format (compatible with Go client)
        recommendations = []

        for d in decisions.decisions:
            if d.decision == "YES" or d.decision in ["STRESSED", "CRITICAL"]:
                # Map task to policy type
                policy_type_map = {
                    'node_health': 'migration' if d.decision == 'CRITICAL' else 'scaling',
                    'autoscale': 'scaling',
                    'migration': 'migration',
                    'caching': 'caching',
                    'load_balancing': 'loadbalance',
                    'provisioning': 'provisioning',
                    'storage_tiering': 'caching',
                }

                # Map task to resource type
                resource_type_map = {
                    'node_health': 'CPU',
                    'autoscale': 'CPU',
                    'migration': 'MEMORY',
                    'caching': 'STORAGE_IO',
                    'load_balancing': 'CPU',
                    'provisioning': 'CPU',
                    'storage_tiering': 'STORAGE_IO',
                }

                # Get predicted utilization from LSTM
                pred_15 = predictions.get(15, {})
                pred_util = max(
                    pred_15.get('predicted_cpu', 0.5),
                    pred_15.get('predicted_memory', 0.5)
                )

                rec = {
                    "node_name": node_name,
                    "policy_type": policy_type_map.get(d.task_name, 'scaling'),
                    "resource_type": resource_type_map.get(d.task_name, 'CPU'),
                    "predicted_utilization": pred_util,
                    "threshold": 0.7 if d.urgency in ['HIGH', 'CRITICAL'] else 0.8,
                    "urgency": d.urgency,
                    "probability": int(d.probability * 100),
                    "horizon_minutes": 15,
                    "reason": d.reason
                }
                recommendations.append(rec)

        # Multi-LightGBM이 전부 NO/NORMAL이면 빈 배열 — LSTM 예측 + 동일 임계치로 보강
        if len(recommendations) == 0:
            lstm_extra = _build_lstm_threshold_recommendations(node_name)
            recommendations.extend(lstm_extra)
            logger.info(
                f"[HTTP] LightGBM yielded 0 YES/STRESSED/CRITICAL; "
                f"added {len(lstm_extra)} LSTM-threshold recommendations for {node_name}"
            )

        logger.info(f"[HTTP] Generated {len(recommendations)} recommendations for {node_name}")
        # PolicyRecommendation 배열 전체를 한 줄 JSON으로 남겨 policy_type 조합을 로그로 검증한다.
        try:
            logger.info(
                "[HTTP] recommendations dump node=%s json=%s",
                node_name,
                json.dumps(recommendations, ensure_ascii=False),
            )
        except (TypeError, ValueError) as dump_err:
            logger.warning("[HTTP] recommendations dump failed node=%s: %s", node_name, dump_err)

        return jsonify(recommendations)

    except Exception as e:
        logger.error(f"[HTTP] Error generating recommendations for {node_name}: {e}")
        return jsonify({"error": str(e)}), 500


def _float_env(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, str(default)))
    except (TypeError, ValueError):
        return default


def _build_lstm_threshold_recommendations(node_name: str) -> List[Dict[str, Any]]:
    """LSTM(또는 시뮬레이션) 예측과 env 임계치로 PolicyRecommendation 리스트 생성."""
    if servicer is None:
        return []

    current_state = servicer.cluster_state.get_node_state(node_name)
    with servicer.history_lock:
        history = servicer.node_history.get(node_name, [])

    if len(history) >= servicer.min_history_for_prediction:
        history_array = servicer._history_to_array(history[-60:])
        with servicer.model_lock:
            if getattr(servicer, 'use_attention', False):
                predictions = servicer.attention_model.predict(sequence=history_array)
            else:
                predictions = servicer.basic_model.predict(sequence=history_array)
    else:
        predictions = servicer._simulate_predictions_from_current(current_state)

    pred_15 = predictions.get(15, {}) if isinstance(predictions, dict) else {}
    cpu_p = float(pred_15.get('predicted_cpu', current_state.get('cpu_util', 0.5)))
    mem_p = float(pred_15.get('predicted_memory', current_state.get('memory_util', 0.5)))
    gpu_p = float(pred_15.get('predicted_gpu', current_state.get('gpu_util', 0.0)))
    io_p = float(pred_15.get('predicted_storage_io', current_state.get('storage_io_util', 0.3)))

    cw = _float_env('FORECASTER_CPU_WARNING', 0.42)
    cc = _float_env('FORECASTER_CPU_CRITICAL', 0.85)
    mw = _float_env('FORECASTER_MEMORY_WARNING', 0.42)
    mc = _float_env('FORECASTER_MEMORY_CRITICAL', 0.90)
    gw = _float_env('FORECASTER_GPU_WARNING', 0.42)
    gc = _float_env('FORECASTER_GPU_CRITICAL', 0.95)
    sw = _float_env('FORECASTER_STORAGE_WARNING', 0.28)
    sc = _float_env('FORECASTER_STORAGE_CRITICAL', 0.85)

    out: List[Dict[str, Any]] = []

    if cpu_p >= cc:
        out.append({
            "node_name": node_name,
            "policy_type": "migration",
            "resource_type": "CPU",
            "predicted_utilization": cpu_p,
            "threshold": cc,
            "urgency": "CRITICAL",
            "probability": min(100, int(cpu_p * 100)),
            "horizon_minutes": 15,
            "reason": f"LSTM CPU forecast {cpu_p:.3f} >= critical {cc}",
        })
    elif cpu_p >= cw:
        out.append({
            "node_name": node_name,
            "policy_type": "scaling",
            "resource_type": "CPU",
            "predicted_utilization": cpu_p,
            "threshold": cw,
            "urgency": "HIGH",
            "probability": min(100, int(cpu_p * 100)),
            "horizon_minutes": 30,
            "reason": f"LSTM CPU forecast {cpu_p:.3f} >= warning {cw}",
        })

    if mem_p >= mc:
        out.append({
            "node_name": node_name,
            "policy_type": "migration",
            "resource_type": "MEMORY",
            "predicted_utilization": mem_p,
            "threshold": mc,
            "urgency": "CRITICAL",
            "probability": min(100, int(mem_p * 100)),
            "horizon_minutes": 15,
            "reason": f"LSTM memory forecast {mem_p:.3f} >= critical {mc}",
        })
    elif mem_p >= mw:
        out.append({
            "node_name": node_name,
            "policy_type": "scaling",
            "resource_type": "MEMORY",
            "predicted_utilization": mem_p,
            "threshold": mw,
            "urgency": "HIGH",
            "probability": min(100, int(mem_p * 100)),
            "horizon_minutes": 30,
            "reason": f"LSTM memory forecast {mem_p:.3f} >= warning {mw}",
        })

    if gpu_p >= gc:
        out.append({
            "node_name": node_name,
            "policy_type": "preemption",
            "resource_type": "GPU",
            "predicted_utilization": gpu_p,
            "threshold": gc,
            "urgency": "CRITICAL",
            "probability": min(100, int(gpu_p * 100)),
            "horizon_minutes": 15,
            "reason": f"LSTM GPU forecast {gpu_p:.3f} >= critical {gc}",
        })
    elif gpu_p >= gw:
        out.append({
            "node_name": node_name,
            "policy_type": "provisioning",
            "resource_type": "GPU",
            "predicted_utilization": gpu_p,
            "threshold": gw,
            "urgency": "HIGH",
            "probability": min(100, int(gpu_p * 100)),
            "horizon_minutes": 30,
            "reason": f"LSTM GPU forecast {gpu_p:.3f} >= warning {gw}",
        })

    if io_p >= sc:
        out.append({
            "node_name": node_name,
            "policy_type": "caching",
            "resource_type": "STORAGE_IO",
            "predicted_utilization": io_p,
            "threshold": sc,
            "urgency": "CRITICAL",
            "probability": min(100, int(io_p * 100)),
            "horizon_minutes": 15,
            "reason": f"LSTM storage I/O forecast {io_p:.3f} >= critical {sc}",
        })
    elif io_p >= sw:
        out.append({
            "node_name": node_name,
            "policy_type": "loadbalance",
            "resource_type": "STORAGE_IO",
            "predicted_utilization": io_p,
            "threshold": sw,
            "urgency": "HIGH",
            "probability": min(100, int(io_p * 100)),
            "horizon_minutes": 30,
            "reason": f"LSTM storage I/O forecast {io_p:.3f} >= warning {sw}",
        })

    return out


def _generate_threshold_based_recommendations(node_name: str):
    """Threshold 기반 권장 사항 생성 (Policy Engine 없을 때 fallback) — cluster_state + env 임계치."""
    current_state = servicer.cluster_state.get_node_state(node_name)

    recommendations = []

    cpu_util = current_state.get('cpu_util', 0.5)
    cw = _float_env('FORECASTER_CPU_WARNING', 0.42)
    cc = _float_env('FORECASTER_CPU_CRITICAL', 0.85)
    if cpu_util >= cc:
        recommendations.append({
            "node_name": node_name,
            "policy_type": "migration",
            "resource_type": "CPU",
            "predicted_utilization": cpu_util,
            "threshold": cc,
            "urgency": "CRITICAL",
            "probability": min(100, int(cpu_util * 100)),
            "horizon_minutes": 15,
            "reason": f"CPU utilization {cpu_util*100:.1f}% exceeds critical threshold",
        })
    elif cpu_util >= cw:
        recommendations.append({
            "node_name": node_name,
            "policy_type": "scaling",
            "resource_type": "CPU",
            "predicted_utilization": cpu_util,
            "threshold": cw,
            "urgency": "HIGH",
            "probability": min(100, int(cpu_util * 100)),
            "horizon_minutes": 30,
            "reason": f"CPU utilization {cpu_util*100:.1f}% exceeds warning threshold",
        })

    mem_util = current_state.get('memory_util', 0.5)
    mw = _float_env('FORECASTER_MEMORY_WARNING', 0.42)
    mc = _float_env('FORECASTER_MEMORY_CRITICAL', 0.90)
    if mem_util >= mc:
        recommendations.append({
            "node_name": node_name,
            "policy_type": "migration",
            "resource_type": "MEMORY",
            "predicted_utilization": mem_util,
            "threshold": mc,
            "urgency": "CRITICAL",
            "probability": min(100, int(mem_util * 100)),
            "horizon_minutes": 15,
            "reason": f"Memory utilization {mem_util*100:.1f}% exceeds critical threshold",
        })
    elif mem_util >= mw:
        recommendations.append({
            "node_name": node_name,
            "policy_type": "scaling",
            "resource_type": "MEMORY",
            "predicted_utilization": mem_util,
            "threshold": mw,
            "urgency": "HIGH",
            "probability": min(100, int(mem_util * 100)),
            "horizon_minutes": 30,
            "reason": f"Memory utilization {mem_util*100:.1f}% exceeds warning threshold",
        })

    try:
        logger.info(
            "[HTTP] recommendations dump (threshold_fallback policy_engine=None) node=%s json=%s",
            node_name,
            json.dumps(recommendations, ensure_ascii=False),
        )
    except (TypeError, ValueError) as dump_err:
        logger.warning("[HTTP] recommendations dump failed node=%s: %s", node_name, dump_err)

    return jsonify(recommendations)


@app.route('/api/v1/peak-idle', methods=['GET'])
def get_peak_idle():
    """피크/유휴 시간대 예측 API"""
    if servicer is None:
        return jsonify({"error": "Servicer not initialized"}), 503

    try:
        node_name = request.args.get('node_name', '')
        lookahead_hours = int(request.args.get('lookahead_hours', '1'))

        with servicer.history_lock:
            if node_name:
                history = servicer.node_history.get(node_name, [])
            else:
                all_histories = [h for h in servicer.node_history.values() if h]
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

        if len(history) < servicer.min_history_for_prediction:
            # Return based on real cluster state
            avg_state = servicer.cluster_state.get_cluster_average()
            avg_util = (avg_state.get('cpu_util', 0.5) + avg_state.get('memory_util', 0.5)) / 2

            return jsonify({
                "peak_periods": [{"horizon_minutes": 30, "avg_utilization": min(avg_util + 0.2, 0.9), "confidence": 0.5}],
                "idle_periods": [{"horizon_minutes": 60, "avg_utilization": max(avg_util - 0.2, 0.1), "confidence": 0.5}],
                "recommended_batch_window": {
                    "start_offset_minutes": 55,
                    "end_offset_minutes": 65,
                    "expected_utilization": max(avg_util - 0.25, 0.15)
                }
            })

        history_array = servicer._history_to_array(history[-60:])

        with servicer.model_lock:
            result = servicer.peak_idle_predictor.predict_periods(
                historical_data=history_array,
                lookahead_steps=lookahead_hours * 60
            )

        return jsonify(result)

    except Exception as e:
        logger.error(f"[HTTP] Error predicting peak/idle: {e}")
        return jsonify({"error": str(e)}), 500


def run_http_server(port: int = 8080):
    """Start HTTP server in a separate thread"""
    logger.info(f"[HTTP] Starting HTTP REST API server on port {port}")
    logger.info(f"[HTTP] Endpoints:")
    logger.info(f"[HTTP]   GET /health - Health check")
    logger.info(f"[HTTP]   GET /api/v1/forecast/node/<node> - Node forecast (LSTM-Attention)")
    logger.info(f"[HTTP]   GET /api/v1/forecast/cluster - Cluster forecast")
    logger.info(f"[HTTP]   GET /api/v1/policy/<node> - Orchestration policy (Multi-LightGBM)")
    logger.info(f"[HTTP]   GET /api/v1/policy/recommendations/<node> - Policy recommendations")
    logger.info(f"[HTTP]   GET /api/v1/peak-idle - Peak/Idle prediction")
    app.run(host='0.0.0.0', port=port, threaded=True, use_reloader=False)


def start_http_server_thread(port: int = 8080):
    """Start HTTP server in background thread"""
    http_thread = threading.Thread(target=run_http_server, args=(port,), daemon=True)
    http_thread.start()
    logger.info(f"[HTTP] HTTP server thread started on port {port}")
    return http_thread
