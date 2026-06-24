import os
from training.data import acquire


def test_ensure_trace_skips_download_when_cached(tmp_path, monkeypatch):
    # Pre-create cached files; download must NOT be called.
    node = tmp_path / acquire.NODE_FILE
    pod = tmp_path / acquire.POD_FILE
    node.write_text("cpu_milli,memory_mib,gpu\n1000,2000,2\n")
    pod.write_text("cpu_milli,memory_mib,num_gpu,creation_time,scheduled_time,deletion_time\n500,1000,1,0,60,180\n")

    called = {"n": 0}
    monkeypatch.setattr(acquire, "_download", lambda url, dst: called.__setitem__("n", called["n"] + 1))

    n_path, p_path = acquire.ensure_trace(str(tmp_path))
    assert os.path.exists(n_path) and os.path.exists(p_path)
    assert called["n"] == 0  # no download because cached


def test_load_trace_returns_dataframes(tmp_path, monkeypatch):
    (tmp_path / acquire.NODE_FILE).write_text("cpu_milli,memory_mib,gpu\n1000,2000,2\n")
    (tmp_path / acquire.POD_FILE).write_text(
        "cpu_milli,memory_mib,num_gpu,creation_time,scheduled_time,deletion_time\n500,1000,1,0,60,180\n")
    monkeypatch.setattr(acquire, "_download", lambda url, dst: (_ for _ in ()).throw(AssertionError("should not download")))
    nodes_df, pods_df = acquire.load_trace(str(tmp_path))
    assert list(nodes_df.columns)[:3] == ["cpu_milli", "memory_mib", "gpu"]
    assert int(pods_df.iloc[0]["num_gpu"]) == 1
