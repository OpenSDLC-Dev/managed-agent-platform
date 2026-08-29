- **The operator's own deployment coordinates are out of the documentation** (#514).
  #355/#356 moved them into GitHub Actions variables by hand, but that sweep stopped at
  `deploy/` and `.github/`: three acceptance records in `docs/HISTORY.md` still carried a
  project id, two project numbers, an Artifact Registry path and a zone, and — found while
  fixing the rest — two **routable** LoadBalancer addresses. A project id survived in a Go
  test fixture, and the staging database's private address in `deploy/gcp/README.md` went
  with them. Each is now the variable name the workflow reads, or a placeholder where no
  variable exists; every acceptance record still makes the same claim about the same run.
  Nothing here was a credential and no rotation is implied — these were coordinates a
  public repository had no reason to publish.
