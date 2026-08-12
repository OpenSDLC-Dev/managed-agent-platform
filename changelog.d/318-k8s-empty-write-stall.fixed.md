- **A zero-byte write no longer opens a Kubernetes exec stream that can never close** (#318).
  Five of the six `coverage` failures in one week of CI — the merge gate, on branches whose
  diffs contained no Go at all — were the same panic: `test timed out after 10m0s`,
  `FAIL internal/sandbox/k8s 600.0xxs`. The package's green time is about two minutes, so
  the ten-minute alarm was not a stopwatch running out; it was the only thing ending a
  stall. Every one of the five hung on the same call and no other: `sb.WriteFile(ctx, path,
  nil)`, a write of no bytes, with the test goroutine parked in
  `remotecommand.StreamWithContext` for eight to nine minutes while the stdout copy, the
  stdin drain and the error-stream watcher all sat blocked on `spdystream.(*Stream).Read`
  and the connection's frame reader sat in `IO wait`. Nothing on the client was running:
  the pod's side of the exec simply never completed, and one stuck exec cost every row in
  the package, because `go test`'s alarm kills the whole binary.
  The write path opened a stdin stream for those writes, and the client's copy of it
  finished and closed it in the same instant it was created. A write of no bytes now asks
  for no stream at all, the way the bulk scripts that read no stdin are already given none.
  It costs the in-pod script nothing — a container whose exec requested no stdin reads EOF
  immediately, so `tee | wc -c` still counts the same zero the script compares its expected
  size against, measured against a live cluster.
  Why a cluster loses that close is *not* established, and this does not claim to have
  diagnosed it. What is established is the correlation: all five stalls landed on one of the
  suite's two zero-byte writes, none on the hundreds of writes that carry bytes. Removing
  the stream removes the race. The containment gap it exposed — that nothing in the
  production path bounds a wedged exec, and the lease keeper renews through one rather than
  noticing it — is #383, deliberately not fixed here.
  Raising `go test`'s ten-minute package timeout, which #318 proposed, was **rejected on the
  same evidence**: a package that normally finishes in two minutes has five times the
  headroom it needs, so the default is not policing slowness, and a larger one would only
  make each stall burn longer before saying so.

- **The watchdog assertion stops measuring the runner's load** (#318). The Kubernetes
  backend's deadline watchdog must not hold the exec's stderr open after the command exits,
  or every timed command returns up to a poll interval — about a second — late. The row that
  guarded it timed one `echo hi` and failed above 900ms absolute, which is a cluster's
  latency at least as much as the property: it was observed failing at 2.27s on a loaded
  machine and passing at 0.85s, fifty milliseconds inside the bound, on the same machine
  idle. It is now measured as a difference — the same command run with and without a
  deadline, five interleaved pairs, the median of their differences deciding — since both
  halves make the same two round trips and the deadline adds only the watchdog. Measured
  against a live cluster, the difference is single-digit milliseconds idle and tens of
  milliseconds on a host under fivefold CPU load, while the regression it exists for is a
  fixed second: with the watchdog's `3>&-` removed the difference measured 1.002s, 1.005s
  and 1.003s across three runs.
  The median rather than the best pair, because the most permissive statistic is the one a
  guard can least afford: `min` goes green as soon as one pair's noise runs the favourable
  way by half a second, which on a host at 28 load average it nearly did — the regression's
  five differences came out 779ms, 901ms, 928ms, 936ms, 1.15s, so the smallest had lost
  220ms of a 1s signal to noise while the median had not. The median has to be bought three
  times out of five in whichever direction is wrong. Both directions were then measured on
  the same oversubscribed host: three runs red against the mutated script (medians 928ms and
  1.045s), three runs green against the fixed one.
  The script's own half of that property — the watchdog closing its inherited copy of the
  stream — is now pinned without a cluster at all, on the host's own shell, by running the
  wrapper with its stderr on an `os.Pipe` and requiring the pipe to reach EOF once the
  wrapper exits. It costs milliseconds, it cannot be blurred by cluster latency, and with
  the `3>&-` removed it fails every time, including at a quarter of a CPU.
