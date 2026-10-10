# Contributing

## Prerequisites

- Go 1.26.9+
- [just](https://github.com/casey/just) (optional)
- `GH_TOKEN` for releases

## Getting Started

```bash
git clone git@github.com:urmzd/saige.git
cd saige
```

## Development

```bash
go vet ./...        # static analysis
go test ./...       # run tests
gofmt -w .          # format
go build ./...      # build
```

### Load and concurrency testing

The stress suite and the `-cpu` benchmarks are opt-in:

```bash
go test -tags stress -race -count=1 -v ./integration/stress/
go test -run '^$' -bench . -cpu 1,4,8 ./...
```

See [docs/load-testing.md](docs/load-testing.md) for the Postgres and live provider options.

## Commit Convention

Angular conventional commits enforced by [gitit](https://github.com/urmzd/gitit):

```
feat: add new retriever type
fix: correct BM25 scoring edge case
docs: update README examples
```

## Pull Requests

1. Fork and create a feature branch
2. Write tests for new functionality
3. Ensure `go test ./...` passes
4. Open a PR against `main`
