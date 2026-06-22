"""
============================================
LSTM-Attention 기반 Node Resource Forecaster
============================================

발표자료 기반 구현:
- 시계열 예측 모델 (LSTM-Attention)
- 과거 60분 데이터 → 15, 30, 60, 120분 후 예측
- Attention 메커니즘으로 시점별 중요도 반영
- 예측 정확도 목표: 90%+

입력 메트릭 (Alibaba Cluster Trace + KETI 추가):
- cpu_util, memory_util, net_in, net_out (기본)
- available_cores, load_average, workload_phase (KETI)
- total_iops, throughput, cache_hit_ratio (3차년도 스토리지)
"""

import torch
import torch.nn as nn
import torch.nn.functional as F
import numpy as np
from typing import Dict, List, Optional, Tuple
from dataclasses import dataclass, field
import logging
import math

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


@dataclass
class LSTMAttentionConfig:
    """LSTM-Attention 설정"""
    # 입력 메트릭 수
    input_dim: int = 12  # 12개 메트릭

    # 모델 구조
    hidden_dim: int = 96
    num_layers: int = 2
    num_heads: int = 4  # Multi-head attention
    dropout: float = 0.2

    # 시퀀스 설정
    sequence_length: int = 60  # 과거 60분 (1분 단위)
    forecast_horizons: List[int] = field(default_factory=lambda: [15, 30, 60, 120])

    # 출력 메트릭 수 (예측 대상)
    output_dim: int = 4  # cpu, memory, gpu, pending_pods

    # 학습 설정
    learning_rate: float = 0.001
    batch_size: int = 32

    device: str = "cuda" if torch.cuda.is_available() else "cpu"


class PositionalEncoding(nn.Module):
    """시퀀스 위치 인코딩"""

    def __init__(self, d_model: int, max_len: int = 120, dropout: float = 0.1):
        super().__init__()
        self.dropout = nn.Dropout(p=dropout)

        position = torch.arange(max_len).unsqueeze(1)
        div_term = torch.exp(torch.arange(0, d_model, 2) * (-math.log(10000.0) / d_model))

        pe = torch.zeros(1, max_len, d_model)
        pe[0, :, 0::2] = torch.sin(position * div_term)
        pe[0, :, 1::2] = torch.cos(position * div_term)
        self.register_buffer('pe', pe)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        """
        Args:
            x: (batch, seq_len, d_model)
        """
        x = x + self.pe[:, :x.size(1), :]
        return self.dropout(x)


class TemporalAttention(nn.Module):
    """
    Temporal Attention Layer

    시계열 데이터에서 중요한 시점에 더 높은 가중치 부여
    """

    def __init__(self, hidden_dim: int, num_heads: int = 4, dropout: float = 0.1):
        super().__init__()
        self.num_heads = num_heads
        self.head_dim = hidden_dim // num_heads
        assert self.head_dim * num_heads == hidden_dim, "hidden_dim must be divisible by num_heads"

        self.query = nn.Linear(hidden_dim, hidden_dim)
        self.key = nn.Linear(hidden_dim, hidden_dim)
        self.value = nn.Linear(hidden_dim, hidden_dim)
        self.out_proj = nn.Linear(hidden_dim, hidden_dim)

        self.dropout = nn.Dropout(dropout)
        self.scale = math.sqrt(self.head_dim)

    def forward(self, x: torch.Tensor, mask: Optional[torch.Tensor] = None) -> Tuple[torch.Tensor, torch.Tensor]:
        """
        Args:
            x: (batch, seq_len, hidden_dim)
            mask: optional attention mask

        Returns:
            output: (batch, seq_len, hidden_dim)
            attention_weights: (batch, num_heads, seq_len, seq_len)
        """
        batch_size, seq_len, _ = x.shape

        # Q, K, V 계산
        Q = self.query(x).view(batch_size, seq_len, self.num_heads, self.head_dim).transpose(1, 2)
        K = self.key(x).view(batch_size, seq_len, self.num_heads, self.head_dim).transpose(1, 2)
        V = self.value(x).view(batch_size, seq_len, self.num_heads, self.head_dim).transpose(1, 2)

        # Attention scores
        scores = torch.matmul(Q, K.transpose(-2, -1)) / self.scale

        if mask is not None:
            scores = scores.masked_fill(mask == 0, float('-inf'))

        attention_weights = F.softmax(scores, dim=-1)
        attention_weights = self.dropout(attention_weights)

        # Apply attention to values
        context = torch.matmul(attention_weights, V)
        context = context.transpose(1, 2).contiguous().view(batch_size, seq_len, -1)

        output = self.out_proj(context)

        return output, attention_weights


