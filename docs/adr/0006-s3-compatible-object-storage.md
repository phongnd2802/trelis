# Source Files and Figures live in S3-compatible object storage; every read goes through Trelis

The bytes of each Source File, and of each Figure taken out of a Document when it is parsed, are stored in one private bucket of an S3-compatible object store. Postgres stays the source of truth (ADR-0002): it holds the Document and Figure rows, their Access Lists and the object key of each blob, never the bytes. Clients never reach the bucket. They read a Source File or a Figure through the Trelis API, which checks the Access List on every request and streams the bytes back. We chose this because Figures multiply the number and size of blobs well past what belongs in Postgres rows, one S3 API covers silo (an S3-compatible MinIO fork) in development and AWS S3, R2, Ceph or Garage in production, and keeping every read behind `can_read_document` means a change to an Access List takes effect on the next request, with no URL already handed out that still works.

## Considered Options

- **Bytes in Postgres (`bytea`).** Rejected: backups, replication and vacuum grow with every Figure, and streaming a large blob out of a row is awkward.
- **A local volume** (what Trelis first shipped in #101). Rejected: it ties the service to one host's disk, and it would be a second code path next to S3 that only development uses.
- **One adapter per vendor** (Omnisc: local, MinIO, S3, COS, OSS, OBS, TOS, KS3). Rejected: one S3-compatible provider with a configurable endpoint and path-style addressing reaches the same stores without seven adapters to maintain.
- **Presigned URLs, or a public bucket.** Rejected for now: a presigned URL keeps working until it expires, even after the Access List changes or the link is passed on, which is Leakage by our own definition. If download throughput ever needs it, it gets its own ADR.

## Consequences

- Objects are keyed by IDs, never by the uploaded file name: `documents/{document_id}/{upload_id}/source` and `documents/{document_id}/{upload_id}/figures/{seq}.{ext}`. The database stores the key; the bucket comes from configuration, so a bucket or vendor can change without rewriting rows.
- Each upload writes under a fresh `upload_id`. A re-upload never overwrites the bytes an in-flight ingest is reading; the previous upload's objects are deleted only after the new version's Chunks are committed.
- Postgres and the bucket are not written in one transaction. On upload the object is written first and the row second (the object is deleted if the row fails). On delete the row goes first and an idempotent Temporal activity removes the `documents/{document_id}/` prefix (ADR-0005). A Document row therefore never points at a missing object, but an object can briefly outlive its row; the reconciler removes prefixes with no Document.
- Bytes are served so they cannot run in the API's origin: a Source File always as `Content-Disposition: attachment`, a Figure inline only for raster image types (anything else, SVG included, as an attachment), always with `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`. A Document the caller cannot read answers the same 404 as one that does not exist.
- Development and CI run [silo](https://github.com/pgsty/silo), an S3-compatible fork of MinIO (both compose files, and a testcontainer in the integration seam through the MinIO module). It runs as a separate server, so its AGPL license does not reach Trelis. There is no local-disk provider; unit tests use an in-memory fake of the storage interface.
- The bucket is created on start when `storage.s3.create_bucket` is set (the development default); otherwise a missing bucket fails the storage check in `/readyz`.
