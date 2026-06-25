"""Pure helpers for the scheduling-policy-engine model lifecycle.

Kept dependency-light (stdlib only) so they are unit-testable without importing
the gRPC server (which pulls generated protos + torch).
"""
import os
from typing import Mapping, Optional

DEFAULT_MODEL_PATH = "/policy-models/ppo_scheduler.pt"
ONLINE_RL_ENV = "APOLLO_PPO_ONLINE_RL"
_TRUTHY = ("1", "true", "yes", "on")


def resolve_model_path(arg_path: Optional[str] = None,
                       env: Optional[Mapping] = None) -> str:
    """CLI arg wins; else MODEL_PATH env; else the baked default."""
    env = os.environ if env is None else env
    return arg_path or env.get("MODEL_PATH", DEFAULT_MODEL_PATH)


def online_rl_enabled(env: Optional[Mapping] = None) -> bool:
    """Online PPO updates are FROZEN unless APOLLO_PPO_ONLINE_RL is truthy."""
    env = os.environ if env is None else env
    return env.get(ONLINE_RL_ENV, "").strip().lower() in _TRUTHY


def policy_status(model_loaded: bool, online_rl: bool) -> str:
    """One honest startup line; no RL/benchmark superiority is claimed."""
    if model_loaded:
        head = "PPO actor=uniform-init (deterministic [0.2]x5)"
    else:
        head = "PPO actor=!!FELL BACK TO RANDOM (model missing/load failed)!!"
    rl = "online-RL=ON" if online_rl else "online-RL=frozen"
    return f"[apollo-ml] {head}; {rl}; critic=untrained (confidence not meaningful)"
