The delivery pipeline builds its images on the runner instead of submitting
them to Cloud Build. Its first real run failed at the build step in under a
second — `The user is forbidden from accessing the bucket
[…_cloudbuild]` — and kept failing with both documented remedies applied,
`roles/storage.admin` on that bucket and
`roles/serviceusage.serviceUsageConsumer` on the project. The trap underneath
it is that `gcloud builds submit` stages the source as the **caller**, not as
the build's `--service-account`, so every manual run that worked was called by
a human with Owner and none of them exercised the path CI takes. Building on
the runner needs one permission the job already holds,
`artifactregistry.writer`, and no bucket, no staging upload and no Cloud Build
API. `deploy/gcp/cloudbuild.yaml` is unchanged and remains the manual path's
build definition.
