// The Go SDK is its own module (ADR-0015, resolved 2026-09-14) so that a
// consumer importing it does not inherit the server's dependency graph.
//
// There is no require block, and that is the point: everything below this line
// is stdlib. Adding a dependency here is a decision about every project that
// imports the SDK, so it needs the same scrutiny as an ADR.
module github.com/b3vet/atlascache/pkg/client

go 1.24
