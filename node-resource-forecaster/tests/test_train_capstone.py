import os
import numpy as np
from training.train import run_training
from training.data.replay import ReplayResult


def _replay(n=300):
    t = list(range(0, n * 60, 60))
    x = np.linspace(0, 12.5, n)
    occ = {"cpu": np.clip(0.5 + 0.35 * np.sin(x), 0, 1),
           "memory": np.clip(0.4 + 0.2 * np.cos(x), 0, 1),
           "gpu": np.clip(0.3 + 0.3 * np.sin(x / 2), 0, 1)}
    return ReplayResult(times=t, occupancy=occ,
                        queue_pending=(np.abs(np.sin(x)) * 20).astype(int),
                        queue_admitted=np.ones(n, dtype=int))


def test_capstone_produces_all_artifacts(tmp_path):
    rep = run_training(out_dir=str(tmp_path), cache_dir=str(tmp_path / "cache"),
                       lstm_epochs=1, seed=0, replay=_replay(300))
    for f in ("lstm_attention.pt", "node_health_model.txt", "autoscale_model.txt",
              "migration_model.txt", "provisioning_model.txt",
              "eval_report.json", "eval_report.md"):
        assert os.path.exists(tmp_path / f), f"missing {f}"
    assert "lstm" in rep and "lightgbm" in rep
