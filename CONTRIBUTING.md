# Contributing

## Prerequisites

- Go 1.26.11+
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

See [docs/guides/load-testing.md](docs/guides/load-testing.md) for the Postgres and live provider options.

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

## Documentation site

From the repository root, use [uv](https://docs.astral.sh/uv/) to run the pinned generator.

```sh
uvx --from zensical==0.1.2 zensical serve
uvx --from zensical==0.1.2 zensical build
```

The preview command prints its local address. The build writes the static site to `site/`. Edit the Markdown in `docs/` and the navigation in `zensical.toml`.

To see the shared typefaces, logo and colors locally under `/saige/`, follow [Preview a guide with the shared brand](https://github.com/urmzd/urmzd.github.io#preview-a-guide-with-the-shared-brand) after building the site.

Pull requests to `main` build the site in strict mode. Merging to `main` publishes the result through this repository’s GitHub Pages workflow to [the Saige guide](https://docs.urmzd.com/saige/).
