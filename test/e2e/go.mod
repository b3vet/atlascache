module github.com/b3vet/atlascache/test/e2e

go 1.24

replace github.com/b3vet/atlascache => ../..

// The sdk-* specs drive the SDK itself rather than the suite's own client, so
// this module imports it like any other consumer would (ADR-0015). The replace
// is what keeps it the SDK in this tree; go.work would hide its absence here
// and nowhere else.
replace github.com/b3vet/atlascache/pkg/client => ../../pkg/client

require (
	github.com/b3vet/atlascache/pkg/client v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
)
