"""Capstone: acquire -> replay -> train LSTM -> train LightGBM -> evaluate -> save.

Deterministic (seeded). Run at Docker build (sampled) or offline (full).
"""
import argparse
import os
import random
import numpy as np
import torch

from training.data.acquire import load_trace
from training.data.replay import replay_trace
from training.features import build_lstm_sequences
from training.train_lstm import train_lstm, save_lstm
from training.train_lightgbm import train_lightgbm
from training.evaluate import lstm_rmse_vs_persistence, lightgbm_vs_threshold, write_report


def run_training(out_dir, cache_dir, sample_pods=None, lstm_epochs=10, seed=0, replay=None):
    random.seed(seed); np.random.seed(seed); torch.manual_seed(seed)
    os.makedirs(out_dir, exist_ok=True)

    if replay is None:
        nodes_df, pods_df = load_trace(cache_dir)
        if sample_pods:
            pods_df = pods_df.sample(n=min(sample_pods, len(pods_df)), random_state=seed)
        replay = replay_trace(nodes_df, pods_df, step_seconds=60)

    X, Y = build_lstm_sequences(replay, seq_len=60)
    if X.shape[0] == 0:
        raise RuntimeError("Not enough trace steps to build sequences; widen sample/time span.")

    lstm_model, trainer = train_lstm(X, Y, epochs=lstm_epochs, batch_size=32, seed=seed)
    save_lstm(trainer, os.path.join(out_dir, "lstm_attention.pt"))

    engine = train_lightgbm(replay, lstm_model, out_dir, seq_len=60)

    report = {
        "lstm": lstm_rmse_vs_persistence(lstm_model, X, Y),
        "lightgbm": lightgbm_vs_threshold(engine, replay, lstm_model, seq_len=60),
    }
    write_report(report, out_dir)
    return report


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="artifacts")
    ap.add_argument("--cache", default="training/data/cache")
    ap.add_argument("--sample", type=int, default=None)
    ap.add_argument("--epochs", type=int, default=10)
    ap.add_argument("--seed", type=int, default=0)
    args = ap.parse_args()
    rep = run_training(args.out, args.cache, sample_pods=args.sample,
                       lstm_epochs=args.epochs, seed=args.seed)
    print(rep["lightgbm"])


if __name__ == "__main__":
    main()
