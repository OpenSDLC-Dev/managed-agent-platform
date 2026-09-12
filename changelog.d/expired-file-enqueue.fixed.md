- **The expired-file sweep can no longer lose the objects it orphans** — it removed a
  file's row and then deleted the object best-effort, which left two ways for a batch to
  vanish without trace: a shutdown landing while the statement's ids were still being read
  could commit the removal and lose the ids with it, and a store refusing every key of a
  healthy sweep did the same for up to a thousand objects an hour, logging counts and no
  ids. Either way nothing in any tier still named those objects. The sweep now records the
  debt instead of paying it — one `pending_object_deletes` row per object, written in the
  transaction that removes the file rows — so the removal and the record commit together or
  not at all, and the object-delete drain that already serves session deletes retries each
  key with backoff and drops none. This sweep therefore reaches no object store: it has no
  budget to run out of, nothing best-effort left in it, and a deployment whose store is down
  or absent falls behind rather than losing anything. The control plane's termination grace
  period is unchanged but is now margin rather than arithmetic, the detached 30-second
  cleanup it was sized for having gone with the object deletes (#696, and the first finding
  of #698).
