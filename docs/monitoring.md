# Monitoring & Observability

Groxpi provides comprehensive monitoring and observability features for production deployments, including structured logging, health checks, and metrics collection.

## Current Features

### Structured Logging
Groxpi uses [phuslu/log](https://github.com/phuslu/log) for high-performance structured logging.

#### Log Levels
- **DEBUG**: Detailed debugging information
- **INFO**: General operational information (default)
- **WARN**: Warning conditions
- **ERROR**: Error conditions requiring attention

#### Log Format
```json
{
  "time": "2024-01-01T12:00:00Z",
  "level": "info",
  "message": "request completed",
  "method": "GET",
  "path": "/simple/numpy/",
  "status": 200,
  "latency": "25.4ms",
  "user_agent": "pip/23.0",
  "remote_ip": "192.168.1.100"
}
```

#### Configuration
```bash
export GROXPI_LOGGING_LEVEL=INFO  # DEBUG, INFO, WARN, ERROR
```

### Health Checks
Comprehensive health monitoring endpoint for container orchestration.

#### Health Endpoint
- **URL**: `GET /health`
- **Response**: JSON with detailed system information
- **Use**: Docker health checks, load balancer probes

#### Health Response
```json
{
  "status": "healthy",
  "timestamp": "2024-01-01T12:00:00Z",
  "version": "1.0.0",
  "uptime": "2h30m45s",
  "cache": {
    "index_entries": 1250,
    "file_cache_size": "2.1GB",
    "file_cache_entries": 450,
    "response_cache_entries": 89,
    "hit_ratio": 0.89
  },
  "storage": {
    "type": "local",
    "available_space": "45.2GB",
    "cache_directory": "/app/cache"
  },
  "indices": [
    {
      "url": "https://pypi.org/simple/",
      "status": "healthy",
      "last_check": "2024-01-01T11:58:30Z",
      "response_time": "123ms"
    }
  ],
  "performance": {
    "requests_total": 125847,
    "requests_per_second": 45.2,
    "average_response_time": "15ms",
    "error_rate": 0.02
  }
}
```

### Performance Metrics
Real-time performance tracking integrated into the server.

#### Request Metrics
- **Total requests**: Counter with method/status breakdown
- **Response times**: Histogram with P50/P95/P99
- **Error rates**: 4xx/5xx response tracking
- **Concurrent connections**: Active connection count

#### Cache Metrics
- **Hit ratios**: Index cache, file cache, response cache
- **Cache sizes**: Memory usage and entry counts
- **Eviction rates**: LRU cache eviction frequency
- **Miss penalties**: Time spent on cache misses

#### System Metrics
- **Memory usage**: Heap size, GC frequency, allocations
- **CPU usage**: Process CPU utilization
- **Disk I/O**: File cache read/write operations
- **Network I/O**: Upstream request latency

### Error Recovery
Automatic error recovery with comprehensive logging.

#### Panic Recovery
- **Middleware**: Automatic panic recovery
- **Logging**: Full stack trace logging
- **Response**: 500 Internal Server Error
- **Metrics**: Panic counter for alerting

#### Graceful Shutdown
- **Signal handling**: SIGTERM/SIGINT support
- **Connection draining**: Active requests completion
- **Resource cleanup**: Cache persistence, file handles
- **Timeout**: Configurable shutdown timeout

## Monitoring Setup

### Docker Health Checks
Built-in Docker health check configuration:

```dockerfile
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:5000/health || exit 1
```

### Prometheus Integration (Planned)
Configuration for Prometheus metrics scraping.

#### Prometheus Configuration
```yaml
# monitoring/prometheus.yml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'groxpi'
    static_configs:
      - targets: ['groxpi:5000']
    metrics_path: '/metrics'
    scrape_interval: 5s
    scrape_timeout: 5s
```

#### Metrics Endpoint (Planned)
- **URL**: `GET /metrics`
- **Format**: Prometheus exposition format
- **Metrics**: Application and system metrics

### Grafana Dashboards (Planned)
Pre-configured Grafana dashboards for visualization.

#### Dashboard Features
- **Request Rate**: RPS over time with breakdown
- **Response Times**: Latency histograms and percentiles
- **Cache Performance**: Hit ratios and cache efficiency
- **Error Monitoring**: Error rate trends and alerting
- **System Health**: Memory, CPU, and disk usage

#### Dashboard Configuration
```yaml
# monitoring/grafana/datasources/prometheus.yml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

## Production Monitoring

### Key Metrics to Monitor

#### Application Metrics
- **Request rate**: Requests per second (target: >100 RPS)
- **Response time**: P95 latency (target: <100ms)
- **Error rate**: 4xx/5xx percentage (target: <1%)
- **Cache hit ratio**: Index/file cache efficiency (target: >90%)

#### System Metrics
- **Memory usage**: Heap size and GC pressure (target: <512MB)
- **CPU utilization**: Process CPU usage (target: <50%)
- **Disk space**: Cache directory usage (monitor: >80% full)
- **Network latency**: Upstream index response time (target: <500ms)

### Alerting Rules (Prometheus)

#### Critical Alerts
```yaml
groups:
  - name: groxpi.critical
    rules:
      - alert: GroxpiDown
        expr: up{job="groxpi"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Groxpi service is down"
          
      - alert: HighErrorRate
        expr: rate(groxpi_requests_total{status=~"5.."}[5m]) > 0.05
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "High 5xx error rate"
```

#### Warning Alerts
```yaml
      - alert: HighLatency
        expr: histogram_quantile(0.95, rate(groxpi_request_duration_seconds_bucket[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High request latency"
          
      - alert: LowCacheHitRate
        expr: groxpi_cache_hit_ratio < 0.8
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Cache hit rate below threshold"
```

### Log Analysis

#### Structured Log Fields
- **timestamp**: RFC3339 formatted timestamp
- **level**: Log level (debug, info, warn, error)
- **message**: Human-readable message
- **method**: HTTP method
- **path**: Request path
- **status**: HTTP status code
- **latency**: Request processing time
- **user_agent**: Client user agent
- **remote_ip**: Client IP address
- **package**: Package name (for package requests)
- **cache_hit**: Cache hit/miss indicator

#### Log Aggregation
Recommended log aggregation setup:
- **ELK Stack**: Elasticsearch, Logstash, Kibana
- **Grafana Loki**: Lightweight log aggregation
- **Fluentd/Fluent Bit**: Log forwarding and processing

### Observability Best Practices

#### Monitoring Strategy
1. **Golden Signals**: Latency, traffic, errors, saturation
2. **RED Method**: Rate, errors, duration for services
3. **USE Method**: Utilization, saturation, errors for resources
4. **SLI/SLO**: Service level indicators and objectives

#### Recommended SLOs
- **Availability**: 99.9% uptime (8.76 hours downtime/year)
- **Latency**: 95% of requests < 100ms
- **Error Rate**: < 0.1% of requests return 5xx errors
- **Cache Hit Rate**: > 90% for index requests

### Container Monitoring

#### Docker Integration
```yaml
# docker-compose.yml monitoring section
version: '3.8'
services:
  groxpi:
    # ... other config
    labels:
      - "prometheus.io/scrape=true"
      - "prometheus.io/port=5000"
      - "prometheus.io/path=/metrics"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:5000/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 40s
```

#### Kubernetes Integration
```yaml
apiVersion: v1
kind: Service
metadata:
  name: groxpi
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "5000"
    prometheus.io/path: "/metrics"
spec:
  ports:
    - port: 5000
      targetPort: 5000
  selector:
    app: groxpi
```

### Troubleshooting

#### Common Issues
- **High memory usage**: Check cache size configuration
- **Slow response times**: Monitor upstream index latency
- **Cache misses**: Verify TTL configuration and storage health
- **Connection errors**: Check network connectivity to indices

#### Debug Mode
Enable debug logging for detailed troubleshooting:
```bash
export GROXPI_LOGGING_LEVEL=DEBUG
```

Debug logs include:
- Cache hit/miss details
- Upstream request/response information
- Storage operation timing
- Memory allocation patterns