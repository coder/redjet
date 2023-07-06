SHELL = /bin/bash
.ONESHELL:

.PHONY: test

test:
	go test -timeout=3m -race . -coverprofile=coverage.out -covermode=atomic

send-cover:
	go install github.com/mattn/goveralls@v0.0.12
	goveralls -coverprofile=coverage.out -service=github

.PHONY: lint
lint:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.53.3
	~/go/bin/golangci-lint run

.PHONY: doctoc
doctoc:
	doctoc --title "**Table of Contents**" --github README.md

.PHONY: gen-bench
gen-bench:
	pushd bench
	libs=(go-redis redigo redjet);
	for lib in $${libs[@]}; do
		echo "Benchmarking $$lib";
		go test -bench=. -count=10 -run=. -lib=$$lib \
		-memprofile=/tmp/$$lib.mem.out -cpuprofile=/tmp/$$lib.cpu.out | tee /tmp/$$lib.bench.out
		echo "Finished benchmarking $$lib";
	done
	benchstat -table .fullname -row=unit -col .file redjet=/tmp/redjet.bench.out redigo=/tmp/redigo.bench.out go-redis=/tmp/go-redis.bench.out > benchstat.txt