"""Train the existing LSTM-Attention forecaster on occupancy sequences.

Self-supervised: predict future occupancy channels from a 60-step window.
Reuses LSTMAttentionTrainer unchanged; builds the dict-batch format it expects.
"""
import random
import numpy as np
import torch

from models.forecaster_config import make_occupancy_forecaster_config
from models.lstm_attention import create_lstm_attention_forecaster


def make_batches(X, Y, batch_size: int):
    n = X.shape[0]
    batches = []
    for s in range(0, n, batch_size):
        e = min(s + batch_size, n)
        bx = torch.from_numpy(np.asarray(X[s:e], dtype="float32"))
        by = {h: torch.from_numpy(np.asarray(Y[h][s:e], dtype="float32")) for h in Y}
        batches.append((bx, by))
    return batches


def train_lstm(X, Y, epochs: int = 5, batch_size: int = 32, seed: int = 0):
    random.seed(seed); np.random.seed(seed); torch.manual_seed(seed)
    cfg = make_occupancy_forecaster_config()
    cfg.batch_size = batch_size
    model, trainer = create_lstm_attention_forecaster(cfg)
    batches = make_batches(X, Y, batch_size)
    for ep in range(epochs):
        loss = trainer.train_epoch(batches)
        print(f"[train_lstm] epoch {ep+1}/{epochs} loss={loss:.5f}")
    return model, trainer


def save_lstm(trainer, out_path: str):
    trainer.save(out_path)
