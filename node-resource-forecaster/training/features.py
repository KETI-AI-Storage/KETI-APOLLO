"""Turn a ReplayResult into LSTM training sequences and into the engine's
`current_state` / `node_info` dicts. The 25-feature LightGBM vector is built by
the engine's own `_build_features` (Task 7) — NOT here — so train==serve.
"""
import numpy as np

HORIZONS = [15, 30, 60, 120]
PENDING_NORM = 50.0  # pending count -> [0,1] pressure (clipped)


def _channels(replay):
    pending_norm = np.clip(replay.queue_pending / PENDING_NORM, 0.0, 1.0)
    return np.stack([
        replay.occupancy["cpu"],
        replay.occupancy["memory"],
        replay.occupancy["gpu"],
        pending_norm,
    ], axis=1)  # (n_steps, 4)


def build_lstm_sequences(replay, seq_len: int = 60):
    chans = _channels(replay)            # (T, 4)
    T = chans.shape[0]
    max_h = max(HORIZONS)
    X, Y = [], {h: [] for h in HORIZONS}
    last = T - seq_len - max_h
    for i in range(0, max(last, 0)):
        X.append(chans[i:i + seq_len])
        base = i + seq_len
        for h in HORIZONS:
            Y[h].append(chans[base + h])
    X = np.asarray(X, dtype=np.float32)
    Y = {h: np.asarray(v, dtype=np.float32) for h, v in Y.items()}
    return X, Y


def current_state_at(replay, idx: int) -> dict:
    return {
        "cpu_util": float(replay.occupancy["cpu"][idx]),
        "memory_util": float(replay.occupancy["memory"][idx]),
        "gpu_util": float(replay.occupancy["gpu"][idx]),
        "pending_pods": float(min(replay.queue_pending[idx] / PENDING_NORM, 1.0)),
    }


def node_info_at(replay, idx: int, node_type: str = "gpu") -> dict:
    return {
        "node_name": "trace-cluster",
        "node_type": node_type,
        "queue_status": {
            "pending": int(replay.queue_pending[idx]),
            "admitted": int(replay.queue_admitted[idx]),
        },
    }


def lstm_pred_to_engine_dict(model_predict_out: dict) -> dict:
    # LSTMAttentionForecaster.predict already returns {h: {predicted_cpu, ...}},
    # which is exactly the engine's lstm_predictions contract.
    return model_predict_out
