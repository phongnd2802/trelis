module github.com/phongnd2802/trelis

go 1.27.4

require github.com/phongnd2802/trelis/third_party/anydoc-go v0.0.0

// anydoc-go is a vendored fork of the Go bindings from firecrawl/anydoc#30
// (taken from Tencent/WeKnora). It is never fetched by path; the static
// archive it links is built by scripts/build-anydoc-lib.sh.
replace github.com/phongnd2802/trelis/third_party/anydoc-go => ./third_party/anydoc-go
