- **A zero-byte write no longer opens a Kubernetes exec stream that can never close**
  (#318). A write of no bytes opened an exec stdin stream the client closed at once; the
  pod's side then never completed, stalling the run until `go test`'s package alarm killed
  the whole binary. Such a write now asks for no stream. Because that stream also counted
  the bytes, the write reads one byte itself first, so a stream that disagrees with its
  declared size is still refused instead of landing an empty file over the target. The
  backend's watchdog assertion now measures a paired difference rather than absolute
  latency, so a loaded host no longer fails it. Bounding a wedged exec in production is
  #383.
