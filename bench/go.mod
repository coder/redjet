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
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gomodule/redigo v1.8.9 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/redis/go-redis/v9 v9.0.5 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
