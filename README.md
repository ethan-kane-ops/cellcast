# cellcast

Multi-cluster deployment placement oracle and short-lived credential broker

## Requirements

- Go 1.22+
- [mise](https://mise.jdx.dev/) — runtime manager (`brew install mise`)
- [just](https://just.systems/) — task runner (managed by mise)

## Getting started

```bash
mise install        # install pinned Go + tools
just build          # compile → bin/cellcast
./bin/cellcast --help
```

## Development

```bash
just check          # tidy + lint + test (run before every commit)
just test           # go test ./...
just lint           # go vet + golangci-lint
just build          # compile
just install        # install to $GOBIN
```
