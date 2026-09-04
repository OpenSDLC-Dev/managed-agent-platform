- **The memory-version tests stop resting a wire contract on the wall clock** (#551).
  `TestMemoryVersionsPerOperation` asserts a four-row newest-first listing whose rows four
  HTTP requests wrote a few milliseconds apart. That order is the contract the test exists to
  pin, so it is kept and the clock taken out from under it instead: one statement stamps the
  four rows a second apart, three of them named by the id their own responses returned and
  the fourth, which the delete response does not name, by exclusion.
  `TestMemoryVersionRedact` took the superseded row by position and now names it by id, which
  needs no order at all. Test-only.
