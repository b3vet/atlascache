module github.com/b3vet/atlascache

go 1.24

// The Go SDK is its own module (ADR-0015). atlasctl lives here and imports it,
// so the dependency has to be declared even though go.work resolves it locally:
// without the require and the replace below, a build outside the workspace --
// CI, or anyone who fetches this module -- compiles atlasctl against whatever
// version of the SDK is published rather than the one in this tree.
require github.com/b3vet/atlascache/pkg/client v0.0.0

replace github.com/b3vet/atlascache/pkg/client => ./pkg/client

require (
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/google/uuid v1.6.0
	github.com/rs/zerolog v1.34.0
	github.com/spf13/viper v1.21.0
	github.com/stretchr/testify v1.11.1
	go.uber.org/goleak v1.3.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.28.0 // indirect
)
