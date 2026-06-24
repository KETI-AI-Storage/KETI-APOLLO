"""Train the 4 data-supported LightGBM heads.

Features are built by the engine's OWN _build_features (train==serve). The LSTM
predictions used as features are produced by the trained LSTM, matching serving.
Only the 4 supported heads are passed to train_models; the other 3 stay
rule-based (train_models skips tasks absent from the dicts).
"""
import numpy as np

from training.features import current_state_at, node_info_at, build_lstm_sequences
from training.labels import derive_labels
from models.multi_lightgbm import create_policy_engine

TRAINED_HEADS = ["node_health", "autoscale", "migration", "provisioning"]


def _windows(replay, seq_len):
    # same channel stack + windowing as build_lstm_sequences, to get input windows
    X, _ = build_lstm_sequences(replay, seq_len=seq_len)
    return X  # (n_samples, seq_len, 4)


def build_lightgbm_matrix(replay, lstm_model, seq_len: int = 60) -> np.ndarray:
    engine = create_policy_engine(None)  # rule mode; we only use _build_features
    windows = _windows(replay, seq_len)
    rows = []
    for i in range(windows.shape[0]):
        idx = i + seq_len
        preds = lstm_model.predict(windows[i])           # {h: {predicted_cpu,...}}
        state = current_state_at(replay, idx)
        info = node_info_at(replay, idx)
        feat = engine._build_features(state, preds, info)  # (1, 25)
        rows.append(feat[0])
    return np.asarray(rows, dtype=np.float32)


def train_lightgbm(replay, lstm_model, out_dir: str, seq_len: int = 60):
    X = build_lightgbm_matrix(replay, lstm_model, seq_len=seq_len)
    labels = derive_labels(replay, label_horizon=30, seq_len=seq_len)
    n = min(X.shape[0], *(len(v) for v in labels.values()))
    X = X[:n]
    training_data = {h: X for h in TRAINED_HEADS}
    labels = {h: labels[h][:n] for h in TRAINED_HEADS}
    engine = create_policy_engine(None)
    engine.train_models(training_data, labels)
    engine.save_models(out_dir)
    return engine
