- **`environment/`'s Terraform state moved to a bucket, so destroy and re-apply are no longer
  tied to one laptop** (#478). Local state made two of the four Terraform operations
  non-portable and one of them **silently**: `terraform destroy` iterates the state, so on a
  machine that does not hold it destroy finds nothing to destroy and reports *success* while
  the environment keeps billing. The bucket is owned by `foundation/` — it has to outlive
  every `make gcp-env-destroy`, which is exactly what that half means — and is versioned, so
  a state lost to a bad apply is recoverable. The backend block is deliberately **partial**: a
  backend cannot interpolate a variable, and a bucket name is an operator identifier this
  public repository does not carry (#356), so it arrives at `init` time — derived from
  `PROJECT` and `NAME_PREFIX` on both sides rather than recorded as one more coordinate. That
  is why the targets that touch it now require `PROJECT`. `make gcp-env-migrate-state` is the
  one-time move, and it has to run from the machine that still holds the local file.
  `foundation/`'s own state stays local, deliberately: a remote backend for it would need a
  bucket the foundation exists to own, and none of #478's failure modes reach a configuration
  with no destroy target at all.
