- **A tool call the docker sandbox timed out is no longer reported as an ordinary kill** (#390).
  The deadline was always enforced; the label was not. Classification rested on two probes a
  loaded host can schedule after the watchdog's kill has landed: both read false beside exit code
  137, and the model was told its command had died rather than run too long. The watchdog now
  marks its own kill, read as a third witness; the k8s backend lost the same verdict to the same
  race (#95, #110) and shares the fix. The mark can only *raise* a timeout verdict — a hostile
  command can suppress its own mark and get the old mislabel back — and nothing gates the kill on
  it, so none outlives its deadline.
