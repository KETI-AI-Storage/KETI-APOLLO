"""Offline training pipeline for the APOLLO node-resource-forecaster.

Trains the LSTM-Attention forecaster and the 4 data-supported LightGBM
policy heads on the Alibaba GPU-2023 trace (allocation-occupancy signal).
Not imported by the serving path.
"""
