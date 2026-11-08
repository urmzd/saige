# Session persistence

A session key is `(tenant, parent, agent, label)`. Opening the same key
continues its transcript. Labels select parallel conversations; `Fresh`
generates a new label. Parents can be external place URIs or existing sessions
in the same tenant. Evidence links do not grant read access.

Memory and Postgres stores expose the same identity, relationship, run-binding,
explicit read-grant and atomic admission interfaces. Budget accounts reserve
bounded child allocations and retain immutable settlement receipts. Actual
model-call accounting IDs update the session and its ancestors once, including
usage reported after a parent finishes. Hosts authenticate and authorize before
calling these tenant-scoped stores; these interfaces do not authenticate users.

The Postgres adapter owns only its tables, accepts the host's pool, and performs
migrations explicitly through `Migrate`. Unit and concurrency coverage lives in
`session_test.go` and `accounting_test.go`. Set `SAIGE_TEST_POSTGRES_DSN` to run the
same contracts against an isolated Postgres database, including restart checks.
