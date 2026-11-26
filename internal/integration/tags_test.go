//go:build integration && !anydoc

package integration

// buildTags are the tags Trelis is built with: the same as this test binary,
// so `-tags integration,anydoc` tests the linked engine.
const buildTags = ""
