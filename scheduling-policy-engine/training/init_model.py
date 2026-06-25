"""Analytic uniform initialization for the PPO scheduling actor.

Produces a DETERMINISTIC policy whose output is uniform plugin weights
([0.2]*5) for ANY input, by zeroing the actor's policy head (and the critic
head). The uniform output is exact by construction:

    logits = policy_head.weight @ hidden + policy_head.bias
    with weight=0 and bias=0  ->  logits = 0  ->  softmax(logits) = uniform

This replaces random-initialized weights (which are worse than no-op when fed
to the live scheduler). It is NOT reinforcement learning and claims no
performance superiority. State-conditioned policy learning is a follow-up.
"""
import argparse
import os
from typing import Optional, Tuple

import torch

from models.ppo import SchedulingPolicyPPO, PPOConfig, PPOTrainer

# ClusterEncoder default capacity (SchedulingPolicyPPO -> ClusterEncoder(max_nodes=32)).
_MAX_NODES = 32


def build_uniform_model(config: Optional[PPOConfig] = None) -> SchedulingPolicyPPO:
    """Instantiate the PPO model and force exactly-uniform, deterministic output."""
    config = config or PPOConfig()
    model = SchedulingPolicyPPO(config)
    with torch.no_grad():
        # Actor: zeroing the final head makes logits input-independent (==bias==0)
        # -> softmax is uniform for every input.
        torch.nn.init.zeros_(model.actor.policy_head.weight)
        torch.nn.init.zeros_(model.actor.policy_head.bias)
        # Critic: zero the final layer -> deterministic value 0 (confidence is
        # not meaningful; flagged in the server status line).
        torch.nn.init.zeros_(model.critic.network[-1].weight)
        torch.nn.init.zeros_(model.critic.network[-1].bias)
    return model


def random_state(batch: int = 1,
                 gen: Optional[torch.Generator] = None) -> Tuple[dict, dict, dict]:
    """Build a random but structurally-valid (workload, pipeline, cluster) input
    triple matching the encoders' expected tensor shapes/dtypes."""
    def r(*shape):
        return torch.rand(*shape, generator=gen)

    workload = {
        "workload_type": torch.randint(0, 10, (batch,), generator=gen),
        "preprocessing_type": torch.randint(0, 10, (batch,), generator=gen),
        "io_pattern": r(batch, 7),
        "compute_profile": r(batch, 6),
        "flags": r(batch, 3),
    }
    pipeline = {
        "position": r(batch, 3),
        "dag_structure": r(batch, 4),
        "locality": r(batch, 2),
    }
    cluster = {
        "nodes": r(batch, _MAX_NODES, 10),
        "node_mask": torch.ones(batch, _MAX_NODES, dtype=torch.bool),
        "cluster_metrics": r(batch, 8),
        "queue_status": r(batch, 4),
    }
    return workload, pipeline, cluster


def verify_uniform(model: SchedulingPolicyPPO,
                   num_samples: int = 64,
                   tol: float = 1e-5) -> bool:
    """Assert the model outputs uniform weights across many varied inputs."""
    model.eval()
    gen = torch.Generator().manual_seed(0)
    n_plugins = model.config.num_plugins
    for _ in range(num_samples):
        w, p, c = random_state(batch=4, gen=gen)
        with torch.no_grad():
            policy, _ = model.forward(w, p, c)
        expected = torch.full_like(policy, 1.0 / n_plugins)
        max_dev = (policy - expected).abs().max().item()
        if max_dev > tol:
            raise AssertionError(f"non-uniform output: max deviation {max_dev} > {tol}")
    return True


def save_uniform_model(out_path: str,
                       config: Optional[PPOConfig] = None) -> str:
    """Build, self-verify, save (PPOTrainer format), then reload-verify."""
    config = config or PPOConfig()
    model = build_uniform_model(config)
    verify_uniform(model)

    out_dir = os.path.dirname(out_path)
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    PPOTrainer(model, config).save(out_path)

    # Build-time guard: the artifact must load into a fresh, server-shaped model
    # and still be uniform -> guarantees load-compatibility with PPOConfig.
    fresh = SchedulingPolicyPPO(config)
    PPOTrainer(fresh, config).load(out_path)
    verify_uniform(fresh)
    return out_path


def main() -> None:
    parser = argparse.ArgumentParser(description="Build the PPO uniform-init artifact.")
    parser.add_argument("--out", default="/policy-models/ppo_scheduler.pt")
    parser.add_argument("--seed", type=int, default=0)
    args = parser.parse_args()
    torch.manual_seed(args.seed)
    path = save_uniform_model(args.out)
    print(f"[apollo-ml] PPO uniform-init model written to {path}")


if __name__ == "__main__":
    main()
