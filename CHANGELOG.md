# Changelog

## 0.9.0 (2026-04-09)

### Features

- absorb zoro research tools and add MCP server ([e0a3474](https://github.com/urmzd/saige/commit/e0a347443b003b95cfce5bd3a718df70fc891e98))

### Bug Fixes

- **ci**: suppress gosec G304 on intentional file tools, bump Go to 1.25.9 ([b9f0292](https://github.com/urmzd/saige/commit/b9f0292159ef69e448bc2c3bf476ff33a12b42e1))

### Documentation

- add research tools, SearXNG client, graph formatting, and MCP server ([872a33f](https://github.com/urmzd/saige/commit/872a33feeb1eeb90aead8a2748df9ecf6aef1752))

### Miscellaneous

- **gitignore**: ignore .fastembed_cache ([d60dc69](https://github.com/urmzd/saige/commit/d60dc69eb3dcef3213bebadc0aad835dafc3cecd))
- add linguist overrides to fix language stats (#25) ([39066cf](https://github.com/urmzd/saige/commit/39066cf684dd3b11e1a0c3333793f43741ce3a78))
- **deps**: bump github.com/anthropics/anthropic-sdk-go ([5dcdf7c](https://github.com/urmzd/saige/commit/5dcdf7c9ea3aadf96ca465a035d1bdd21792f238))
- **deps**: bump github.com/openai/openai-go/v3 from 3.29.0 to 3.30.0 ([a513c57](https://github.com/urmzd/saige/commit/a513c57e81a2f7404f88d3c41ecfc67ab1e1703e))
- **deps**: bump actions/setup-go from 5 to 6 ([ce416d1](https://github.com/urmzd/saige/commit/ce416d1db81baeb0eea46c755b660ce377261acf))
- **deps**: bump actions/create-github-app-token from 1 to 3 ([f17d9da](https://github.com/urmzd/saige/commit/f17d9dae6a23a7e4be404d133e4a137aeaa73a93))
- **deps**: bump golangci/golangci-lint-action from 6 to 9 ([e7c4e02](https://github.com/urmzd/saige/commit/e7c4e02d72d0d201eb05ce801cf081723655504d))
- **deps**: bump actions/upload-artifact from 4 to 7 ([e35b2f1](https://github.com/urmzd/saige/commit/e35b2f1016620ae365943356e1aac899e56843d8))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.8.0...v0.9.0)


## 0.8.0 (2026-04-05)

### Features

- add persistence, extended thinking, and observability (#22) ([69067fa](https://github.com/urmzd/saige/commit/69067fa4c2f247b8389f5864af9c3ed209fc5778))
- **eval**: add universal evaluation framework (#21) ([9e18876](https://github.com/urmzd/saige/commit/9e1887652313bb1c7f48ef6100d1d81d2cdbb2ba))

### Miscellaneous

- remove arxiv binary and ignore build artifacts ([29f7794](https://github.com/urmzd/saige/commit/29f7794baf6f5e6bc65319e486a8cc36c69fb565))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.7.0...v0.8.0)


## 0.7.0 (2026-03-30)

### Features

- **ollama**: add thinking support to generation ([46f8124](https://github.com/urmzd/saige/commit/46f81247a95dc3c7ad32690d75af1ceb90abe808))

### Bug Fixes

- **agent**: reduce runLoop complexity and add compaction tests ([2523714](https://github.com/urmzd/saige/commit/2523714cae68481183e648cacd65c44c1c9ee377))
- **agent**: redesign compaction to use tree branching and fix related bugs ([36a9799](https://github.com/urmzd/saige/commit/36a979959a81f2467fe8fcb6e2040d1368662646))

### Miscellaneous

- update sr action from v2 to v3 ([b9f698a](https://github.com/urmzd/saige/commit/b9f698a3c38d4251231f6bfb50a61e1d2f0ecafb))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.6.1...v0.7.0)


## 0.6.1 (2026-03-29)

### Bug Fixes

- **release**: add patch bump for refactor commit type ([7c33ba0](https://github.com/urmzd/saige/commit/7c33ba06b695fe6fe6d2c2d11a002ee8ba54192f))

### Miscellaneous

- **deps**: bump actions/checkout from 4 to 6 ([15902ab](https://github.com/urmzd/saige/commit/15902ab56021e6ef2e1068ae1f6c27b163d370b4))
- **deps**: bump golang.org/x/net from 0.41.0 to 0.52.0 ([00e495b](https://github.com/urmzd/saige/commit/00e495bb1fdfd9a600230d2b9241a763514d1823))
- **deps**: bump github.com/openai/openai-go/v3 from 3.27.0 to 3.29.0 ([bbabe89](https://github.com/urmzd/saige/commit/bbabe897056d1539a529055bd787f221fc9a7cba))
- **deps**: bump github.com/anthropics/anthropic-sdk-go ([66ed8cf](https://github.com/urmzd/saige/commit/66ed8cfec35fc5bfd6aa93595410f16ef690cdc5))

[Full Changelog](https://github.com/urmzd/saige/compare/v0.6.0...v0.6.1)


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
