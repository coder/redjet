# redjet
[![Go Reference](https://pkg.go.dev/badge/github.com/ammario/redjet.svg)](https://pkg.go.dev/github.com/ammario/redjet)
![ci](https://github.com/ammario/redjet/actions/workflows/ci.yaml/badge.svg)
[![Coverage Status](https://coveralls.io/repos/github/ammario/redjet/badge.svg)](https://coveralls.io/github/ammario/redjet)



redjet is a high-performance Go library for Redis. Its hallmark feature is
a low-allocation, streaming API. See the [benchmarks](#benchmarks) section for
more details.

Unlike [redigo](https://github.com/gomodule/redigo) and [go-redis](https://github.com/redis/go-redis), redjet does not provide a function for every
Redis command. Instead, it offers a generic interface that supports [all commands
and options](https://redis.io/commands/). While this approach has less
type-safety, it provides forward compatibility with new Redis features.

In the aim of both performance and ease-of-use, redjet attempts to provide
an API that closely resembles the protocol. For example, the `Command` method
is really a Pipeline of size 1.

<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->
**Table of Contents**

- [redjet](#redjet)
  - [Basic Usage](#basic-usage)
  - [Streaming](#streaming)
  - [Pipelining](#pipelining)
  - [PubSub](#pubsub)
  - [Connection Pooling](#connection-pooling)
  - [Benchmarks](#benchmarks)
  - [Limitations](#limitations)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->

## Basic Usage

Install:

```bash
go get github.com/ammario/redjet@latest
```

For the most part, you can interact with Redis using a familiar interface:

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/ammario/redjet"
)

func main() {
    client := redjet.New("localhost:6379")
    ctx := context.Background()

    err := client.Command(ctx, "SET", "foo", "bar").Ok()
    // check error

    got, err := client.Command(ctx, "GET", "foo").Bytes()
    // check error
    // got == []byte("bar")
}
```

## Streaming

To minimize allocations, call `(*Result).WriteTo` instead of `(*Result).Bytes`.
`WriteTo` streams the response directly to an `io.Writer` such as a file or HTTP response.

For example:

```go
_, err := client.Command(ctx, "GET", "big-object").WriteTo(os.Stdout)
// check error
```

Similarly, you can pass in a value that implements `redjet.LenReader` to
`Command` to stream larger values into Redis. Unfortunately, the API
cannot accept a regular `io.Reader` because bulk string messages in
the Redis protocol are length-prefixed.

Here's an example of streaming a large file into Redis:

```go
bigFile, err := os.Open("bigfile.txt")
// check error
defer bigFile.Close()

stat, err := bigFile.Stat()
// check error

err = client.Command(
    ctx, "SET", "bigfile",
    redjet.NewLenReader(bigFile, stat.Size()),
).Ok()
// check error
```


If you have no way of knowing the size of your blob in advance and still
want to avoid large allocations, you may chunk a stream into Redis using repeated [`APPEND`](https://redis.io/commands/append/) commands.

## Pipelining

`redjet` supports [pipelining](https://redis.io/docs/manual/pipelining/) via the `Pipeline` method. This method accepts a Result, potentially that of a previous, open command.

```go
// Set foo0, foo1, ..., foo99 to "bar", and confirm that each succeeded.
//
// This entire example only takes one round-trip to Redis!
var r *Result
for i := 0; i < 100; i++ {
    r = client.Pipeline(r, "SET", fmt.Sprintf("foo%d", i), "bar")
}

for r.Next() {
    if err := r.Ok(); err != nil {
        log.Fatal(err)
    }
}
```

Fun fact: authentication happens over a pipeline, so it doesn't incur a round-trip.


## PubSub

redjet suports PubSub via the `NextSubMessage` method. For example:

```go
// Subscribe to a channel
sub := client.Command(ctx, "SUBSCRIBE", "my-channel")
sub.NextSubMessage() // ignore the first message, which is a confirmation of the subscription

// Publish a message to the channel
n, err := client.Command(ctx, "PUBLISH", "my-channel", "hello world").Int()
// check error
// n == 1, since there is one subscriber

// Receive the message
sub.NextSubMessage()
// sub.Payload == "hello world"
// sub.Channel == "my-channel"
// sub.Type == "message"
```

Note that `NextSubMessage` will block until a message is received. To interrupt the subscription, cancel the context passed to `Command`.

Once a connection enters subscribe mode, the internal pool does not
re-use it.

It is possible to subscribe to a channel in a performant, low-allocation way
via the public API. NextSubMessage is just a convenience method.

## Connection Pooling

Redjet provides automatic connection pooling. Configuration knobs exist
within the `Client` struct that may be changed before any Commands are
issued.

If you want synchronous command execution over the same connection,
use the `Pipeline` method and consume the Result after each call to `Pipeline`. Storing a long-lived `Result`
offers the same functionality as storing a long-lived connection.

## Benchmarks

On a pure throughput basis, redjet will perform similarly to redigo and go-redis.
But, since redjet doesn't allocate memory for the entire response object, it
consumes far less resources when handling large responses.

Here are some benchmarks (reproducible via `make gen-bench`) to illustrate:

```
goos: darwin
goarch: arm64
pkg: github.com/ammario/redjet/bench
 │    redjet    │             redigo             │               go-redis               │
 │    sec/op    │    sec/op     vs base          │    sec/op     vs base                │
   1.296m ± 10%   1.401m ± 10%  ~ (p=0.075 n=10)   1.519m ± 14%  +17.21% (p=0.000 n=10)

 │    redjet    │             redigo              │               go-redis                │
 │     B/s      │      B/s       vs base          │      B/s       vs base                │
   771.8Mi ± 9%   715.9Mi ± 10%  ~ (p=0.075 n=10)   658.6Mi ± 12%  -14.67% (p=0.000 n=10)

 │   redjet   │                    redigo                    │                   go-redis                   │
 │    B/op    │      B/op        vs base                     │      B/op        vs base                     │
   49.50 ± 9%   1047456.50 ± 0%  +2115973.74% (p=0.000 n=10)   1056983.50 ± 0%  +2135220.20% (p=0.000 n=10)

 │   redjet   │            redigo            │              go-redis               │
 │ allocs/op  │ allocs/op   vs base          │ allocs/op   vs base                 │
   3.000 ± 0%   3.000 ± 0%  ~ (p=1.000 n=10)   6.000 ± 0%  +100.00% (p=0.000 n=10)
```

Note that these results are a bit contrived in that they GET a 1MB value. The performance
of all libraries converge as response size decreases.

## Limitations

- redjet does not have tidy support for client side caching. But, the redjet API
  is flexible enough that a client could implement it themselves by following the instructions [here](https://redis.io/docs/manual/client-side-caching/#two-connections-mode).
- RESP3 is not supported. Practically, this means that connections aren't
  multiplexed, and other Redis libraries may perform better in high-concurrency
  scenarios.
- Certain features have not been tested but may still work:
  - Redis Streams
  - Monitor