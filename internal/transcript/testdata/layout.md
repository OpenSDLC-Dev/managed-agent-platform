# session sesn_layout

## user

the lease test flakes in CI; find out why

## agent

Reading the test first.

## tool call: bash

{"command":"go test -run TestLease ./internal/queue/"}

## tool result

--- FAIL: TestLease (0.31s)
    lease_test.go:44: renewal missed

## system

the sandbox was restarted

## tool call: search_docs

{"query":"lease renewal"}

## tool result

Lease renewal
docs://queue/lease
a keeper renews at half the TTL

## tool result

[image]
the CI timing chart

## agent

The TTL budget is too tight for CI.

