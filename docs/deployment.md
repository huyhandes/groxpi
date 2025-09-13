# Deployment

Groxpi supports multiple deployment methods from local development to production-ready containerized environments.

## Development Deployment

### Local Development
Run groxpi directly on your development machine for testing and development.

#### Prerequisites
- Go 1.24+
- Git

#### Quick Start
```bash
# Clone the repository
git clone https://github.com/yourusername/groxpi.git
cd groxpi

# Install dependencies
go mod download

# Run locally with default configuration
go run cmd/groxpi/main.go
```

#### Custom Configuration
```bash
# Set environment variables
export GROXPI_INDEX_URL="https://pypi.org/simple/"
export GROXPI_CACHE_SIZE=1073741824  # 1GB
export GROXPI_CACHE_DIR="./cache"
export GROXPI_LOGGING_LEVEL=DEBUG

# Run with custom config
go run cmd/groxpi/main.go
```

#### Build Binary
```bash
# Build optimized binary
go build -ldflags="-s -w" -o groxpi cmd/groxpi/main.go

# Run the binary
./groxpi
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run benchmarks
go test -bench=. ./...

# Run integration tests
go test -run Integration ./...
```

## Container Deployment

### Docker

#### Build Image
```bash
# Build Docker image
docker build -t groxpi:latest .

# Build for specific architecture
docker buildx build --platform linux/amd64,linux/arm64 -t groxpi:latest .
```

#### Run Container
```bash
# Basic run with default configuration
docker run -p 5000:5000 groxpi:latest

# Run with custom environment
docker run -p 5000:5000 \
  -e GROXPI_INDEX_URL=https://pypi.org/simple/ \
  -e GROXPI_CACHE_SIZE=5368709120 \
  -e GROXPI_LOGGING_LEVEL=INFO \
  groxpi:latest

# Run with volume for persistent cache
docker run -p 5000:5000 \
  -v groxpi-cache:/cache \
  -e GROXPI_CACHE_DIR=/cache \
  groxpi:latest
```

#### Docker Health Checks
Built-in health check configuration:
```dockerfile
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:5000/health || exit 1
```

### Docker Compose

#### Basic Setup
```yaml
# docker-compose.yml
version: "3.8"

services:
  groxpi:
    build:
      context: .
      dockerfile: Dockerfile
    image: groxpi:latest
    container_name: groxpi
    restart: unless-stopped
    ports:
      - "5000:5000"
    environment:
      - GROXPI_INDEX_URL=https://pypi.org/simple/
      - GROXPI_CACHE_SIZE=5368709120  # 5GB
      - GROXPI_INDEX_TTL=1800         # 30 minutes
      - GROXPI_CACHE_DIR=/cache
      - GROXPI_LOGGING_LEVEL=INFO
    volumes:
      - groxpi_cache:/cache
    healthcheck:
      test: ["CMD-SHELL", "wget --no-verbose --tries=1 --spider http://localhost:5000/health || exit 1"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 40s

volumes:
  groxpi_cache:
    driver: local
```

#### Run with Docker Compose
```bash
# Start services
docker-compose up -d

# View logs
docker-compose logs -f groxpi

# Stop services
docker-compose down

# Rebuild and restart
docker-compose up -d --build
```

### S3 Storage Configuration

#### MinIO Setup
```yaml
# docker-compose.minio.yml
version: "3.8"

services:
  groxpi:
    build: .
    ports:
      - "5000:5000"
    environment:
      - GROXPI_STORAGE_TYPE=s3
      - AWS_ENDPOINT_URL=http://minio:9000
      - AWS_ACCESS_KEY_ID=minioadmin
      - AWS_SECRET_ACCESS_KEY=minioadmin
      - AWS_REGION=us-east-1
      - GROXPI_S3_BUCKET=groxpi
      - GROXPI_S3_USE_SSL=false
      - GROXPI_S3_FORCE_PATH_STYLE=true
    depends_on:
      - minio

  minio:
    image: minio/minio:latest
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      - MINIO_ACCESS_KEY=minioadmin
      - MINIO_SECRET_KEY=minioadmin
    volumes:
      - minio_data:/data
    command: server /data --console-address ":9001"

volumes:
  minio_data:
```

