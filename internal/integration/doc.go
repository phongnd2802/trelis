// Package integration drives Trelis through its HTTP API, the primary test
// seam: each test boots the real binary against Postgres (ParadeDB), Neo4j
// and an S3-compatible store in testcontainers and a Temporal dev server,
// with a [modeltest.FakeProvider] standing in for every model, and asserts
// only on what an API client observes.
//
// The tests need Docker and are behind the `integration` build tag, so
// `make test` skips them:
//
//	make test-integration
//	make test-integration-linked // with the anydoc engine linked
//
// Each test gets its own Trelis process, Postgres database, task queue and
// fake provider; the containers, Neo4j, object store and Temporal dev server
// are shared by the package.
//
// [modeltest.FakeProvider]: https://pkg.go.dev/github.com/phongnd2802/trelis/internal/models/modeltest#FakeProvider
package integration
