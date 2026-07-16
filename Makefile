# The single executable source of the merge gate. CI invokes these targets and
# the verifier runs `make verify`; prose (CLAUDE.md, AGENTS.md, README.md)
# names targets instead of duplicating commands. Mirrors
# .github/workflows/ci.yml — a new check lands in both, in the same PR.

.PHONY: build crossbuild vet fmt-check test cover-gate verify

build:
	go build ./...

# The BYOC worker is meant to cross-compile; 32-bit-only breakage
# (e.g. an untyped 64-bit const) is invisible to the host build.
crossbuild:
	GOOS=linux GOARCH=arm go build ./internal/...

vet:
	go vet ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

# Coverage denominator: logic packages only. internal/pgtest and
# internal/sandbox/sandboxtest are test support — packages solely because a
# test in another package must import them. Their uncovered statements are the
# assertion branches that run only when a suite fails, so counting them
# measures nothing and dilutes the gate, exactly as cmd/ main glue would.
test:
	go test -count=1 \
		-coverpkg="$$(go list ./internal/... | grep -vE '/(pgtest|sandboxtest)$$' | paste -sd, -)" \
		-coverprofile=coverage.out ./...

# Exact total from the profile: `go tool cover -func` rounds to 0.1%, which
# would let ~89.95% pass as "90.0". Duplicate blocks (same package covered by
# several test binaries) merge with OR semantics; an empty profile fails closed.
cover-gate:
	@awk 'NR>1 { stmts[$$1]=$$2; if ($$3>0) covered[$$1]=1 } \
	     END { \
	       for (k in stmts) { t+=stmts[k]; if (k in covered) c+=stmts[k] } \
	       if (t==0) { print "no statements in coverage profile"; exit 1 } \
	       pct=100*c/t; \
	       printf "total statement coverage: %.2f%%\n", pct; \
	       exit !(pct >= 90.0) \
	     }' coverage.out

verify: build crossbuild vet fmt-check test cover-gate
