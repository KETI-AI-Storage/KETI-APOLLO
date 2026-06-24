import numpy as np
import torch
from training.train_lstm import train_lstm, save_lstm, make_batches
from models.forecaster_config import make_occupancy_forecaster_config
from models.lstm_attention import create_lstm_attention_forecaster


def _data(n=40):
    X = np.random.RandomState(0).rand(n, 60, 4).astype("float32")
    Y = {h: np.random.RandomState(h).rand(n, 4).astype("float32") for h in (15, 30, 60, 120)}
    return X, Y


def test_make_batches_shapes():
    X, Y = _data(40)
    batches = make_batches(X, Y, batch_size=16)
    bx, by = batches[0]
    assert bx.shape == (16, 60, 4)
    assert set(by.keys()) == {15, 30, 60, 120}
    assert by[15].shape == (16, 4)


def test_train_and_load_roundtrip(tmp_path):
    X, Y = _data(40)
    model, trainer = train_lstm(X, Y, epochs=1, batch_size=16, seed=0)
    out = tmp_path / "lstm_attention.pt"
    save_lstm(trainer, str(out))
    assert out.exists()
    # reload into a fresh model built from the SAME shared config
    _, fresh_trainer = create_lstm_attention_forecaster(make_occupancy_forecaster_config())
    fresh_trainer.load(str(out))
    pred = fresh_trainer.model.predict(X[0])
    assert 15 in pred and "predicted_cpu" in pred[15]
    assert np.isfinite(pred[15]["predicted_cpu"])
