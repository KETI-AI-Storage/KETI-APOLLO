# Node Resource Forecaster Dockerfile
# 발표자료 기반: LSTM-Attention + Multi-LightGBM
# CPU-only 경량화

FROM python:3.11-slim

WORKDIR /app

# Install system dependencies (LightGBM requires libgomp)
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Install PyTorch CPU-only version separately (약 200MB)
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu

# Install LightGBM separately (better error handling)
RUN pip install --no-cache-dir lightgbm>=4.0.0

# Copy and install other requirements
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt || true

# Copy proto files and generate stubs
COPY proto/ ./proto/
RUN python -m grpc_tools.protoc \
    -I./proto \
    --python_out=./proto \
    --grpc_python_out=./proto \
    ./proto/forecaster.proto \
    ./proto/hub.proto

# Copy source code
COPY models/ ./models/
COPY server/ ./server/

# Create directories
RUN mkdir -p /models /data /policy-models

# Environment variables
ENV PYTHONUNBUFFERED=1
ENV GRPC_PORT=50055
ENV HTTP_PORT=8080
ENV MODEL_PATH=/models/lstm_attention.pt
ENV POLICY_MODEL_DIR=/policy-models
ENV LOG_LEVEL=INFO

EXPOSE 50055
EXPOSE 8080
EXPOSE 9091

# Support both model types
# --model: LSTM/LSTM-Attention checkpoint
# --policy-models: Directory with LightGBM policy models
CMD ["python", "-m", "server.grpc_server", "--port", "50055", "--http-port", "8080"]