Run with MinIO:
```bash
docker-compose -f docker-compose.minio.yml up -d
```

#### AWS S3 Configuration
```yaml
services:
  groxpi:
    # ... other config
    environment:
      - GROXPI_STORAGE_TYPE=s3
      - AWS_ACCESS_KEY_ID=your_access_key
      - AWS_SECRET_ACCESS_KEY=your_secret_key
      - AWS_REGION=us-west-2
      - GROXPI_S3_BUCKET=your-groxpi-bucket
      - GROXPI_S3_PREFIX=packages/
      - GROXPI_S3_USE_SSL=true
      - GROXPI_S3_FORCE_PATH_STYLE=false
```

## Production Deployment

### Production Configuration

#### Environment Variables
```bash
# Core configuration
export GROXPI_INDEX_URL="https://pypi.org/simple/"
export GROXPI_INDEX_TTL=1800
export GROXPI_CACHE_SIZE=10737418240  # 10GB
export GROXPI_LOGGING_LEVEL=INFO

# Performance tuning
export GROXPI_RESPONSE_CACHE_TTL=300
export GROXPI_MAX_CONCURRENT_DOWNLOADS=20
export GROXPI_DOWNLOAD_TIMEOUT=30

# Security
export GROXPI_DISABLE_INDEX_SSL_VERIFICATION=false

# Storage (choose one)
# Local storage
export GROXPI_STORAGE_TYPE=local
export GROXPI_CACHE_DIR=/app/cache

# S3 storage
export GROXPI_STORAGE_TYPE=s3
export AWS_ENDPOINT_URL=https://s3.amazonaws.com
export AWS_ACCESS_KEY_ID=your_access_key
export AWS_SECRET_ACCESS_KEY=your_secret_key
export AWS_REGION=us-west-2
export GROXPI_S3_BUCKET=your-production-bucket
```

#### Production Docker Compose
```yaml
version: "3.8"

services:
  groxpi:
    image: groxpi:latest
    restart: unless-stopped
    ports:
      - "5000:5000"
    environment:
      - GROXPI_INDEX_URL=https://pypi.org/simple/
      - GROXPI_CACHE_SIZE=10737418240
      - GROXPI_INDEX_TTL=1800
      - GROXPI_LOGGING_LEVEL=INFO
      - GROXPI_STORAGE_TYPE=s3
      - AWS_ENDPOINT_URL=${AWS_ENDPOINT_URL}
      - AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}
      - AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}
      - AWS_REGION=${AWS_REGION}
      - GROXPI_S3_BUCKET=${GROXPI_S3_BUCKET}
    healthcheck:
      test: ["CMD-SHELL", "wget --no-verbose --tries=1 --spider http://localhost:5000/health || exit 1"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 40s
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.groxpi.rule=Host(`pypi.yourdomain.com`)"
      - "traefik.http.services.groxpi.loadbalancer.server.port=5000"
    networks:
      - proxy

networks:
  proxy:
    external: true
```

### Reverse Proxy Configuration

#### Nginx
```nginx
upstream groxpi {
    server 127.0.0.1:5000;
    # Add more servers for load balancing
    # server 127.0.0.1:5001;
    # server 127.0.0.1:5002;
}

server {
    listen 80;
    server_name pypi.yourdomain.com;
    
    # Redirect to HTTPS
    return 301 https://$server_name$request_uri;
}

server {
    listen 443 ssl http2;
    server_name pypi.yourdomain.com;
    
    # SSL configuration
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    
    # Security headers
    add_header X-Content-Type-Options nosniff;
    add_header X-Frame-Options DENY;
    add_header X-XSS-Protection "1; mode=block";
    
    # Proxy configuration
    location / {
        proxy_pass http://groxpi;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        
        # Increase timeouts for large file downloads
        proxy_connect_timeout 60s;
        proxy_send_timeout 60s;
        proxy_read_timeout 60s;
        
        # Enable compression
        gzip on;
        gzip_types application/json text/html;
    }
    
    # Health check endpoint (optional basic auth)
    location /health {
        proxy_pass http://groxpi;
        # auth_basic "Health Check";
        # auth_basic_user_file /path/to/.htpasswd;
    }
}
```

