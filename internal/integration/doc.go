// Package integration drives Trelis through its HTTP API, the primary test
// seam: each test boots the real binary against Postgres (ParadeDB) and
// Neo4j in testcontainers, with a [fakeprovider.Provider] standing in for
// every model, and asserts only on what an API client observes.
//
// The tests need Docker and are behind the `integration` build tag, so
// `make test` skips them:
//
//	make test-integration
//
// Each test gets its own Trelis process, Postgres database and fake
// provider; the containers and Neo4j are shared by the package.
//
// [fakeprovider.Provider]: https://pkg.go.dev/github.com/phongnd2802/trelis/internal/fakeprovider#Provider
package integration
