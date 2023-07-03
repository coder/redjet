SHELL = /bin/bash
.ONESHELL:

.PHONY: test

test:
	go test -race -timeout=60s .

.PHONY: doctoc
doctoc:
	doctoc --title "**Table of Contents**" --github README.md

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