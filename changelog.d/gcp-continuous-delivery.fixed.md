- **The GCP Cloud Build path builds again**. `deploy/gcp/cloudbuild.yaml` had
  described a build nobody had run since the Dockerfile gained
  `FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build`:
  `BUILDPLATFORM`, `TARGETOS` and `TARGETARCH` are BuildKit-only variables, so
  under Cloud Build's classic builder they expand to the empty string and the
  daemon rejects the platform specifier outright —
  `failed to parse platform : "" is an invalid component of ""`. Both docker
  steps now carry `env: ["DOCKER_BUILDKIT=1"]`, which is what makes the build run
  at all rather than a performance choice. Alongside it,
  `options.logging: CLOUD_LOGGING_ONLY`, which becomes mandatory as soon as a
  build names its own service account — and it must, because a project created
  under an organisation no longer receives the automatic Editor grant on the
  Compute Engine default service account, and that identity therefore cannot read
  the source tarball `gcloud builds submit` has just uploaded
  (`does not have storage.objects.get access to … <project>_cloudbuild`). The two
  out-of-band IAM grants that follow from it — `roles/logging.logWriter` on the
  project and `roles/storage.admin` on the `_cloudbuild` bucket, both on the
  deploy identity, which uploads that tarball as well as reading it back — are
  recorded in
  [deploy/gcp/README.md](./deploy/gcp/README.md#continuous-delivery), since
  nothing in the repository would otherwise say they exist.
