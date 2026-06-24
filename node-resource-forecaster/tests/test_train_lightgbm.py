import os
import numpy as np
from training.data.replay import ReplayResult
from training.train_lightgbm import train_lightgbm, build_lightgbm_matrix, TRAINED_HEADS
from training.train_lstm import train_lstm
from training.features import build_lstm_sequences


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    rng = np.random.RandomState(1)
    occ = {"cpu": np.clip(np.linspace(0.1, 0.95, n) + rng.rand(n) * 0.05, 0, 1),
           "memory": np.clip(np.linspace(0.1, 0.7, n), 0, 1),
           "gpu": np.clip(np.linspace(0.1, 0.9, n), 0, 1)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=(rng.rand(n) * 30).astype(int),
                        queue_admitted=np.ones(n, dtype=int))


def test_matrix_is_25_wide_and_train_writes_4_models(tmp_path):
    r = _replay(300)
    X, Y = build_lstm_sequences(r, seq_len=60)
    model, _ = train_lstm(X, Y, epochs=1, batch_size=32, seed=0)
    M = build_lightgbm_matrix(r, model, seq_len=60)
    assert M.shape[1] == 25
    engine = train_lightgbm(r, model, str(tmp_path), seq_len=60)
    for head in TRAINED_HEADS:
        assert os.path.exists(tmp_path / f"{head}_model.txt")
    # the 3 rule heads must NOT be trained/written
    for head in ("caching", "storage_tiering", "load_balancing"):
        assert not os.path.exists(tmp_path / f"{head}_model.txt")
        assert head not in engine.models
