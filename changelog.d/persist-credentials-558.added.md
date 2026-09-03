- **`make pins-test` now holds the companion clause too: every `actions/checkout` drops the
  credential** (#558). The guard added for #518 read the SHA pin and deliberately deferred this
  half, because a rule for it would have gone red on the three call sites that lacked the flag;
  those are fixed in the same change, so the rung lands with them rather than before them. Reading
  an input means reading a block, which the pin rung had refused to do — so the guard now measures
  a step's extent from its key column, in both directions since YAML fixes no key order, and reads
  only the lines at its `with:` block's own column. `persist-credentials` anywhere else stops the
  run by name and line rather than being counted: under `env:`, inside a block scalar, nested
  under another input, in flow style, set twice, or spelled in a way the scan cannot read. Each is
  a way the input reaches a diff, reads as compliant, and reaches checkout never. An explaining
  comment is deliberately not required, and the docstring says why.
