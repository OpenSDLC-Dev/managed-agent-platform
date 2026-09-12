- **The bundled MinIO now comes from quay.io** — Docker Hub stopped serving the
  `minio` namespace, answering an anonymous pull with "repository does not exist"
  rather than a rate limit, so every machine without the image already cached lost
  it at once: the compose stack, the chart's in-cluster MinIO, and the
  object-storage contract tests alike. All three now pin the identical release at
  `quay.io/minio/minio` — MinIO's own mirror, where that tag was checked to
  resolve to the same manifest digest the Docker Hub copy had. An operator
  mirroring images into a private registry needs a second remote pointed at
  quay.io for this one, because a remote repository pointed at Docker Hub cannot
  serve it; `deploy/gcp`'s mirror output says so where it explains the rewrite
  (#701).
