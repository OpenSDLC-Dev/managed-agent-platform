- **The operator's own deployment coordinates are no longer written into the
  documentation** (#514). #355/#356 moved them into GitHub Actions variables by hand,
  but that sweep stopped at `deploy/` and `.github/`: two acceptance records in
  `docs/HISTORY.md` still carried a project id, two project numbers, an Artifact
  Registry path and — found while fixing the rest — two routable LoadBalancer
  addresses, and one project id survived in a Go test fixture. Each is now the
  variable name the workflow reads, or a description where no variable exists.
  Nothing here was a credential and no rotation is implied; the repository is
  public, and these were coordinates it had no reason to publish.
