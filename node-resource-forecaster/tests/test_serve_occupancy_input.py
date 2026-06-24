"""
Tests for the serve-time 4-channel occupancy LSTM input builder.

Verifies:
1. build_occupancy_array returns the right shape and channel values.
2. The output can be fed directly into a model built from
   make_occupancy_forecaster_config() without a shape error (train==serve).
"""
import numpy as np
import pytest


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

def _make_history(n: int, cpu=0.4, memory=0.6, gpu=0.2, pending=10):
    """Return a list of n history dicts with constant values."""
    return [
        {'cpu': cpu, 'memory': memory, 'gpu': gpu, 'pending': pending}
        for _ in range(n)
    ]


# ---------------------------------------------------------------------------
# Import the module-level helper (no servicer construction needed)
# ---------------------------------------------------------------------------

def _import_builder():
    from server.grpc_server import build_occupancy_array
    return build_occupancy_array


# ---------------------------------------------------------------------------
# Shape tests
# ---------------------------------------------------------------------------

def test_shape_exact_seq_len():
    builder = _import_builder()
    history = _make_history(60)
    arr = builder(history, seq_len=60)
    assert arr.shape == (60, 4), f"expected (60, 4), got {arr.shape}"
    assert arr.dtype == np.float32


def test_shape_longer_than_seq_len_truncates():
    builder = _import_builder()
    history = _make_history(80)
    arr = builder(history, seq_len=60)
    assert arr.shape == (60, 4)


def test_shape_shorter_than_seq_len_pads():
    builder = _import_builder()
    history = _make_history(20)
    arr = builder(history, seq_len=60)
    assert arr.shape == (60, 4)
    # First 40 rows should be zero-padding
    assert np.all(arr[:40] == 0.0), "front-padding rows should be zero"


# ---------------------------------------------------------------------------
# Channel value tests
# ---------------------------------------------------------------------------

def test_channel_values_cpu_memory_gpu():
    builder = _import_builder()
    history = _make_history(60, cpu=0.3, memory=0.7, gpu=0.1, pending=0)
    arr = builder(history, seq_len=60)
    np.testing.assert_allclose(arr[:, 0], 0.3, rtol=1e-5, err_msg="ch0 (cpu)")
    np.testing.assert_allclose(arr[:, 1], 0.7, rtol=1e-5, err_msg="ch1 (memory)")
    np.testing.assert_allclose(arr[:, 2], 0.1, rtol=1e-5, err_msg="ch2 (gpu)")


def test_channel_pending_normalized():
    builder = _import_builder()
    # pending=25 → 25/50 = 0.5
    history = _make_history(60, pending=25)
    arr = builder(history, seq_len=60)
    np.testing.assert_allclose(arr[:, 3], 0.5, rtol=1e-5, err_msg="ch3 (pending norm 25→0.5)")


def test_channel_pending_clipped_at_one():
    builder = _import_builder()
    # pending=200 >> 50 → clipped to 1.0
    history = _make_history(60, pending=200)
    arr = builder(history, seq_len=60)
    np.testing.assert_allclose(arr[:, 3], 1.0, rtol=1e-5, err_msg="ch3 clipped at 1.0")


def test_channel_pending_default_zero():
    builder = _import_builder()
    # history entries with no 'pending' key → default 0 → 0/50 = 0.0
    history = [{'cpu': 0.5, 'memory': 0.5, 'gpu': 0.0} for _ in range(60)]
    arr = builder(history, seq_len=60)
    np.testing.assert_allclose(arr[:, 3], 0.0, rtol=1e-5, err_msg="ch3 missing key → 0.0")


# ---------------------------------------------------------------------------
# Critical: model.predict on 4-ch array must succeed (no shape error)
# ---------------------------------------------------------------------------

def test_model_predict_on_4ch_array_succeeds():
    """
    Build a fresh model from make_occupancy_forecaster_config() (input_dim=4)
    and call model.predict on a (60, 4) occupancy array.

    This is the critical regression test: if the serve-time input is still
    12-channel, the Linear(4,96) first layer raises a shape error.
    """
    from models.forecaster_config import make_occupancy_forecaster_config
    from models.lstm_attention import create_lstm_attention_forecaster

    cfg = make_occupancy_forecaster_config()
    model, _trainer = create_lstm_attention_forecaster(cfg)

    builder = _import_builder()
    history = _make_history(60, cpu=0.4, memory=0.6, gpu=0.2, pending=10)
    arr = builder(history, seq_len=60)  # (60, 4)

    assert arr.shape == (60, 4), "pre-condition: array must be (60, 4)"

    # Must not raise — this is the core regression guard
    preds = model.predict(sequence=arr)

    # Predictions should be a dict keyed by horizon
    assert isinstance(preds, dict), "predict should return a dict"
    assert len(preds) > 0, "predictions dict should be non-empty"

    # All predicted values should be finite
    for horizon, pred in preds.items():
        for key, val in pred.items():
            v = val if np.isscalar(val) else np.asarray(val).ravel()[0]
            assert np.isfinite(v), f"horizon={horizon} key={key} val={val} is not finite"
