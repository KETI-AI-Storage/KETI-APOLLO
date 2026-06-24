import os
import numpy as np
from training.evaluate import lstm_rmse_vs_persistence, write_report, _head_verdict
from training.train_lstm import train_lstm
from training.features import build_lstm_sequences
from training.data.replay import ReplayResult


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    # smooth, learnable signal -> LSTM should beat (or match) persistence
    x = np.linspace(0, 6.28, n)
    occ = {"cpu": 0.5 + 0.3 * np.sin(x), "memory": 0.5 + 0.2 * np.cos(x),
           "gpu": np.full(n, 0.3)}
    return ReplayResult(times=t, occupancy={k: np.clip(v, 0, 1) for k, v in occ.items()},
                        queue_pending=np.zeros(n, dtype=int),
                        queue_admitted=np.zeros(n, dtype=int))


def test_head_verdict_not_evaluable_when_no_positives():
    """not_evaluable when n_positive=0, regardless of F1 values."""
    assert _head_verdict(1.0, 0.0, n_positive=0) == "not_evaluable"
    assert _head_verdict(0.0, 0.0, n_positive=0) == "not_evaluable"
    assert _head_verdict(0.5, 0.3, n_positive=0) == "not_evaluable"


def test_head_verdict_model_better():
    """model_better when n_positive > 0 and model_f1 > rule_f1."""
    assert _head_verdict(0.8, 0.5, n_positive=10) == "model_better"
    assert _head_verdict(0.1, 0.0, n_positive=1) == "model_better"


def test_head_verdict_tie():
    """tie when n_positive > 0 and model_f1 == rule_f1."""
    assert _head_verdict(0.5, 0.5, n_positive=5) == "tie"
    assert _head_verdict(0.0, 0.0, n_positive=3) == "tie"


def test_head_verdict_rule_better():
    """rule_better when n_positive > 0 and model_f1 < rule_f1."""
    assert _head_verdict(0.4, 0.9, n_positive=7) == "rule_better"
    assert _head_verdict(0.0, 0.5, n_positive=2) == "rule_better"


def test_lstm_eval_keys_and_report(tmp_path):
    r = _replay(300)
    X, Y = build_lstm_sequences(r, seq_len=60)
    model, _ = train_lstm(X, Y, epochs=2, batch_size=32, seed=0)
    res = lstm_rmse_vs_persistence(model, X, Y)
    assert {"lstm_rmse", "persistence_rmse", "beats_baseline"} <= set(res)
    assert np.isfinite(res["lstm_rmse"]) and res["lstm_rmse"] >= 0
    write_report({"lstm": res, "lightgbm": {}}, str(tmp_path))
    assert os.path.exists(tmp_path / "eval_report.json")
    assert os.path.exists(tmp_path / "eval_report.md")
