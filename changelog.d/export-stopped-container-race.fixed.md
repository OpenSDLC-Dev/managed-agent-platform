- **A Docker sandbox test stops racing the teardown it asks for** (#625).
  `TestExportWorksOnAStoppedContainer` stopped its container by killing that container's init from a
  command running inside it, and then asserted on the command's own exit status — a status the teardown
  can pre-empt, which is how the test turned up red in CI. It now stops the container from outside, as
  the attach test always did, through one helper the two of them share; that helper bounds the wait, so
  a daemon which never answers fails a named test rather than the whole package. Test-only.
