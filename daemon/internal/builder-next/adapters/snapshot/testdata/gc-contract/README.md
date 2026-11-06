# Multistage COPY fixture

`A/file.txt` and `B/file.txt` are the input files beside the Dockerfile.
The Go regression tests embed these same files and assemble an in-memory
build context from them, without requiring a Docker client session.

To build this directory independently, run from this directory:

```sh
docker buildx build --no-cache --output type=local,dest=/tmp/gc-contract-output .
```

The output contains `app/A/file.txt` with `A` and `app/B/file.txt` with `B`.
This standalone build checks the fixture. The Go tests separately enforce
the GC/checksum timing needed to reproduce the shared-snapshot bug.
