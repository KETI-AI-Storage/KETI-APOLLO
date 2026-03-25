"""
============================================
Node Resource Forecaster - LSTM Model
노드 리소스 사용량 예측을 위한 LSTM 모델
============================================
"""

import torch
import torch.nn as nn
import numpy as np
from typing import Dict, List, Optional, Tuple
from dataclasses import dataclass, field
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


@dataclass
class ForecastConfig:
    """LSTM Forecaster Configuration"""
    # Model architecture
    input_dim: int = 4  # CPU, Memory, GPU, Storage I/O
    hidden_dim: int = 64
    num_layers: int = 2
    output_dim: int = 4  # Same as input (predicting same metrics)
    dropout: float = 0.2

    # Sequence settings
    sequence_length: int = 60  # 60 time steps (e.g., 60 minutes of data)
    forecast_horizons: List[int] = field(default_factory=lambda: [5, 10, 30, 60])  # minutes

    # Training settings
    learning_rate: float = 0.001
    batch_size: int = 32
    epochs: int = 100

    # Normalization
    normalize: bool = True

    # Device
    device: str = "cuda" if torch.cuda.is_available() else "cpu"


class ResourceEncoder(nn.Module):
    """Encode resource metrics into embeddings"""

    def __init__(self, input_dim: int, hidden_dim: int):
        super().__init__()
        self.encoder = nn.Sequential(
            nn.Linear(input_dim, hidden_dim),
            nn.LayerNorm(hidden_dim),
            nn.ReLU(),
            nn.Linear(hidden_dim, hidden_dim),
            nn.LayerNorm(hidden_dim),
            nn.ReLU()
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.encoder(x)


class LSTMForecaster(nn.Module):
    """LSTM-based Resource Usage Forecaster"""

    def __init__(self, config: ForecastConfig):
        super().__init__()
        self.config = config

        # Resource encoder
        self.resource_encoder = ResourceEncoder(config.input_dim, config.hidden_dim)

        # LSTM layers
        self.lstm = nn.LSTM(
            input_size=config.hidden_dim,
            hidden_size=config.hidden_dim,
            num_layers=config.num_layers,
            batch_first=True,
            dropout=config.dropout if config.num_layers > 1 else 0,
            bidirectional=False
        )

        # Multi-horizon prediction heads
        self.horizon_heads = nn.ModuleDict({
            f"horizon_{h}": nn.Sequential(
                nn.Linear(config.hidden_dim, config.hidden_dim // 2),
                nn.ReLU(),
                nn.Dropout(config.dropout),
                nn.Linear(config.hidden_dim // 2, config.output_dim),
                nn.Sigmoid()  # Output in [0, 1] for utilization
            ) for h in config.forecast_horizons
        })

        # Confidence estimation head
        self.confidence_head = nn.Sequential(
            nn.Linear(config.hidden_dim, config.hidden_dim // 2),
            nn.ReLU(),
            nn.Linear(config.hidden_dim // 2, len(config.forecast_horizons)),
            nn.Sigmoid()
        )

        # Uncertainty estimation (for confidence intervals)
        self.uncertainty_head = nn.Sequential(
            nn.Linear(config.hidden_dim, config.hidden_dim // 2),
            nn.ReLU(),
            nn.Linear(config.hidden_dim // 2, config.output_dim * len(config.forecast_horizons)),
            nn.Softplus()  # Positive uncertainty
        )

    def forward(
        self,
        x: torch.Tensor,
        hidden: Optional[Tuple[torch.Tensor, torch.Tensor]] = None
    ) -> Dict[str, torch.Tensor]:
        """
        Forward pass for resource forecasting.

        Args:
            x: Input tensor of shape (batch, seq_len, input_dim)
            hidden: Optional initial hidden state

        Returns:
            Dictionary containing predictions for each horizon and confidence
        """
        batch_size, seq_len, _ = x.shape

        # Encode resources
        x_encoded = self.resource_encoder(x.view(-1, x.shape[-1]))
        x_encoded = x_encoded.view(batch_size, seq_len, -1)

        # LSTM forward
        if hidden is None:
            lstm_out, (h_n, c_n) = self.lstm(x_encoded)
        else:
            lstm_out, (h_n, c_n) = self.lstm(x_encoded, hidden)

        # Use last hidden state for prediction
        final_hidden = lstm_out[:, -1, :]  # (batch, hidden_dim)

        # Multi-horizon predictions
        predictions = {}
        for horizon in self.config.forecast_horizons:
            head_name = f"horizon_{horizon}"
            predictions[head_name] = self.horizon_heads[head_name](final_hidden)

        # Confidence and uncertainty
        confidence = self.confidence_head(final_hidden)
        uncertainty = self.uncertainty_head(final_hidden)
        uncertainty = uncertainty.view(batch_size, len(self.config.forecast_horizons), -1)

        return {
            'predictions': predictions,
            'confidence': confidence,
            'uncertainty': uncertainty,
            'hidden_state': (h_n, c_n)
        }

    def predict(
        self,
        sequence: torch.Tensor,
        horizons: Optional[List[int]] = None
    ) -> Dict[str, Dict]:
        """
        Make predictions for specific horizons.

        Args:
            sequence: Input sequence tensor (seq_len, input_dim) or (batch, seq_len, input_dim)
            horizons: List of horizons to predict (default: all configured horizons)

        Returns:
            Dictionary with predictions, confidence, and intervals for each horizon
        """
        self.eval()
        horizons = horizons or self.config.forecast_horizons

        # Convert numpy array to tensor if needed
        if isinstance(sequence, np.ndarray):
            sequence = torch.FloatTensor(sequence)

        # Ensure batch dimension
        if sequence.dim() == 2:
            sequence = sequence.unsqueeze(0)

        with torch.no_grad():
            output = self.forward(sequence)

        results = {}
        for i, h in enumerate(self.config.forecast_horizons):
            if h in horizons:
                pred = output['predictions'][f'horizon_{h}'].squeeze().cpu().numpy()
                conf = output['confidence'][:, i].squeeze().cpu().item()
                unc = output['uncertainty'][:, i, :].squeeze().cpu().numpy()

                results[h] = {
                    'predicted_cpu': float(pred[0]),
                    'predicted_memory': float(pred[1]),
                    'predicted_gpu': float(pred[2]),
                    'predicted_storage_io': float(pred[3]),
                    'confidence': float(conf),
                    'lower_bound': (pred - 1.96 * unc).clip(0, 1).tolist(),
                    'upper_bound': (pred + 1.96 * unc).clip(0, 1).tolist()
                }

        return results


class ForecastTrainer:
    """Trainer for LSTM Forecaster"""

    def __init__(self, model: LSTMForecaster, config: ForecastConfig):
        self.model = model.to(config.device)
        self.config = config
        self.device = config.device

        self.optimizer = torch.optim.Adam(
            model.parameters(),
            lr=config.learning_rate
        )
        self.scheduler = torch.optim.lr_scheduler.ReduceLROnPlateau(
            self.optimizer, 'min', patience=5, factor=0.5
        )

        # Loss functions
        self.mse_loss = nn.MSELoss()
        self.mae_loss = nn.L1Loss()

        # Training history
        self.train_losses = []
        self.val_losses = []

    def prepare_sequences(
        self,
        data: np.ndarray,
        horizons: List[int]
    ) -> Tuple[torch.Tensor, Dict[str, torch.Tensor]]:
        """
        Prepare training sequences from time series data.

        Args:
            data: Shape (num_samples, input_dim)
            horizons: List of forecast horizons

        Returns:
            Input sequences and target tensors for each horizon
        """
        seq_len = self.config.sequence_length
        max_horizon = max(horizons)

        X, Y = [], {h: [] for h in horizons}

        for i in range(len(data) - seq_len - max_horizon):
            X.append(data[i:i+seq_len])
            for h in horizons:
                Y[h].append(data[i + seq_len + h - 1])

        X = torch.FloatTensor(np.array(X))
        Y = {h: torch.FloatTensor(np.array(y)) for h, y in Y.items()}

        return X, Y

    def train_epoch(
        self,
        X: torch.Tensor,
        Y: Dict[int, torch.Tensor]
    ) -> float:
        """Train one epoch"""
        self.model.train()

        # Create batches
        n_samples = X.shape[0]
        indices = torch.randperm(n_samples)

        total_loss = 0.0
        n_batches = 0

        for i in range(0, n_samples, self.config.batch_size):
            batch_idx = indices[i:i+self.config.batch_size]
            batch_x = X[batch_idx].to(self.device)

            self.optimizer.zero_grad()

            output = self.model(batch_x)

            # Compute loss for all horizons
            loss = 0.0
            for h in self.config.forecast_horizons:
                batch_y = Y[h][batch_idx].to(self.device)
                pred = output['predictions'][f'horizon_{h}']
                loss += self.mse_loss(pred, batch_y)

            loss /= len(self.config.forecast_horizons)

            loss.backward()
            torch.nn.utils.clip_grad_norm_(self.model.parameters(), 1.0)
            self.optimizer.step()

            total_loss += loss.item()
            n_batches += 1

        return total_loss / n_batches

    def validate(
        self,
        X: torch.Tensor,
        Y: Dict[int, torch.Tensor]
    ) -> float:
        """Validate model"""
        self.model.eval()

        with torch.no_grad():
            X = X.to(self.device)
            output = self.model(X)

            loss = 0.0
            for h in self.config.forecast_horizons:
                batch_y = Y[h].to(self.device)
                pred = output['predictions'][f'horizon_{h}']
                loss += self.mse_loss(pred, batch_y)

            loss /= len(self.config.forecast_horizons)

        return loss.item()

    def train(
        self,
        train_data: np.ndarray,
        val_data: Optional[np.ndarray] = None,
        epochs: Optional[int] = None
    ) -> Dict[str, List[float]]:
        """
        Train the forecaster.

        Args:
            train_data: Training data of shape (num_samples, input_dim)
            val_data: Optional validation data
            epochs: Number of epochs (default: config.epochs)

        Returns:
            Training history
        """
        epochs = epochs or self.config.epochs

        # Prepare training data
        X_train, Y_train = self.prepare_sequences(
            train_data, self.config.forecast_horizons
        )

        # Prepare validation data
        if val_data is not None:
            X_val, Y_val = self.prepare_sequences(
                val_data, self.config.forecast_horizons
            )

        logger.info(f"Training with {X_train.shape[0]} sequences")

        for epoch in range(epochs):
            train_loss = self.train_epoch(X_train, Y_train)
            self.train_losses.append(train_loss)

            if val_data is not None:
                val_loss = self.validate(X_val, Y_val)
                self.val_losses.append(val_loss)
                self.scheduler.step(val_loss)

                logger.info(f"Epoch {epoch+1}/{epochs} - "
                           f"Train Loss: {train_loss:.6f}, Val Loss: {val_loss:.6f}")
            else:
                self.scheduler.step(train_loss)
                logger.info(f"Epoch {epoch+1}/{epochs} - Train Loss: {train_loss:.6f}")

        return {
            'train_losses': self.train_losses,
            'val_losses': self.val_losses
        }

    def save(self, path: str):
        """Save model checkpoint"""
        torch.save({
            'model_state_dict': self.model.state_dict(),
            'optimizer_state_dict': self.optimizer.state_dict(),
            'config': self.config,
            'train_losses': self.train_losses,
            'val_losses': self.val_losses
        }, path)
        logger.info(f"Model saved to {path}")

    def load(self, path: str):
        """Load model checkpoint"""
        checkpoint = torch.load(path, map_location=self.device)
        self.model.load_state_dict(checkpoint['model_state_dict'])
        self.optimizer.load_state_dict(checkpoint['optimizer_state_dict'])
        self.train_losses = checkpoint.get('train_losses', [])
        self.val_losses = checkpoint.get('val_losses', [])
        logger.info(f"Model loaded from {path}")


class PeakIdlePredictor:
    """Predicts peak and idle periods based on forecast"""

    def __init__(self, forecaster: LSTMForecaster, threshold_high: float = 0.75, threshold_low: float = 0.25):
        self.forecaster = forecaster
        self.threshold_high = threshold_high
        self.threshold_low = threshold_low

    def predict_periods(
        self,
        historical_data: np.ndarray,
        lookahead_steps: int = 60
    ) -> Dict[str, List[Dict]]:
        """
        Predict peak and idle periods.

        Args:
            historical_data: Historical resource usage (seq_len, 4)
            lookahead_steps: Number of steps to look ahead

        Returns:
            Dictionary with peak_periods, idle_periods, and recommended_batch_window
        """
        self.forecaster.eval()

        # Convert to tensor
        seq = torch.FloatTensor(historical_data).unsqueeze(0)

        # Get predictions for all horizons
        with torch.no_grad():
            predictions = self.forecaster.predict(seq)

        peak_periods = []
        idle_periods = []

        for horizon, pred in predictions.items():
            avg_util = (pred['predicted_cpu'] + pred['predicted_memory'] +
                       pred['predicted_gpu']) / 3

            if avg_util > self.threshold_high:
                peak_periods.append({
                    'horizon_minutes': horizon,
                    'avg_utilization': avg_util,
                    'confidence': pred['confidence']
                })
            elif avg_util < self.threshold_low:
                idle_periods.append({
                    'horizon_minutes': horizon,
                    'avg_utilization': avg_util,
                    'confidence': pred['confidence']
                })

        # Find best batch window (lowest utilization period)
        recommended_window = None
        if idle_periods:
            best_idle = min(idle_periods, key=lambda x: x['avg_utilization'])
            recommended_window = {
                'start_offset_minutes': best_idle['horizon_minutes'] - 5,
                'end_offset_minutes': best_idle['horizon_minutes'] + 5,
                'expected_utilization': best_idle['avg_utilization']
            }

        return {
            'peak_periods': peak_periods,
            'idle_periods': idle_periods,
            'recommended_batch_window': recommended_window
        }


# Factory function
def create_forecaster(config: Optional[ForecastConfig] = None) -> Tuple[LSTMForecaster, ForecastTrainer]:
    """Create forecaster model and trainer"""
    config = config or ForecastConfig()
    model = LSTMForecaster(config)
    trainer = ForecastTrainer(model, config)
    return model, trainer
