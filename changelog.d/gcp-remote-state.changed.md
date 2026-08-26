- **`environment/`'s Terraform state moved to a bucket, so destroy and re-apply are no longer
  tied to one laptop** (#478). Local state made two of the four Terraform operations
  non-portable and one of them **silently**: `terraform destroy` iterates the state, so on a
  machine that does not hold it destroy finds nothing to destroy and reports *success* while
  the environment keeps billing. The bucket is owned by `foundation/`, which has to outlive
  every `make gcp-env-destroy`, and is versioned so a state lost to a bad apply is
  recoverable. The backend block is deliberately **partial**: a backend cannot interpolate a
  variable, and a bucket name is an operator identifier this public repository does not carry
  (#356), so it arrives at `init` time — derived from `PROJECT` and `NAME_PREFIX` on both
  sides rather than recorded as one more coordinate, which is why the targets that touch it
  now require `PROJECT`. Two guards stop the move reintroducing what it removes: those two
  variables choose the *bucket* while `terraform.tfvars` chooses the *resources*, so a
  disagreement is refused rather than applied against another environment's state; and a
  destroy over an empty remote state is refused, that being the original silent no-op wearing
  a new backend. `make gcp-env-migrate-state` is the one-time move, from the machine still
  holding the local file. `foundation/`'s own state stays local, deliberately.
