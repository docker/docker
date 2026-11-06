# Jobs API

Jobs are one-shot workloads managed by the daemon.
A job is a container spec plus a trigger declaration, and every execution is a run with a durable identity that survives container removal and daemon restarts.
Jobs fire manually or on a cron schedule evaluated by the daemon, so scheduled work keeps firing while no client is connected.

The builtin `org.mobyproject.jobs.v0` extension provides this point and offers it for publication, so the daemon serves it as a gRPC service on its socket.
The API is experimental and may change between releases.
See [jobs.go](jobs.go) for the full contract; [jobs.proto](jobs.proto) is generated from it.

## Enabling the extension

The extension ships with the daemon but is disabled by default:

```console
$ dockerd --extension-enable org.mobyproject.jobs.v0
```

or in `daemon.json`:

```json
{
  "extensions": {
    "enable": ["org.mobyproject.jobs.v0"]
  }
}
```

When the extension is not enabled, calls fail with `Unimplemented`.
While the daemon is starting, calls fail with `Unavailable` until the extension has reconciled its runs.

Authorization plugins are not supported yet.
They do not evaluate gRPC calls on the daemon socket, so Jobs API calls bypass them.
The extension also drives run containers through the daemon API anonymously, so a plugin that denies anonymous requests keeps the Jobs API unavailable.
Do not enable the extension on a daemon that relies on authorization plugins.

## Calling the API

Go clients resolve the point through the extensions client:

```go
extensions, err := extensionclient.New(apiClient, extensionclient.WithGRPCPoint(protogen.ClientPoint))
if err != nil {
    return err
}
jobs, err := extensionclient.Resolve(extensions, jobsv0.Point)
```

Any gRPC client works, for example grpcurl:

```console
$ grpcurl -plaintext -unix -proto extpoints/jobs/api/v0/jobs.proto \
    -d '{"name": "backup", "spec": {"container_spec": "eyJJbWFnZSI6ICJidXN5Ym94In0="}}' \
    /var/run/docker.sock org.mobyproject.extension.jobs.api.v0.Jobs/Create
```

`container_spec` is a JSON container-create request body; unknown fields are rejected.

## Semantics

- `Create` registers a job and arms its trigger without starting a run.
  It is idempotent on a canonical spec hash; the same name with a different spec fails with `AlreadyExists`.
  Specs are immutable: remove and re-create a job to change it.
- Schedule triggers take five-field cron expressions (numbers, lists, ranges and steps) evaluated in the job's IANA timezone, UTC by default.
- When a schedule fires during a run, `forbid` (default) drops the fire and `queue` defers one.
  An explicit `Run` on a running job fails with `FailedPrecondition`.
- After daemon downtime, `one` (default) fires a single catch-up run and `skip` drops missed fires.
- The run record is persisted before its container is created, so a failure or crash always leaves a record.
  Runs end `succeeded`, `failed`, `timed_out` or `cancelled`, and terminal runs never change.
- Run history is capped per job (10000 by default) and kept when a job is removed, unless the removal asks otherwise.
  After removal, use the original job ID with `ListRuns` or `InspectRun` to read retained history, or with `Remove` and `runs_removal: "remove"` to purge it.
  Names resolve only to live jobs; `Wait` also requires a live job.

## Run containers

Run containers carry the `com.docker.job.id` and `com.docker.job.run-id` labels.
A run container lives as long as its run record: it is removed when history eviction drops the run, or when `Remove` drops the job's runs.
The Jobs API does not proxy logs; read them with the container logs API using the run's `container_id`.
`remove_on_success` and `remove_on_failure` remove the container as soon as the run ends, trading its logs for earlier cleanup; the run record keeps the exit code and error.

## Restarts

Jobs and runs persist through the storage point.
On startup the extension reconciles runs left in flight by an unclean stop before re-arming schedules:
a run whose container exited is resolved with its exit code, a still-running container is re-attached, timeouts are re-armed from the original start, and a run whose start was never recorded is failed.
