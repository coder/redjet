# redjet
[![Go Reference](https://pkg.go.dev/badge/github.com/coder/redjet.svg)](https://pkg.go.dev/github.com/coder/redjet)
![ci](https://github.com/coder/redjet/actions/workflows/ci.yaml/badge.svg)
[![Coverage Status](https://coveralls.io/repos/github/coder/redjet/badge.svg)](https://coveralls.io/github/coder/redjet)
[![Go Report Card](https://goreportcard.com/badge/github.com/coder/redjet)](https://goreportcard.com/report/github.com/coder/redjet)



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
  - [JSON](#json)
  - [Connection Pooling](#connection-pooling)
  - [Benchmarks](#benchmarks)
  - [Limitations](#limitations)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->

## Basic Usage

Install:

```bash
go get github.com/coder/redjet@latest
```

For the most part, you can interact with Redis using a familiar interface:

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/coder/redjet"
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

To minimize allocations, call `(*Pipeline).WriteTo` instead of `(*Pipeline).Bytes`.
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

`redjet` supports [pipelining](https://redis.io/docs/manual/pipelining/) via the `(*Client).Pipeline` method. This method accepts a `Pipeline`, potentially that of a previous, open command.

```go
// Set foo0, foo1, ..., foo99 to "bar", and confirm that each succeeded.
//
// This entire example only takes one round-trip to Redis!
var p *Pipeline
for i := 0; i < 100; i++ {
    p = client.Pipeline(p, "SET", fmt.Sprintf("foo%d", i), "bar")
}

for r.Next() {
    if err := p.Ok(); err != nil {
        log.Fatal(err)
    }
}
p.Close() // allow the underlying connection to be reused.
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

## JSON

`redjet` supports convenient JSON encoding and decoding via the `(*Pipeline).JSON` method. For example:

```go
type Person struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}

// Set a person
// Unknown argument types are automatically encoded to JSON.
err := client.Command(ctx, "SET", "person", Person{
    Name: "Alice",
    Age:  30,
}).Ok()
// check error

// Get a person
var p Person
client.Command(ctx, "GET", "person").JSON(&p)
// check error

// p == Person{Name: "Alice", Age: 30}
```

## Connection Pooling

Redjet provides automatic connection pooling. Configuration knobs exist
within the `Client` struct that may be changed before any Commands are
issued.

If you want synchronous command execution over the same connection,
use the `Pipeline` method and consume the Pipeline after each call to `Pipeline`. Storing a long-lived `Pipeline`
offers the same functionality as storing a long-lived connection.

## Benchmarks

On a pure throughput basis, redjet will perform similarly to redigo and go-redis.
But, since redjet doesn't allocate memory for the entire response object, it
consumes far less resources when handling large responses.

Here are some benchmarks (reproducible via `make gen-bench`) to illustrate:

```
.fullname: Get/1_B-10
 │   redjet    │               redigo               │           go-redis            │               rueidis                │
 │   sec/op    │   sec/op     vs base               │   sec/op     vs base          │    sec/op     vs base                │
   908.2n ± 2%   962.4n ± 1%  +5.97% (p=0.000 n=10)   913.8n ± 3%  ~ (p=0.280 n=10)   1045.0n ± 1%  +15.06% (p=0.000 n=10)

 │    redjet     │                redigo                │            go-redis             │               rueidis                │
 │      B/s      │      B/s       vs base               │      B/s       vs base          │     B/s       vs base                │
   1074.2Ki ± 2%   1015.6Ki ± 1%  -5.45% (p=0.000 n=10)   1069.3Ki ± 2%  ~ (p=0.413 n=10)   937.5Ki ± 1%  -12.73% (p=0.000 n=10)

 │  redjet   │            redigo            │           go-redis            │            rueidis            │
 │   B/op    │    B/op     vs base          │    B/op      vs base          │    B/op      vs base          │
   0.00 ± 0%   41.00 ± 0%  ? (p=0.000 n=10)   275.50 ± 2%  ? (p=0.000 n=10)   249.00 ± 0%  ? (p=0.000 n=10)

 │   redjet   │            redigo            │           go-redis           │           rueidis            │
 │ allocs/op  │ allocs/op   vs base          │ allocs/op   vs base          │ allocs/op   vs base          │
   0.000 ± 0%   3.000 ± 0%  ? (p=0.000 n=10)   4.000 ± 0%  ? (p=0.000 n=10)   2.000 ± 0%  ? (p=0.000 n=10)

.fullname: Get/1.0_kB-10
 │   redjet    │               redigo                │              go-redis               │               rueidis               │
 │   sec/op    │   sec/op     vs base                │   sec/op     vs base                │   sec/op     vs base                │
   1.302µ ± 2%   1.802µ ± 1%  +38.42% (p=0.000 n=10)   1.713µ ± 3%  +31.58% (p=0.000 n=10)   1.645µ ± 1%  +26.35% (p=0.000 n=10)

 │    redjet    │                redigo                │               go-redis               │               rueidis                │
 │     B/s      │     B/s       vs base                │     B/s       vs base                │     B/s       vs base                │
   750.4Mi ± 2%   542.1Mi ± 1%  -27.76% (p=0.000 n=10)   570.3Mi ± 3%  -24.01% (p=0.000 n=10)   593.8Mi ± 1%  -20.87% (p=0.000 n=10)

 │    redjet    │             redigo             │            go-redis            │            rueidis             │
 │     B/op     │     B/op      vs base          │     B/op      vs base          │     B/op      vs base          │
   0.000Ki ± 0%   1.039Ki ± 0%  ? (p=0.000 n=10)   1.392Ki ± 0%  ? (p=0.000 n=10)   1.248Ki ± 1%  ? (p=0.000 n=10)

 │   redjet   │            redigo            │           go-redis           │           rueidis            │
 │ allocs/op  │ allocs/op   vs base          │ allocs/op   vs base          │ allocs/op   vs base          │
   0.000 ± 0%   3.000 ± 0%  ? (p=0.000 n=10)   4.000 ± 0%  ? (p=0.000 n=10)   2.000 ± 0%  ? (p=0.000 n=10)

.fullname: Get/1.0_MB-10
 │   redjet    │            redigo             │              go-redis               │            rueidis            │
 │   sec/op    │   sec/op     vs base          │   sec/op     vs base                │   sec/op     vs base          │
   472.5µ ± 7%   477.3µ ± 2%  ~ (p=0.190 n=10)   536.8µ ± 6%  +13.61% (p=0.000 n=10)   475.3µ ± 6%  ~ (p=0.684 n=10)

 │    redjet    │             redigo             │               go-redis               │            rueidis             │
 │     B/s      │     B/s       vs base          │     B/s       vs base                │     B/s       vs base          │
   2.067Gi ± 8%   2.046Gi ± 2%  ~ (p=0.190 n=10)   1.819Gi ± 6%  -11.98% (p=0.000 n=10)   2.055Gi ± 6%  ~ (p=0.684 n=10)

 │   redjet    │                    redigo                    │                   go-redis                   │                   rueidis                    │
 │    B/op     │      B/op        vs base                     │      B/op        vs base                     │      B/op        vs base                     │
   51.00 ± 12%   1047849.50 ± 0%  +2054506.86% (p=0.000 n=10)   1057005.00 ± 0%  +2072458.82% (p=0.000 n=10)   1048808.50 ± 0%  +2056387.25% (p=0.000 n=10)

 │   redjet   │               redigo                │              go-redis               │               rueidis               │
 │ allocs/op  │ allocs/op   vs base                 │ allocs/op   vs base                 │ allocs/op   vs base                 │
   1.000 ± 0%   3.000 ± 0%  +200.00% (p=0.000 n=10)   4.000 ± 0%  +300.00% (p=0.000 n=10)   2.000 ± 0%  +100.00% (p=0.000 n=10)

```

## Limitations

- redjet does not have convenient support for client side caching. But, the redjet API
  is flexible enough that a client could implement it themselves by following the instructions [here](https://redis.io/docs/manual/client-side-caching/#two-connections-mode).
- RESP3 is not supported. Practically, this means that connections aren't
  multiplexed, and other Redis libraries may perform better in high-concurrency
  scenarios.
- Certain features have not been tested but may still work:
  - Redis Streams
  - Monitor