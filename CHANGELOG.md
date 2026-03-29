# Changelog

## 0.6.0 (2026-03-29)

### Features

- **tui**: add unified Output interface and template system ([0e0f02a](https://github.com/urmzd/saige/commit/0e0f02aa022bdff50b418f54f94f1c1b43629cf9))

### Bug Fixes

- **tui**: reduce cyclomatic complexity in stream rendering ([6a8c351](https://github.com/urmzd/saige/commit/6a8c351e4fa19f14149aac0248a80a51a35e05b6))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.5.0...v0.6.0)


## 0.5.0 (2026-03-29)

### Breaking Changes

- **rag**: extract context assembler to dedicated package ([0ba981c](https://github.com/urmzd/saige/commit/0ba981c0ccb915fb8602129f9003a82314f2b83f))
- **tree**: propagate context through tree operations ([d0ae090](https://github.com/urmzd/saige/commit/d0ae090f3b9397687d33c6640667347aa7050b69))

### Features

- **provider**: handle structured tool errors across adapters ([f878467](https://github.com/urmzd/saige/commit/f878467849967c71ab06a2bddef085a74babd0a7))
- **agent**: add functional options pattern ([686db0e](https://github.com/urmzd/saige/commit/686db0ebf317af6f70ade4149c3312b50cc09975))
- **types**: add IsError flag to tool results ([a0df939](https://github.com/urmzd/saige/commit/a0df939d1ccdfd212fad1c349a045946e2d69f01))

### Documentation

- update README with new features and examples ([a6e641c](https://github.com/urmzd/saige/commit/a6e641cc8c0e73f364fdab28378286964aa4b770))

### Miscellaneous

- **agent**: update for context propagation and error handling ([18729b7](https://github.com/urmzd/saige/commit/18729b7e910533ab2b7a09092505db44268bd503))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.4.1...v0.5.0)


## 0.4.1 (2026-03-28)

### Bug Fixes

- resolve all golangci-lint errors for v2 compatibility ([e1e3000](https://github.com/urmzd/saige/commit/e1e30003f7265b0bfb900821edc774e5564a6772))
- **deps**: bump Go to 1.25.8 and x/net to v0.45.0 ([baf9596](https://github.com/urmzd/saige/commit/baf95965531fa7d56821c2bfc577a81c75b99f9e))
- **ci**: upgrade golangci-lint to v2 for Go 1.25 compatibility ([3e1560c](https://github.com/urmzd/saige/commit/3e1560cdfc73aabce6a4f643ad6c681e778f3929))

### Documentation

- update README ([d5bd6e8](https://github.com/urmzd/saige/commit/d5bd6e86b697290afcee1a461814841eaa770496))
- **skills**: align SKILL.md with agentskills.io spec ([465d111](https://github.com/urmzd/saige/commit/465d1117f4049d5411155efe6dc68040fd08a932))
- remove architecture diagram, fix project name ([6df379e](https://github.com/urmzd/saige/commit/6df379edbb580ea404610a8f6bbb5dbe5019988d))

### Miscellaneous

- use sr-releaser GitHub App for release workflow (#11) ([f4d3a7d](https://github.com/urmzd/saige/commit/f4d3a7dc4efc4d9dfbd49fd172012e13680066d5))
- update semantic-release action to sr@v2 ([0753bcf](https://github.com/urmzd/saige/commit/0753bcfc5a94ecc5c42043ea21549a47ca2cea12))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.4.0...v0.4.1)


## 0.3.1 (2026-03-22)

### Bug Fixes

- **examples**: update default ollama model to qwen3.5:4b ([310170d](https://github.com/urmzd/saige/commit/310170d6d612d8cd0fe2f173a5377415de4d3282))

### Miscellaneous

- **teasr**: configure output formats and font settings ([ae7b1f3](https://github.com/urmzd/saige/commit/ae7b1f3b5ae75d1ef0d68670659c8f5c262d621d))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.3.0...v0.3.1)


## 0.3.0 (2026-03-22)

### Features

- **rag**: add parallelism, tokenizer, extractors, sources, and caching ([2b2bc72](https://github.com/urmzd/saige/commit/2b2bc727bd9478e34898da0c0bc4ba8b626366cb))

### Documentation

- add showcase screenshot ([fdf3e88](https://github.com/urmzd/saige/commit/fdf3e88023dbb5ea7f2c1a183227cd2e403c3c0e))
- add RLHF feedback to README, AGENTS.md, llms.txt, and skill ([7d492b3](https://github.com/urmzd/saige/commit/7d492b3ff265cb89ef9ef55e095185225a4ced86))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.2.1...v0.3.0)


## 0.2.1 (2026-03-19)

### Bug Fixes

- **agent**: redesign feedback as permanent leaf nodes off target ([d2317dd](https://github.com/urmzd/saige/commit/d2317dd18f368d04b1c20332922cc157a3848508))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.2.0...v0.2.1)


## 0.2.0 (2026-03-19)

### Features

- **agent**: add RLHF feedback events for conversation rating ([28bb7ef](https://github.com/urmzd/saige/commit/28bb7ef44e8c12c9d070131fe3cb0bafb12b73bb))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.1.0...v0.2.0)


## 0.1.0 (2026-03-19)

### Features

- unify adk, kgdk, and ragdk into saige ([f45815c](https://github.com/urmzd/saige/commit/f45815c69624504b4bd5b0280bb72f99e1375ffa))
