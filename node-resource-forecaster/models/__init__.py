# Node Resource Forecaster Models
"""
APOLLO Node Resource Forecaster Models
- LSTMForecaster: 기본 LSTM 예측 모델
- LSTMAttentionForecaster: LSTM-Attention 예측 모델 (발표자료 기반)
- MultiLightGBMPolicyEngine: 7개 정책 결정 모델 (발표자료 기반)
"""

# Basic LSTM (backward compatibility)
from models.lstm_forecaster import (
    LSTMForecaster,
    ForecastConfig,
    ForecastTrainer,
    PeakIdlePredictor,
    create_forecaster
)

# LSTM-Attention (발표자료 구현)
from models.lstm_attention import (
    LSTMAttentionForecaster,
    LSTMAttentionConfig,
    LSTMAttentionTrainer,
    TemporalAttention,
    PositionalEncoding,
    create_lstm_attention_forecaster
)

# Multi-LightGBM Policy Engine (발표자료 구현)
from models.multi_lightgbm import (
    MultiLightGBMPolicyEngine,
    PolicyDecision,
    OrchestrationDecisions,
    create_policy_engine
)

__all__ = [
    # Basic LSTM
    'LSTMForecaster',
    'ForecastConfig',
    'ForecastTrainer',
    'PeakIdlePredictor',
    'create_forecaster',
    # LSTM-Attention
    'LSTMAttentionForecaster',
    'LSTMAttentionConfig',
    'LSTMAttentionTrainer',
    'TemporalAttention',
    'PositionalEncoding',
    'create_lstm_attention_forecaster',
    # Multi-LightGBM
    'MultiLightGBMPolicyEngine',
    'PolicyDecision',
    'OrchestrationDecisions',
    'create_policy_engine',
]
