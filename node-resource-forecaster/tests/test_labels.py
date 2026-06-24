import numpy as np
from training.data.replay import ReplayResult
from training.labels import derive_labels, STRESSED, CRITICAL


def _replay_rising(n=200):
    t = list(range(0, n * 60, 60))
    occ = {"cpu": np.linspace(0.1, 0.99, n),
           "memory": np.linspace(0.1, 0.5, n),
           "gpu": np.linspace(0.1, 0.95, n)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=np.zeros(n, dtype=int),
                        queue_admitted=np.zeros(n, dtype=int))


def test_label_shapes_align_and_classes_present():
    r = _replay_rising(600)
    y = derive_labels(r, label_horizon=30)
    assert set(y) == {"node_health", "autoscale", "migration", "provisioning"}
    n = len(y["node_health"])
    assert all(len(y[k]) == n for k in y)
    # node_health is multiclass with values in {0,1,2}
    assert set(np.unique(y["node_health"])).issubset({0, 1, 2})
    # rising cpu -> at least one CRITICAL migration label and one critical node_health
    assert y["migration"].max() == 1
    assert y["node_health"].max() == 2
    # provisioning fires where future gpu >= STRESSED
    assert y["provisioning"].max() == 1
