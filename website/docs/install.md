# Install

## Prebuilt binary (linux/amd64)

Prebuilt **linux/amd64** binaries are attached to each GitHub Release.
Download, verify the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_linux_amd64.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_linux_amd64.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```

!!! tip "Latest release"
    Find all releases — including the latest version number to substitute for
    `VERSION` above — on the
    [Releases page](https://github.com/imantaba/kubeagent/releases).

## Build from source

If you have Go installed, you can build directly from the repository:

```bash
go build -o kubeagent .
```

Requires Go 1.22 or later. The resulting binary has no external runtime
dependencies.
