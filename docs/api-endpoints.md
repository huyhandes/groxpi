# API Endpoints

Groxpi provides a fully compliant PyPI Simple API (PEP 503/691) with additional cache management endpoints.

## Package Index Endpoints

### List All Packages
- **Endpoints**: 
  - `GET /simple/` (PEP 503 standard)
  - `GET /index/` (compatibility)
- **Description**: Returns a list of all available packages
- **Content Negotiation**: 
  - HTML: Browser-friendly package listing
  - JSON: API response for pip/poetry/pipenv clients
- **Headers**: 
  - `Accept: application/json` → JSON response
  - `Accept: text/html` → HTML response
- **Compression**: Automatic gzip/deflate based on client support

**Example JSON Response:**
```json
{
  "packages": [
    {
      "name": "numpy",
      "normalized_name": "numpy"
    },
    {
      "name": "requests", 
      "normalized_name": "requests"
    }
  ]
}
```

### List Package Files
- **Endpoints**:
  - `GET /simple/{package}/` (PEP 503 standard)
  - `GET /index/{package}` (compatibility)
- **Description**: Returns available files for a specific package
- **Parameters**: 
  - `package`: Package name (case-insensitive, normalized)
- **Content Negotiation**: HTML/JSON based on Accept header

**Example JSON Response:**
```json
{
  "files": [
    {
      "filename": "numpy-1.24.3-cp39-cp39-win32.whl",
      "url": "https://files.pythonhosted.org/packages/.../numpy-1.24.3-cp39-cp39-win32.whl",
      "hashes": {
        "sha256": "abc123..."
      },
      "requires-python": ">=3.8",
      "size": 12345678
    }
  ]
}
```

### Download/Redirect to File
- **Endpoints**:
  - `GET /simple/{package}/{file}` (PEP 503 standard)
  - `GET /index/{package}/{file}` (compatibility)
- **Description**: Downloads file or redirects to upstream URL
- **Parameters**:
  - `package`: Package name
  - `file`: Filename
- **Behavior**:
  - If cached: Serves file directly with optimized streaming
  - If not cached: Downloads, caches, then serves (or redirects based on timeout)
  - Uses SingleFlight pattern to deduplicate concurrent downloads

## Administrative Endpoints

### Home Page
- **Endpoint**: `GET /`
- **Description**: Server status and statistics
- **Response**: HTML page with:
  - Server information
  - Cache statistics
  - Performance metrics
  - System health indicators

### Health Check
- **Endpoint**: `GET /health`
- **Description**: Detailed health status for monitoring
- **Response**: JSON with system information

**Example Response:**
```json
{
  "status": "healthy",
  "timestamp": "2024-01-01T12:00:00Z",
  "version": "1.0.0",
  "uptime": "2h30m45s",
  "cache": {
    "index_entries": 1250,
    "file_cache_size": "2.1GB",
    "hit_ratio": 0.89
  },
  "storage": {
    "type": "local",
    "available_space": "45.2GB"
  },
  "indices": [
    {
      "url": "https://pypi.org/simple/",
      "status": "healthy",
      "last_check": "2024-01-01T11:58:30Z"
    }
  ]
}
```

## Cache Management Endpoints

### Invalidate Package List Cache
- **Endpoint**: `DELETE /cache/list`
- **Description**: Clears the cached package list
- **Response**: `200 OK` with confirmation message
- **Use Case**: Force refresh of package list from upstream indices

### Invalidate Package Cache
- **Endpoint**: `DELETE /cache/{package}`
- **Description**: Clears cached data for a specific package
- **Parameters**:
  - `package`: Package name to invalidate
- **Response**: `200 OK` with confirmation message
- **Use Case**: Force refresh of package files/metadata

### Method Not Allowed Handler
- **Endpoint**: `ALL /cache/list` (except DELETE)
- **Description**: Returns 405 Method Not Allowed for non-DELETE requests
- **Response**: `405 Method Not Allowed`

## Error Responses

### 404 Not Found
- **Condition**: Invalid routes or non-existent packages/files
- **Response**: `404 Not Found` with plain text message

### 500 Internal Server Error
- **Condition**: Server errors, upstream failures
- **Response**: `500 Internal Server Error` with error details
- **Logging**: Full error context logged for debugging

### 502 Bad Gateway
- **Condition**: Upstream index unavailable
- **Response**: `502 Bad Gateway` when all configured indices fail

## Content Negotiation Details

### Accept Headers
- `application/json`: Returns JSON response (PEP 691 compliant)
- `text/html`: Returns HTML response with templates
- `*/*` or missing: Defaults to JSON for API clients

### Compression Support
- **Supported**: gzip, deflate
- **Automatic**: Based on `Accept-Encoding` header
- **Performance**: Significant bandwidth savings for JSON responses

## Caching Behavior

### Index Caching
- **TTL**: Configurable per index (default: 30 minutes)
- **Strategy**: In-memory cache with automatic expiration
- **Invalidation**: Manual via `/cache/list` endpoint

### File Caching
- **Strategy**: LRU eviction with size limits
- **Storage**: Configurable (local filesystem or S3)
- **Streaming**: Zero-copy optimization for large files

### Response Caching
- **Duration**: Short-term response caching (5 minutes default)
- **Key**: URL + Accept header combination
- **Benefit**: Reduces redundant processing for repeated requests

## Rate Limiting & Performance

### Concurrent Handling
- **Downloads**: SingleFlight pattern prevents duplicate downloads
- **Connections**: Handles 1000+ concurrent connections
- **Streaming**: Efficient file serving with minimal memory usage

### Timeouts
- **Download**: Configurable timeout before redirect (default: 0.9s)
- **Connect**: Socket connection timeout (default: 30s)
- **Read**: Data read timeout (default: 30s)

## Compatibility

### PEP 503 Compliance
- ✅ Simple repository API
- ✅ Package name normalization
- ✅ File hash verification support
- ✅ Metadata support

### PEP 691 Compliance
- ✅ JSON API variant
- ✅ Content negotiation
- ✅ Structured metadata format

### Client Compatibility
- ✅ pip (all versions)
- ✅ poetry
- ✅ pipenv
- ✅ conda/mamba (via pip fallback)
- ✅ PDM
- ✅ uv