#### Traefik
```yaml
# traefik.yml
version: "3.8"

services:
  traefik:
    image: traefik:latest
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - "/var/run/docker.sock:/var/run/docker.sock:ro"
      - "./traefik.yml:/traefik.yml:ro"
      - "./certs:/certs"

  groxpi:
    image: groxpi:latest
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.groxpi.rule=Host(`pypi.yourdomain.com`)"
      - "traefik.http.routers.groxpi.tls=true"
      - "traefik.http.routers.groxpi.tls.certresolver=letsencrypt"
      - "traefik.http.services.groxpi.loadbalancer.server.port=5000"
```

## Kubernetes Deployment

### Basic Kubernetes Manifests

#### Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: groxpi
  labels:
    app: groxpi
spec:
  replicas: 3
  selector:
    matchLabels:
      app: groxpi
  template:
    metadata:
      labels:
        app: groxpi
    spec:
      containers:
      - name: groxpi
        image: groxpi:latest
        ports:
        - containerPort: 5000
        env:
        - name: GROXPI_INDEX_URL
          value: "https://pypi.org/simple/"
        - name: GROXPI_CACHE_SIZE
          value: "10737418240"
        - name: GROXPI_STORAGE_TYPE
          value: "s3"
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: groxpi-secrets
              key: aws-access-key-id
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: groxpi-secrets
              key: aws-secret-access-key
        - name: GROXPI_S3_BUCKET
          value: "groxpi-prod"
        livenessProbe:
          httpGet:
            path: /health
            port: 5000
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 5000
          initialDelaySeconds: 5
          periodSeconds: 5
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
```

#### Service
```yaml
apiVersion: v1
kind: Service
metadata:
  name: groxpi-service
spec:
  selector:
    app: groxpi
  ports:
  - protocol: TCP
    port: 80
    targetPort: 5000
  type: ClusterIP
```

#### Ingress
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: groxpi-ingress
  annotations:
    kubernetes.io/ingress.class: "nginx"
    cert-manager.io/cluster-issuer: "letsencrypt-prod"
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
spec:
  tls:
  - hosts:
    - pypi.yourdomain.com
    secretName: groxpi-tls
  rules:
  - host: pypi.yourdomain.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: groxpi-service
            port:
              number: 80
```

#### ConfigMap & Secret
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: groxpi-secrets
type: Opaque
stringData:
  aws-access-key-id: your_access_key
  aws-secret-access-key: your_secret_key
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: groxpi-config
data:
  GROXPI_INDEX_URL: "https://pypi.org/simple/"
  GROXPI_CACHE_SIZE: "10737418240"
  GROXPI_INDEX_TTL: "1800"
  GROXPI_LOGGING_LEVEL: "INFO"
```

### Helm Chart (Structure)
```
helm/
├── Chart.yaml
├── values.yaml
├── templates/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── ingress.yaml
│   ├── configmap.yaml
│   ├── secret.yaml
│   └── hpa.yaml
└── values-production.yaml
```

## Monitoring in Production

### Health Checks
- **Endpoint**: `/health`
- **Response**: JSON with system status
- **Use**: Load balancer health checks

### Logging
- **Format**: Structured JSON logs
- **Level**: INFO for production
- **Output**: stdout/stderr for container logging

### Metrics Collection (Planned)
- **Endpoint**: `/metrics`
- **Format**: Prometheus exposition format
- **Integration**: Prometheus + Grafana

## Security Considerations

### Network Security
- Use HTTPS in production
- Implement proper firewall rules
- Consider VPN for internal deployments

### Container Security
- Use non-root user in containers
- Scan images for vulnerabilities
- Keep base images updated

### Access Control
- Implement authentication if needed
- Use RBAC in Kubernetes
- Secure S3 bucket access

### Secrets Management
- Use environment variables for configuration
- Store secrets in secure secret stores
- Rotate credentials regularly

## Backup and Disaster Recovery

### Cache Backup
- S3 storage provides built-in durability
- Local storage requires backup strategies
- Consider cache warming after restores

### Configuration Backup
- Store configuration in version control
- Use Infrastructure as Code (IaC)
- Document deployment procedures

### Monitoring and Alerting
- Monitor service availability
- Alert on high error rates
- Track cache hit ratios