# UV Docker container for benchmark testing
FROM python:3.12-slim

# Install UV from official image
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

# Install system dependencies for benchmarking
RUN apt-get update && apt-get install -y \
    curl \
    wget \
    time \
    && rm -rf /var/lib/apt/lists/*

# Create workspace
WORKDIR /workspace

# Copy UV configuration
COPY pyproject.toml ./

# Keep container running for manual testing
CMD ["tail", "-f", "/dev/null"]