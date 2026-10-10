// Package stress holds load and concurrency tests for the SDK. The tests sit
// behind the stress build tag, so plain `go test ./...` never runs them:
//
//	go test -tags stress -race -count=1 -v ./integration/stress/
//
// The in-process tests need nothing else. The PostgreSQL tests run when
// SAIGE_TEST_POSTGRES_DSN names a server with pgvector and pg_search, such as
// the paradedb/paradedb image. The live burst runs only with SAIGE_LIVE=1 and
// provider keys. Tunables:
//
//	SAIGE_STRESS_AGENTS  concurrent agents in TestConcurrentAgents (default 200)
//	SAIGE_STRESS_BURST   parallel calls per model in TestLiveBurst (default 20)
//
// See docs/load-testing.md.
package stress