class LSTMAttentionForecaster(nn.Module):
    """
    LSTM-Attention 기반 자원 예측 모델

    Architecture:
    1. Input Layer: 12개 메트릭 임베딩
    2. LSTM Layers: 시계열 패턴 학습
    3. Temporal Attention: 중요 시점 강조
    4. Multi-horizon Heads: 15, 30, 60, 120분 예측
    """

    def __init__(self, config: LSTMAttentionConfig):
        super().__init__()
        self.config = config

        # Input embedding
        self.input_proj = nn.Sequential(
            nn.Linear(config.input_dim, config.hidden_dim),
            nn.LayerNorm(config.hidden_dim),
            nn.ReLU(),
            nn.Dropout(config.dropout)
        )

        # Positional encoding
        self.pos_encoding = PositionalEncoding(
            config.hidden_dim,
            max_len=config.sequence_length + 10,
            dropout=config.dropout
        )

        # Bidirectional LSTM
        self.lstm = nn.LSTM(
            input_size=config.hidden_dim,
            hidden_size=config.hidden_dim // 2,  # bidirectional doubles this
            num_layers=config.num_layers,
            batch_first=True,
            dropout=config.dropout if config.num_layers > 1 else 0,
            bidirectional=True
        )

        # Temporal Attention
        self.attention = TemporalAttention(
            hidden_dim=config.hidden_dim,
            num_heads=config.num_heads,
            dropout=config.dropout
        )

        # Layer normalization
        self.layer_norm = nn.LayerNorm(config.hidden_dim)

        # Global attention pooling (sequence → single vector)
        self.global_attention = nn.Sequential(
            nn.Linear(config.hidden_dim, config.hidden_dim // 2),
            nn.Tanh(),
            nn.Linear(config.hidden_dim // 2, 1)
        )

        # Multi-horizon prediction heads
        self.horizon_heads = nn.ModuleDict({
            f"horizon_{h}": nn.Sequential(
                nn.Linear(config.hidden_dim, config.hidden_dim),
                nn.ReLU(),
                nn.Dropout(config.dropout),
                nn.Linear(config.hidden_dim, config.hidden_dim // 2),
                nn.ReLU(),
                nn.Linear(config.hidden_dim // 2, config.output_dim),
                nn.Sigmoid()  # 0-1 범위 (utilization)
            ) for h in config.forecast_horizons
        })

        # Confidence estimation
        self.confidence_head = nn.Sequential(
            nn.Linear(config.hidden_dim, config.hidden_dim // 2),
            nn.ReLU(),
            nn.Linear(config.hidden_dim // 2, len(config.forecast_horizons)),
            nn.Sigmoid()
        )

        # Uncertainty estimation (aleatoric)
        self.uncertainty_head = nn.Sequential(
            nn.Linear(config.hidden_dim, config.hidden_dim // 2),
            nn.ReLU(),
            nn.Linear(config.hidden_dim // 2, config.output_dim * len(config.forecast_horizons)),
            nn.Softplus()
        )

    def forward(self, x: torch.Tensor) -> Dict[str, torch.Tensor]:
        """
        Forward pass

        Args:
            x: (batch, seq_len, input_dim) - 시계열 메트릭 데이터

        Returns:
            Dict with predictions, confidence, uncertainty, attention_weights
        """
        batch_size, seq_len, _ = x.shape

        # 1. Input projection
        x = self.input_proj(x)

        # 2. Add positional encoding
        x = self.pos_encoding(x)

        # 3. LSTM encoding
        lstm_out, _ = self.lstm(x)

        # 4. Temporal attention
        attended, attention_weights = self.attention(lstm_out)

        # 5. Residual connection + layer norm
        x = self.layer_norm(lstm_out + attended)

        # 6. Global attention pooling
        attn_scores = self.global_attention(x)  # (batch, seq_len, 1)
        attn_scores = F.softmax(attn_scores, dim=1)
        context = torch.sum(x * attn_scores, dim=1)  # (batch, hidden_dim)

        # 7. Multi-horizon predictions
        predictions = {}
        for h in self.config.forecast_horizons:
            predictions[f"horizon_{h}"] = self.horizon_heads[f"horizon_{h}"](context)

        # 8. Confidence and uncertainty
        confidence = self.confidence_head(context)
        uncertainty = self.uncertainty_head(context)
        uncertainty = uncertainty.view(batch_size, len(self.config.forecast_horizons), -1)

        return {
            'predictions': predictions,
            'confidence': confidence,
            'uncertainty': uncertainty,
            'attention_weights': attention_weights,
            'context': context
        }

    def predict(self, sequence: torch.Tensor) -> Dict[int, Dict]:
        """
        단일 시퀀스 예측

        Args:
            sequence: (seq_len, input_dim) or (batch, seq_len, input_dim)

        Returns:
            각 horizon별 예측 결과
        """
        self.eval()

        if isinstance(sequence, np.ndarray):
            sequence = torch.FloatTensor(sequence)

        if sequence.dim() == 2:
            sequence = sequence.unsqueeze(0)

        sequence = sequence.to(next(self.parameters()).device)

        with torch.no_grad():
            output = self.forward(sequence)

        results = {}
        for i, h in enumerate(self.config.forecast_horizons):
            pred = output['predictions'][f'horizon_{h}'].squeeze().cpu().numpy()
            conf = output['confidence'][:, i].squeeze().cpu().item()
            unc = output['uncertainty'][:, i, :].squeeze().cpu().numpy()

            results[h] = {
                'predicted_cpu': float(pred[0]) if len(pred) > 0 else 0.5,
                'predicted_memory': float(pred[1]) if len(pred) > 1 else 0.5,
                'predicted_gpu': float(pred[2]) if len(pred) > 2 else 0.0,
                'predicted_pending_pods': float(pred[3]) if len(pred) > 3 else 0.0,
                'confidence': float(conf),
                'uncertainty': unc.tolist() if hasattr(unc, 'tolist') else [0.1] * 4,
            }

        return results


class LSTMAttentionTrainer:
    """LSTM-Attention 학습 관리자"""

    def __init__(self, model: LSTMAttentionForecaster, config: LSTMAttentionConfig):
        self.model = model.to(config.device)
        self.config = config
        self.device = config.device

        self.optimizer = torch.optim.AdamW(
            model.parameters(),
            lr=config.learning_rate,
            weight_decay=0.01
        )

        self.scheduler = torch.optim.lr_scheduler.CosineAnnealingWarmRestarts(
            self.optimizer, T_0=10, T_mult=2
        )

        self.mse_loss = nn.MSELoss()
        self.train_losses = []
        self.val_losses = []

    def train_epoch(self, train_loader) -> float:
        """한 에폭 학습"""
        self.model.train()
        total_loss = 0.0
        n_batches = 0

        for batch_x, batch_y in train_loader:
            batch_x = batch_x.to(self.device)

            self.optimizer.zero_grad()
            output = self.model(batch_x)

            # Multi-horizon loss
            loss = 0.0
            for i, h in enumerate(self.config.forecast_horizons):
                target = batch_y[h].to(self.device)
                pred = output['predictions'][f'horizon_{h}']
                loss += self.mse_loss(pred, target)

            loss /= len(self.config.forecast_horizons)

            loss.backward()
            torch.nn.utils.clip_grad_norm_(self.model.parameters(), 1.0)
            self.optimizer.step()

            total_loss += loss.item()
            n_batches += 1

        self.scheduler.step()
        return total_loss / max(n_batches, 1)

    def save(self, path: str):
        """모델 저장"""
        torch.save({
            'model_state_dict': self.model.state_dict(),
            'optimizer_state_dict': self.optimizer.state_dict(),
            'config': self.config,
        }, path)
        logger.info(f"LSTM-Attention model saved to {path}")

    def load(self, path: str):
        """모델 로드"""
        checkpoint = torch.load(path, map_location=self.device)
        self.model.load_state_dict(checkpoint['model_state_dict'])
        self.optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
        logger.info(f"LSTM-Attention model loaded from {path}")


def create_lstm_attention_forecaster(config: Optional[LSTMAttentionConfig] = None):
    """LSTM-Attention 모델 생성"""
    config = config or LSTMAttentionConfig()
    model = LSTMAttentionForecaster(config)
    trainer = LSTMAttentionTrainer(model, config)
    return model, trainer
