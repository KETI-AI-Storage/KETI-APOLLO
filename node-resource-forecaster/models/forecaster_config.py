"""Shared LSTM-Attention config for the allocation-occupancy forecaster.

SINGLE SOURCE for input_dim/output_dim so the trained artifact and the serving
model are constructed identically (else state_dict load shape-mismatches).
"""
from models.lstm_attention import LSTMAttentionConfig

OCCUPANCY_CHANNELS = ["cpu", "memory", "gpu", "pending"]
OCCUPANCY_INPUT_DIM = 4


def make_occupancy_forecaster_config() -> LSTMAttentionConfig:
    return LSTMAttentionConfig(
        input_dim=OCCUPANCY_INPUT_DIM,
        output_dim=4,
        sequence_length=60,
        forecast_horizons=[15, 30, 60, 120],
    )
