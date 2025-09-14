# GitHub Workflows

Groxpi uses GitHub Actions for automated CI/CD, building, testing, and releasing.

## Workflows Overview

### CI/CD Pipeline (`ci.yml`)
- **Triggers**: Push to main/develop, pull requests
- **Purpose**: Continuous integration with testing, linting, and security scanning
- **Platforms**: Ubuntu, macOS
- **Features**: Go testing, security scanning, code quality checks

### Release Automation (`release.yml`)
- **Triggers**: Push to `release-*` branches, manual dispatch
- **Purpose**: Automated releases with cross-platform binaries and Docker images
- **Artifacts**: Multi-platform binaries, Docker images, checksums
- **Registry**: GitHub Container Registry (ghcr.io)

### Docker Pipeline (`docker.yml`)
- **Triggers**: Push to main, tags, PRs, manual dispatch
- **Purpose**: Advanced Docker image building, testing, and security scanning
- **Features**: Multi-arch builds, vulnerability scanning, image testing

## Quick Start

### Create a Release
```bash
# Create release branch
git checkout -b release-1.2.3
git push origin release-1.2.3

# Automatic release creation with:
# - Cross-platform binaries
# - Multi-arch Docker images
# - GitHub release v1.2.3
```

### Use Docker Images
```bash
# Latest release
docker run -p 5000:5000 ghcr.io/huyhandes/groxpi:latest

# Specific version
docker run -p 5000:5000 ghcr.io/huyhandes/groxpi:v1.2.3
```

## Release Artifacts

Each release includes:
- **Binaries**: Linux, macOS (amd64, arm64)
- **Checksums**: SHA256 for all binaries
- **Docker Images**: Multi-architecture containers
- **Source Code**: Automated GitHub release

## Security Features

- **Code Scanning**: gosec, golangci-lint
- **Container Security**: Trivy vulnerability scanning
- **Dependency Checks**: Automated security monitoring
- **SARIF Integration**: Security findings in GitHub Security tab

For detailed information, see [tasks/github-workflows-setup.md](../tasks/github-workflows-setup.md).