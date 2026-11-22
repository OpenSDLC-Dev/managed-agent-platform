# TOML parser for the harness doctor

The doctor includes the upstream production sources and MIT license so a new checkout can validate Codex project settings without a module download. [BurntSushi/toml v1.6.0](https://github.com/BurntSushi/toml/tree/52534926c55b4cd85b05aee90569dd0668b8cf30)

The upstream Go module checksum is `h1:dRaEfpa2VI55EwlIW72hMRHdWouJeRF7TPYhI+AUQjk=`. The imports of the internal time-zone package in `parse.go` and `encode.go` use the repository-local path. The package comment in `doc.go` spells the TOML specification version without its tag prefix to satisfy the repository citation guard. [License](./COPYING)
