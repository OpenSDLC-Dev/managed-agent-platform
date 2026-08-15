- **The delivery pipeline builds its images on the runner** (#349). The deploy
  workflow now runs `docker build` and `docker push` itself instead of
  submitting to Cloud Build, which failed at the build step with the caller
  forbidden from the project's `_cloudbuild` staging bucket and stayed red with
  both documented IAM remedies applied. `gcloud builds submit` stages the source
  as the *caller* rather than as the build's `--service-account`, so every
  manual run that worked was made by a human with Owner and none exercised the
  path CI takes. Building on the runner needs one permission the deploy identity
  already holds, `roles/artifactregistry.writer`, and no bucket, staging upload or
  Cloud Build API. `deploy/gcp/cloudbuild.yaml` is unchanged and remains the
  manual path's build definition.
