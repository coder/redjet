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
	rm -f /tmp/*.bench.out
	libs=(GoRedis Redigo Redjet);
	for lib in $${libs[@]}; do
		# We need to standardize the names so that benchstat can compare them
		go test -bench=$$lib -count=10 -timeout=60s -run=$$lib | \
			sed "s/$$lib/Redis/g" > \
			/tmp/$$lib.bench.out
		echo "Finished benchmarking $$lib";
	done
	benchstat -table .config -row=unit -col .file Redjet=/tmp/Redjet.bench.out Redigo=/tmp/Redigo.bench.out GoRedis=/tmp/GoRedis.bench.out > benchstat.txt