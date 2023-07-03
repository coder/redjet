module github.com/ammario/redjet/bench

go 1.20

// The benchmarks are separated into its own module to avoid ballooning
// the main package with other Redis library dependencies.

require (
	github.com/ammario/redjet v0.0.0-20230703191230-a607112e096c
	github.com/stretchr/testify v1.8.4
)

replace github.com/ammario/redjet => ../

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
