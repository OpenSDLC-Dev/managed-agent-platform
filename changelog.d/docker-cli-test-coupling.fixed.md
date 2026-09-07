- **The Docker sandbox tests and the provider now agree on which daemon they mean** (#627).
  Those tests shell out to the `docker` CLI for states the sandbox API cannot produce — a container
  stopped but not destroyed, an image built outside the provider, a container it does not own. The CLI
  follows the active `docker context`; the provider reads `DOCKER_HOST` and then the well-known socket.
  On a host where those name different daemons a fixture was created against one and read back from the
  other, and the failure read as though the product had lost it. Every such call in the package now goes
  through one helper that passes the provider's own address as `--host`, which outranks both environment
  variables. The helper that reads a container's state also stops slicing that status out of index
  arithmetic that could panic on empty output, and stops discarding the daemon's message when the read
  fails. The documented requirements for `make test` now name the `docker` binary, which the fixtures
  that drive the daemon through the CLI rather than its HTTP API have always needed. No product
  behaviour changes.
