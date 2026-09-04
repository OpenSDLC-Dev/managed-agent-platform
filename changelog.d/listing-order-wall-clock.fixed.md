- **The `internal/api` listing tests stop resting on millisecond gaps in the wall clock** (#561).
  Thirteen of them read a position or a `created_at` boundary out of a listing whose rows were
  written 1.5 to 6 ms apart — less than the 20 ms backwards step of the database clock recorded in
  #411. Those orders are the wire contract the tests exist to pin, so they are kept and assigned
  instead: one shared helper stamps the named rows a second apart, from a single `now()`, in one
  statement. Where a test still needs a row written afterwards to come back newest, that now turns
  on a second rather than on the milliseconds a request leaves. The memory-version listing also
  gains the `id`-tiebreak case its quoted contract always promised and nothing exercised. Test-only.
