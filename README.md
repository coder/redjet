# redjet


`redjet` is a high-performance Go library for Redis. It's hallmark feature is
a zero-allocation, streaming API.

Unlike redigo and go-redis, `redjet` does not provide a function for every
Redis command. Instead, it offers a generic interface that supports (all commands
and options)[https://redis.io/commands/].


## Benchmarks

TBD