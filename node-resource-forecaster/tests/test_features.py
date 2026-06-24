import numpy as np
from training.data.replay import ReplayResult
from training.features import (
    build_lstm_sequences, current_state_at, node_info_at, PENDING_NORM,
)


def _replay(n=200):
    t = list(range(0, n * 60, 60))
    occ = {
        "cpu": np.linspace(0.1, 0.9, n),
        "memory": np.linspace(0.2, 0.6, n),
        "gpu": np.full(n, 0.3),
    }
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=np.arange(n) % 10,
                        queue_admitted=np.ones(n, dtype=int))


def test_sequence_shapes_and_channels():
    r = _replay(200)
    X, Y = build_lstm_sequences(r, seq_len=60)
    assert X.shape[1:] == (60, 4)
    assert set(Y.keys()) == {15, 30, 60, 120}
    assert Y[120].shape[0] == X.shape[0]
    assert Y[15].shape[1] == 4
    # channel 0 of last input row equals cpu occupancy at that step
    assert abs(X[0, -1, 0] - r.occupancy["cpu"][59]) < 1e-6
    # pending channel normalized into [0,1]
    assert X[:, :, 3].max() <= 1.0


def test_current_state_contract():
    r = _replay(70)
    s = current_state_at(r, 10)
    assert set(s) >= {"cpu_util", "memory_util", "gpu_util", "pending_pods"}
    info = node_info_at(r, 10)
    assert info["queue_status"]["pending"] == int(r.queue_pending[10])
