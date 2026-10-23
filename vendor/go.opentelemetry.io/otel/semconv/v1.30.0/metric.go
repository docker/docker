// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Code generated from semantic convention specification. DO NOT EDIT.

package semconv

const (
	// AzureCosmosDBClientActiveInstanceCount is the metric conforming to the
	// "azure.cosmosdb.client.active_instance.count" semantic conventions. It
	// represents the number of active client instances.
	// Instrument: updowncounter
	// Unit: {instance}
	// Stability: development
	AzureCosmosDBClientActiveInstanceCountName        = "azure.cosmosdb.client.active_instance.count"
	AzureCosmosDBClientActiveInstanceCountUnit        = "{instance}"
	AzureCosmosDBClientActiveInstanceCountDescription = "Number of active client instances"
	// AzureCosmosDBClientOperationRequestCharge is the metric conforming to the
	// "azure.cosmosdb.client.operation.request_charge" semantic conventions. It
	// represents the [Request units] consumed by the operation.
	//
	// [Request units]: https://learn.microsoft.com/azure/cosmos-db/request-units
	// Instrument: histogram
	// Unit: {request_unit}
	// Stability: development
	AzureCosmosDBClientOperationRequestChargeName        = "azure.cosmosdb.client.operation.request_charge"
	AzureCosmosDBClientOperationRequestChargeUnit        = "{request_unit}"
	AzureCosmosDBClientOperationRequestChargeDescription = "[Request units](https://learn.microsoft.com/azure/cosmos-db/request-units) consumed by the operation"
	// CICDPipelineRunActive is the metric conforming to the
	// "cicd.pipeline.run.active" semantic conventions. It represents the number of
	// pipeline runs currently active in the system by state.
	// Instrument: updowncounter
	// Unit: {run}
	// Stability: development
	CICDPipelineRunActiveName        = "cicd.pipeline.run.active"
	CICDPipelineRunActiveUnit        = "{run}"
	CICDPipelineRunActiveDescription = "The number of pipeline runs currently active in the system by state."
	// CICDPipelineRunDuration is the metric conforming to the
	// "cicd.pipeline.run.duration" semantic conventions. It represents the
	// duration of a pipeline run grouped by pipeline, state and result.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	CICDPipelineRunDurationName        = "cicd.pipeline.run.duration"
	CICDPipelineRunDurationUnit        = "s"
	CICDPipelineRunDurationDescription = "Duration of a pipeline run grouped by pipeline, state and result."
	// CICDPipelineRunErrors is the metric conforming to the
	// "cicd.pipeline.run.errors" semantic conventions. It represents the number of
	// errors encountered in pipeline runs (eg. compile, test failures).
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	CICDPipelineRunErrorsName        = "cicd.pipeline.run.errors"
	CICDPipelineRunErrorsUnit        = "{error}"
	CICDPipelineRunErrorsDescription = "The number of errors encountered in pipeline runs (eg. compile, test failures)."
	// CICDSystemErrors is the metric conforming to the "cicd.system.errors"
	// semantic conventions. It represents the number of errors in a component of
	// the CICD system (eg. controller, scheduler, agent).
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	CICDSystemErrorsName        = "cicd.system.errors"
	CICDSystemErrorsUnit        = "{error}"
	CICDSystemErrorsDescription = "The number of errors in a component of the CICD system (eg. controller, scheduler, agent)."
	// CICDWorkerCount is the metric conforming to the "cicd.worker.count" semantic
	// conventions. It represents the number of workers on the CICD system by
	// state.
	// Instrument: updowncounter
	// Unit: {count}
	// Stability: development
	CICDWorkerCountName        = "cicd.worker.count"
	CICDWorkerCountUnit        = "{count}"
	CICDWorkerCountDescription = "The number of workers on the CICD system by state."
	// ContainerCPUTime is the metric conforming to the "container.cpu.time"
	// semantic conventions. It represents the total CPU time consumed.
	// Instrument: counter
	// Unit: s
	// Stability: development
	ContainerCPUTimeName        = "container.cpu.time"
	ContainerCPUTimeUnit        = "s"
	ContainerCPUTimeDescription = "Total CPU time consumed"
	// ContainerCPUUsage is the metric conforming to the "container.cpu.usage"
	// semantic conventions. It represents the container's CPU usage, measured in
	// cpus. Range from 0 to the number of allocatable CPUs.
	// Instrument: gauge
	// Unit: {cpu}
	// Stability: development
	ContainerCPUUsageName        = "container.cpu.usage"
	ContainerCPUUsageUnit        = "{cpu}"
	ContainerCPUUsageDescription = "Container's CPU usage, measured in cpus. Range from 0 to the number of allocatable CPUs"
	// ContainerDiskIo is the metric conforming to the "container.disk.io" semantic
	// conventions. It represents the disk bytes for the container.
	// Instrument: counter
	// Unit: By
	// Stability: development
	ContainerDiskIoName        = "container.disk.io"
	ContainerDiskIoUnit        = "By"
	ContainerDiskIoDescription = "Disk bytes for the container."
	// ContainerMemoryUsage is the metric conforming to the
	// "container.memory.usage" semantic conventions. It represents the memory
	// usage of the container.
	// Instrument: counter
	// Unit: By
	// Stability: development
	ContainerMemoryUsageName        = "container.memory.usage"
	ContainerMemoryUsageUnit        = "By"
	ContainerMemoryUsageDescription = "Memory usage of the container."
	// ContainerNetworkIo is the metric conforming to the "container.network.io"
	// semantic conventions. It represents the network bytes for the container.
	// Instrument: counter
	// Unit: By
	// Stability: development
	ContainerNetworkIoName        = "container.network.io"
	ContainerNetworkIoUnit        = "By"
	ContainerNetworkIoDescription = "Network bytes for the container."
	// ContainerUptime is the metric conforming to the "container.uptime" semantic
	// conventions. It represents the time the container has been running.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	ContainerUptimeName        = "container.uptime"
	ContainerUptimeUnit        = "s"
	ContainerUptimeDescription = "The time the container has been running"
	// DBClientConnectionCount is the metric conforming to the
	// "db.client.connection.count" semantic conventions. It represents the number
	// of connections that are currently in state described by the `state`
	// attribute.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	DBClientConnectionCountName        = "db.client.connection.count"
	DBClientConnectionCountUnit        = "{connection}"
	DBClientConnectionCountDescription = "The number of connections that are currently in state described by the `state` attribute"
	// DBClientConnectionCreateTime is the metric conforming to the
	// "db.client.connection.create_time" semantic conventions. It represents the
	// time it took to create a new connection.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	DBClientConnectionCreateTimeName        = "db.client.connection.create_time"
	DBClientConnectionCreateTimeUnit        = "s"
	DBClientConnectionCreateTimeDescription = "The time it took to create a new connection"
	// DBClientConnectionIdleMax is the metric conforming to the
	// "db.client.connection.idle.max" semantic conventions. It represents the
	// maximum number of idle open connections allowed.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	DBClientConnectionIdleMaxName        = "db.client.connection.idle.max"
	DBClientConnectionIdleMaxUnit        = "{connection}"
	DBClientConnectionIdleMaxDescription = "The maximum number of idle open connections allowed"
	// DBClientConnectionIdleMin is the metric conforming to the
	// "db.client.connection.idle.min" semantic conventions. It represents the
	// minimum number of idle open connections allowed.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	DBClientConnectionIdleMinName        = "db.client.connection.idle.min"
	DBClientConnectionIdleMinUnit        = "{connection}"
	DBClientConnectionIdleMinDescription = "The minimum number of idle open connections allowed"
	// DBClientConnectionMax is the metric conforming to the
	// "db.client.connection.max" semantic conventions. It represents the maximum
	// number of open connections allowed.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	DBClientConnectionMaxName        = "db.client.connection.max"
	DBClientConnectionMaxUnit        = "{connection}"
	DBClientConnectionMaxDescription = "The maximum number of open connections allowed"
	// DBClientConnectionPendingRequests is the metric conforming to the
	// "db.client.connection.pending_requests" semantic conventions. It represents
	// the number of current pending requests for an open connection.
	// Instrument: updowncounter
	// Unit: {request}
	// Stability: development
	DBClientConnectionPendingRequestsName        = "db.client.connection.pending_requests"
	DBClientConnectionPendingRequestsUnit        = "{request}"
	DBClientConnectionPendingRequestsDescription = "The number of current pending requests for an open connection"
	// DBClientConnectionTimeouts is the metric conforming to the
	// "db.client.connection.timeouts" semantic conventions. It represents the
	// number of connection timeouts that have occurred trying to obtain a
	// connection from the pool.
	// Instrument: counter
	// Unit: {timeout}
	// Stability: development
	DBClientConnectionTimeoutsName        = "db.client.connection.timeouts"
	DBClientConnectionTimeoutsUnit        = "{timeout}"
	DBClientConnectionTimeoutsDescription = "The number of connection timeouts that have occurred trying to obtain a connection from the pool"
	// DBClientConnectionUseTime is the metric conforming to the
	// "db.client.connection.use_time" semantic conventions. It represents the time
	// between borrowing a connection and returning it to the pool.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	DBClientConnectionUseTimeName        = "db.client.connection.use_time"
	DBClientConnectionUseTimeUnit        = "s"
	DBClientConnectionUseTimeDescription = "The time between borrowing a connection and returning it to the pool"
	// DBClientConnectionWaitTime is the metric conforming to the
	// "db.client.connection.wait_time" semantic conventions. It represents the
	// time it took to obtain an open connection from the pool.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	DBClientConnectionWaitTimeName        = "db.client.connection.wait_time"
	DBClientConnectionWaitTimeUnit        = "s"
	DBClientConnectionWaitTimeDescription = "The time it took to obtain an open connection from the pool"
	// DBClientConnectionsCreateTime is the metric conforming to the
	// "db.client.connections.create_time" semantic conventions. It represents the
	// deprecated, use `db.client.connection.create_time` instead. Note: the unit
	// also changed from `ms` to `s`.
	// Instrument: histogram
	// Unit: ms
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.create_time`. Note: the unit also changed from `ms` to `s`.
	DBClientConnectionsCreateTimeName        = "db.client.connections.create_time"
	DBClientConnectionsCreateTimeUnit        = "ms"
	DBClientConnectionsCreateTimeDescription = "Deprecated, use `db.client.connection.create_time` instead. Note: the unit also changed from `ms` to `s`."
	// DBClientConnectionsIdleMax is the metric conforming to the
	// "db.client.connections.idle.max" semantic conventions. It represents the
	// deprecated, use `db.client.connection.idle.max` instead.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.idle.max`.
	DBClientConnectionsIdleMaxName        = "db.client.connections.idle.max"
	DBClientConnectionsIdleMaxUnit        = "{connection}"
	DBClientConnectionsIdleMaxDescription = "Deprecated, use `db.client.connection.idle.max` instead."
	// DBClientConnectionsIdleMin is the metric conforming to the
	// "db.client.connections.idle.min" semantic conventions. It represents the
	// deprecated, use `db.client.connection.idle.min` instead.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.idle.min`.
	DBClientConnectionsIdleMinName        = "db.client.connections.idle.min"
	DBClientConnectionsIdleMinUnit        = "{connection}"
	DBClientConnectionsIdleMinDescription = "Deprecated, use `db.client.connection.idle.min` instead."
	// DBClientConnectionsMax is the metric conforming to the
	// "db.client.connections.max" semantic conventions. It represents the
	// deprecated, use `db.client.connection.max` instead.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.max`.
	DBClientConnectionsMaxName        = "db.client.connections.max"
	DBClientConnectionsMaxUnit        = "{connection}"
	DBClientConnectionsMaxDescription = "Deprecated, use `db.client.connection.max` instead."
	// DBClientConnectionsPendingRequests is the metric conforming to the
	// "db.client.connections.pending_requests" semantic conventions. It represents
	// the deprecated, use `db.client.connection.pending_requests` instead.
	// Instrument: updowncounter
	// Unit: {request}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.pending_requests`.
	DBClientConnectionsPendingRequestsName        = "db.client.connections.pending_requests"
	DBClientConnectionsPendingRequestsUnit        = "{request}"
	DBClientConnectionsPendingRequestsDescription = "Deprecated, use `db.client.connection.pending_requests` instead."
	// DBClientConnectionsTimeouts is the metric conforming to the
	// "db.client.connections.timeouts" semantic conventions. It represents the
	// deprecated, use `db.client.connection.timeouts` instead.
	// Instrument: counter
	// Unit: {timeout}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.timeouts`.
	DBClientConnectionsTimeoutsName        = "db.client.connections.timeouts"
	DBClientConnectionsTimeoutsUnit        = "{timeout}"
	DBClientConnectionsTimeoutsDescription = "Deprecated, use `db.client.connection.timeouts` instead."
	// DBClientConnectionsUsage is the metric conforming to the
	// "db.client.connections.usage" semantic conventions. It represents the
	// deprecated, use `db.client.connection.count` instead.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.count`.
	DBClientConnectionsUsageName        = "db.client.connections.usage"
	DBClientConnectionsUsageUnit        = "{connection}"
	DBClientConnectionsUsageDescription = "Deprecated, use `db.client.connection.count` instead."
	// DBClientConnectionsUseTime is the metric conforming to the
	// "db.client.connections.use_time" semantic conventions. It represents the
	// deprecated, use `db.client.connection.use_time` instead. Note: the unit also
	// changed from `ms` to `s`.
	// Instrument: histogram
	// Unit: ms
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.use_time`. Note: the unit also changed from `ms` to `s`.
	DBClientConnectionsUseTimeName        = "db.client.connections.use_time"
	DBClientConnectionsUseTimeUnit        = "ms"
	DBClientConnectionsUseTimeDescription = "Deprecated, use `db.client.connection.use_time` instead. Note: the unit also changed from `ms` to `s`."
	// DBClientConnectionsWaitTime is the metric conforming to the
	// "db.client.connections.wait_time" semantic conventions. It represents the
	// deprecated, use `db.client.connection.wait_time` instead. Note: the unit
	// also changed from `ms` to `s`.
	// Instrument: histogram
	// Unit: ms
	// Stability: development
	// Deprecated: Replaced by `db.client.connection.wait_time`. Note: the unit also changed from `ms` to `s`.
	DBClientConnectionsWaitTimeName        = "db.client.connections.wait_time"
	DBClientConnectionsWaitTimeUnit        = "ms"
	DBClientConnectionsWaitTimeDescription = "Deprecated, use `db.client.connection.wait_time` instead. Note: the unit also changed from `ms` to `s`."
	// DBClientCosmosDBActiveInstanceCount is the metric conforming to the
	// "db.client.cosmosdb.active_instance.count" semantic conventions. It
	// represents the deprecated, use `azure.cosmosdb.client.active_instance.count`
	//  instead.
	// Instrument: updowncounter
	// Unit: {instance}
	// Stability: development
	// Deprecated: Replaced by `azure.cosmosdb.client.active_instance.count`.
	DBClientCosmosDBActiveInstanceCountName        = "db.client.cosmosdb.active_instance.count"
	DBClientCosmosDBActiveInstanceCountUnit        = "{instance}"
	DBClientCosmosDBActiveInstanceCountDescription = "Deprecated, use `azure.cosmosdb.client.active_instance.count` instead."
	// DBClientCosmosDBOperationRequestCharge is the metric conforming to the
	// "db.client.cosmosdb.operation.request_charge" semantic conventions. It
	// represents the deprecated, use
	// `azure.cosmosdb.client.operation.request_charge` instead.
	// Instrument: histogram
	// Unit: {request_unit}
	// Stability: development
	// Deprecated: Replaced by `azure.cosmosdb.client.operation.request_charge`.
	DBClientCosmosDBOperationRequestChargeName        = "db.client.cosmosdb.operation.request_charge"
	DBClientCosmosDBOperationRequestChargeUnit        = "{request_unit}"
	DBClientCosmosDBOperationRequestChargeDescription = "Deprecated, use `azure.cosmosdb.client.operation.request_charge` instead."
	// DBClientOperationDuration is the metric conforming to the
	// "db.client.operation.duration" semantic conventions. It represents the
	// duration of database client operations.
	// Instrument: histogram
	// Unit: s
	// Stability: release_candidate
	DBClientOperationDurationName        = "db.client.operation.duration"
	DBClientOperationDurationUnit        = "s"
	DBClientOperationDurationDescription = "Duration of database client operations."
	// DBClientResponseReturnedRows is the metric conforming to the
	// "db.client.response.returned_rows" semantic conventions. It represents the
	// actual number of records returned by the database operation.
	// Instrument: histogram
	// Unit: {row}
	// Stability: development
	DBClientResponseReturnedRowsName        = "db.client.response.returned_rows"
	DBClientResponseReturnedRowsUnit        = "{row}"
	DBClientResponseReturnedRowsDescription = "The actual number of records returned by the database operation."
	// DNSLookupDuration is the metric conforming to the "dns.lookup.duration"
	// semantic conventions. It represents the measures the time taken to perform a
	// DNS lookup.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	DNSLookupDurationName        = "dns.lookup.duration"
	DNSLookupDurationUnit        = "s"
	DNSLookupDurationDescription = "Measures the time taken to perform a DNS lookup."
	// FaaSColdstarts is the metric conforming to the "faas.coldstarts" semantic
	// conventions. It represents the number of invocation cold starts.
	// Instrument: counter
	// Unit: {coldstart}
	// Stability: development
	FaaSColdstartsName        = "faas.coldstarts"
	FaaSColdstartsUnit        = "{coldstart}"
	FaaSColdstartsDescription = "Number of invocation cold starts"
	// FaaSCPUUsage is the metric conforming to the "faas.cpu_usage" semantic
	// conventions. It represents the distribution of CPU usage per invocation.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	FaaSCPUUsageName        = "faas.cpu_usage"
	FaaSCPUUsageUnit        = "s"
	FaaSCPUUsageDescription = "Distribution of CPU usage per invocation"
	// FaaSErrors is the metric conforming to the "faas.errors" semantic
	// conventions. It represents the number of invocation errors.
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	FaaSErrorsName        = "faas.errors"
	FaaSErrorsUnit        = "{error}"
	FaaSErrorsDescription = "Number of invocation errors"
	// FaaSInitDuration is the metric conforming to the "faas.init_duration"
	// semantic conventions. It represents the measures the duration of the
	// function's initialization, such as a cold start.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	FaaSInitDurationName        = "faas.init_duration"
	FaaSInitDurationUnit        = "s"
	FaaSInitDurationDescription = "Measures the duration of the function's initialization, such as a cold start"
	// FaaSInvocations is the metric conforming to the "faas.invocations" semantic
	// conventions. It represents the number of successful invocations.
	// Instrument: counter
	// Unit: {invocation}
	// Stability: development
	FaaSInvocationsName        = "faas.invocations"
	FaaSInvocationsUnit        = "{invocation}"
	FaaSInvocationsDescription = "Number of successful invocations"
	// FaaSInvokeDuration is the metric conforming to the "faas.invoke_duration"
	// semantic conventions. It represents the measures the duration of the
	// function's logic execution.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	FaaSInvokeDurationName        = "faas.invoke_duration"
	FaaSInvokeDurationUnit        = "s"
	FaaSInvokeDurationDescription = "Measures the duration of the function's logic execution"
	// FaaSMemUsage is the metric conforming to the "faas.mem_usage" semantic
	// conventions. It represents the distribution of max memory usage per
	// invocation.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	FaaSMemUsageName        = "faas.mem_usage"
	FaaSMemUsageUnit        = "By"
	FaaSMemUsageDescription = "Distribution of max memory usage per invocation"
	// FaaSNetIo is the metric conforming to the "faas.net_io" semantic
	// conventions. It represents the distribution of net I/O usage per invocation.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	FaaSNetIoName        = "faas.net_io"
	FaaSNetIoUnit        = "By"
	FaaSNetIoDescription = "Distribution of net I/O usage per invocation"
	// FaaSTimeouts is the metric conforming to the "faas.timeouts" semantic
	// conventions. It represents the number of invocation timeouts.
	// Instrument: counter
	// Unit: {timeout}
	// Stability: development
	FaaSTimeoutsName        = "faas.timeouts"
	FaaSTimeoutsUnit        = "{timeout}"
	FaaSTimeoutsDescription = "Number of invocation timeouts"
	// GenAIClientOperationDuration is the metric conforming to the
	// "gen_ai.client.operation.duration" semantic conventions. It represents the
	// genAI operation duration.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	GenAIClientOperationDurationName        = "gen_ai.client.operation.duration"
	GenAIClientOperationDurationUnit        = "s"
	GenAIClientOperationDurationDescription = "GenAI operation duration"
	// GenAIClientTokenUsage is the metric conforming to the
	// "gen_ai.client.token.usage" semantic conventions. It represents the measures
	// number of input and output tokens used.
	// Instrument: histogram
	// Unit: {token}
	// Stability: development
	GenAIClientTokenUsageName        = "gen_ai.client.token.usage"
	GenAIClientTokenUsageUnit        = "{token}"
	GenAIClientTokenUsageDescription = "Measures number of input and output tokens used"
	// GenAIServerRequestDuration is the metric conforming to the
	// "gen_ai.server.request.duration" semantic conventions. It represents the
	// generative AI server request duration such as time-to-last byte or last
	// output token.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	GenAIServerRequestDurationName        = "gen_ai.server.request.duration"
	GenAIServerRequestDurationUnit        = "s"
	GenAIServerRequestDurationDescription = "Generative AI server request duration such as time-to-last byte or last output token"
	// GenAIServerTimePerOutputToken is the metric conforming to the
	// "gen_ai.server.time_per_output_token" semantic conventions. It represents
	// the time per output token generated after the first token for successful
	// responses.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	GenAIServerTimePerOutputTokenName        = "gen_ai.server.time_per_output_token"
	GenAIServerTimePerOutputTokenUnit        = "s"
	GenAIServerTimePerOutputTokenDescription = "Time per output token generated after the first token for successful responses"
	// GenAIServerTimeToFirstToken is the metric conforming to the
	// "gen_ai.server.time_to_first_token" semantic conventions. It represents the
	// time to generate first token for successful responses.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	GenAIServerTimeToFirstTokenName        = "gen_ai.server.time_to_first_token"
	GenAIServerTimeToFirstTokenUnit        = "s"
	GenAIServerTimeToFirstTokenDescription = "Time to generate first token for successful responses"
	// GoConfigGogc is the metric conforming to the "go.config.gogc" semantic
	// conventions. It represents the heap size target percentage configured by the
	// user, otherwise 100.
	// Instrument: updowncounter
	// Unit: %
	// Stability: development
	GoConfigGogcName        = "go.config.gogc"
	GoConfigGogcUnit        = "%"
	GoConfigGogcDescription = "Heap size target percentage configured by the user, otherwise 100."
	// GoGoroutineCount is the metric conforming to the "go.goroutine.count"
	// semantic conventions. It represents the count of live goroutines.
	// Instrument: updowncounter
	// Unit: {goroutine}
	// Stability: development
	GoGoroutineCountName        = "go.goroutine.count"
	GoGoroutineCountUnit        = "{goroutine}"
	GoGoroutineCountDescription = "Count of live goroutines."
	// GoMemoryAllocated is the metric conforming to the "go.memory.allocated"
	// semantic conventions. It represents the memory allocated to the heap by the
	// application.
	// Instrument: counter
	// Unit: By
	// Stability: development
	GoMemoryAllocatedName        = "go.memory.allocated"
	GoMemoryAllocatedUnit        = "By"
	GoMemoryAllocatedDescription = "Memory allocated to the heap by the application."
	// GoMemoryAllocations is the metric conforming to the "go.memory.allocations"
	// semantic conventions. It represents the count of allocations to the heap by
	// the application.
	// Instrument: counter
	// Unit: {allocation}
	// Stability: development
	GoMemoryAllocationsName        = "go.memory.allocations"
	GoMemoryAllocationsUnit        = "{allocation}"
	GoMemoryAllocationsDescription = "Count of allocations to the heap by the application."
	// GoMemoryGCGoal is the metric conforming to the "go.memory.gc.goal" semantic
	// conventions. It represents the heap size target for the end of the GC cycle.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	GoMemoryGCGoalName        = "go.memory.gc.goal"
	GoMemoryGCGoalUnit        = "By"
	GoMemoryGCGoalDescription = "Heap size target for the end of the GC cycle."
	// GoMemoryLimit is the metric conforming to the "go.memory.limit" semantic
	// conventions. It represents the go runtime memory limit configured by the
	// user, if a limit exists.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	GoMemoryLimitName        = "go.memory.limit"
	GoMemoryLimitUnit        = "By"
	GoMemoryLimitDescription = "Go runtime memory limit configured by the user, if a limit exists."
	// GoMemoryUsed is the metric conforming to the "go.memory.used" semantic
	// conventions. It represents the memory used by the Go runtime.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	GoMemoryUsedName        = "go.memory.used"
	GoMemoryUsedUnit        = "By"
	GoMemoryUsedDescription = "Memory used by the Go runtime."
	// GoProcessorLimit is the metric conforming to the "go.processor.limit"
	// semantic conventions. It represents the number of OS threads that can
	// execute user-level Go code simultaneously.
	// Instrument: updowncounter
	// Unit: {thread}
	// Stability: development
	GoProcessorLimitName        = "go.processor.limit"
	GoProcessorLimitUnit        = "{thread}"
	GoProcessorLimitDescription = "The number of OS threads that can execute user-level Go code simultaneously."
	// GoScheduleDuration is the metric conforming to the "go.schedule.duration"
	// semantic conventions. It represents the time goroutines have spent in the
	// scheduler in a runnable state before actually running.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	GoScheduleDurationName        = "go.schedule.duration"
	GoScheduleDurationUnit        = "s"
	GoScheduleDurationDescription = "The time goroutines have spent in the scheduler in a runnable state before actually running."
	// HTTPClientActiveRequests is the metric conforming to the
	// "http.client.active_requests" semantic conventions. It represents the number
	// of active HTTP requests.
	// Instrument: updowncounter
	// Unit: {request}
	// Stability: development
	HTTPClientActiveRequestsName        = "http.client.active_requests"
	HTTPClientActiveRequestsUnit        = "{request}"
	HTTPClientActiveRequestsDescription = "Number of active HTTP requests."
	// HTTPClientConnectionDuration is the metric conforming to the
	// "http.client.connection.duration" semantic conventions. It represents the
	// duration of the successfully established outbound HTTP connections.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	HTTPClientConnectionDurationName        = "http.client.connection.duration"
	HTTPClientConnectionDurationUnit        = "s"
	HTTPClientConnectionDurationDescription = "The duration of the successfully established outbound HTTP connections."
	// HTTPClientOpenConnections is the metric conforming to the
	// "http.client.open_connections" semantic conventions. It represents the
	// number of outbound HTTP connections that are currently active or idle on the
	// client.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	HTTPClientOpenConnectionsName        = "http.client.open_connections"
	HTTPClientOpenConnectionsUnit        = "{connection}"
	HTTPClientOpenConnectionsDescription = "Number of outbound HTTP connections that are currently active or idle on the client."
	// HTTPClientRequestBodySize is the metric conforming to the
	// "http.client.request.body.size" semantic conventions. It represents the size
	// of HTTP client request bodies.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	HTTPClientRequestBodySizeName        = "http.client.request.body.size"
	HTTPClientRequestBodySizeUnit        = "By"
	HTTPClientRequestBodySizeDescription = "Size of HTTP client request bodies."
	// HTTPClientRequestDuration is the metric conforming to the
	// "http.client.request.duration" semantic conventions. It represents the
	// duration of HTTP client requests.
	// Instrument: histogram
	// Unit: s
	// Stability: stable
	HTTPClientRequestDurationName        = "http.client.request.duration"
	HTTPClientRequestDurationUnit        = "s"
	HTTPClientRequestDurationDescription = "Duration of HTTP client requests."
	// HTTPClientResponseBodySize is the metric conforming to the
	// "http.client.response.body.size" semantic conventions. It represents the
	// size of HTTP client response bodies.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	HTTPClientResponseBodySizeName        = "http.client.response.body.size"
	HTTPClientResponseBodySizeUnit        = "By"
	HTTPClientResponseBodySizeDescription = "Size of HTTP client response bodies."
	// HTTPServerActiveRequests is the metric conforming to the
	// "http.server.active_requests" semantic conventions. It represents the number
	// of active HTTP server requests.
	// Instrument: updowncounter
	// Unit: {request}
	// Stability: development
	HTTPServerActiveRequestsName        = "http.server.active_requests"
	HTTPServerActiveRequestsUnit        = "{request}"
	HTTPServerActiveRequestsDescription = "Number of active HTTP server requests."
	// HTTPServerRequestBodySize is the metric conforming to the
	// "http.server.request.body.size" semantic conventions. It represents the size
	// of HTTP server request bodies.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	HTTPServerRequestBodySizeName        = "http.server.request.body.size"
	HTTPServerRequestBodySizeUnit        = "By"
	HTTPServerRequestBodySizeDescription = "Size of HTTP server request bodies."
	// HTTPServerRequestDuration is the metric conforming to the
	// "http.server.request.duration" semantic conventions. It represents the
	// duration of HTTP server requests.
	// Instrument: histogram
	// Unit: s
	// Stability: stable
	HTTPServerRequestDurationName        = "http.server.request.duration"
	HTTPServerRequestDurationUnit        = "s"
	HTTPServerRequestDurationDescription = "Duration of HTTP server requests."
	// HTTPServerResponseBodySize is the metric conforming to the
	// "http.server.response.body.size" semantic conventions. It represents the
	// size of HTTP server response bodies.
	// Instrument: histogram
	// Unit: By
	// Stability: development
	HTTPServerResponseBodySizeName        = "http.server.response.body.size"
	HTTPServerResponseBodySizeUnit        = "By"
	HTTPServerResponseBodySizeDescription = "Size of HTTP server response bodies."
	// HwEnergy is the metric conforming to the "hw.energy" semantic conventions.
	// It represents the energy consumed by the component.
	// Instrument: counter
	// Unit: J
	// Stability: development
	HwEnergyName        = "hw.energy"
	HwEnergyUnit        = "J"
	HwEnergyDescription = "Energy consumed by the component"
	// HwErrors is the metric conforming to the "hw.errors" semantic conventions.
	// It represents the number of errors encountered by the component.
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	HwErrorsName        = "hw.errors"
	HwErrorsUnit        = "{error}"
	HwErrorsDescription = "Number of errors encountered by the component"
	// HwPower is the metric conforming to the "hw.power" semantic conventions. It
	// represents the instantaneous power consumed by the component.
	// Instrument: gauge
	// Unit: W
	// Stability: development
	HwPowerName        = "hw.power"
	HwPowerUnit        = "W"
	HwPowerDescription = "Instantaneous power consumed by the component"
	// HwStatus is the metric conforming to the "hw.status" semantic conventions.
	// It represents the operational status: `1` (true) or `0` (false) for each of
	// the possible states.
	// Instrument: updowncounter
	// Unit: 1
	// Stability: development
	HwStatusName        = "hw.status"
	HwStatusUnit        = "1"
	HwStatusDescription = "Operational status: `1` (true) or `0` (false) for each of the possible states"
	// K8SCronJobActiveJobs is the metric conforming to the
	// "k8s.cronjob.active_jobs" semantic conventions. It represents the number of
	// actively running jobs for a cronjob.
	// Instrument: updowncounter
	// Unit: {job}
	// Stability: development
	K8SCronJobActiveJobsName        = "k8s.cronjob.active_jobs"
	K8SCronJobActiveJobsUnit        = "{job}"
	K8SCronJobActiveJobsDescription = "The number of actively running jobs for a cronjob"
	// K8SDaemonSetCurrentScheduledNodes is the metric conforming to the
	// "k8s.daemonset.current_scheduled_nodes" semantic conventions. It represents
	// the number of nodes that are running at least 1 daemon pod and are supposed
	// to run the daemon pod.
	// Instrument: updowncounter
	// Unit: {node}
	// Stability: development
	K8SDaemonSetCurrentScheduledNodesName        = "k8s.daemonset.current_scheduled_nodes"
	K8SDaemonSetCurrentScheduledNodesUnit        = "{node}"
	K8SDaemonSetCurrentScheduledNodesDescription = "Number of nodes that are running at least 1 daemon pod and are supposed to run the daemon pod"
	// K8SDaemonSetDesiredScheduledNodes is the metric conforming to the
	// "k8s.daemonset.desired_scheduled_nodes" semantic conventions. It represents
	// the number of nodes that should be running the daemon pod (including nodes
	// currently running the daemon pod).
	// Instrument: updowncounter
	// Unit: {node}
	// Stability: development
	K8SDaemonSetDesiredScheduledNodesName        = "k8s.daemonset.desired_scheduled_nodes"
	K8SDaemonSetDesiredScheduledNodesUnit        = "{node}"
	K8SDaemonSetDesiredScheduledNodesDescription = "Number of nodes that should be running the daemon pod (including nodes currently running the daemon pod)"
	// K8SDaemonSetMisscheduledNodes is the metric conforming to the
	// "k8s.daemonset.misscheduled_nodes" semantic conventions. It represents the
	// number of nodes that are running the daemon pod, but are not supposed to run
	// the daemon pod.
	// Instrument: updowncounter
	// Unit: {node}
	// Stability: development
	K8SDaemonSetMisscheduledNodesName        = "k8s.daemonset.misscheduled_nodes"
	K8SDaemonSetMisscheduledNodesUnit        = "{node}"
	K8SDaemonSetMisscheduledNodesDescription = "Number of nodes that are running the daemon pod, but are not supposed to run the daemon pod"
	// K8SDaemonSetReadyNodes is the metric conforming to the
	// "k8s.daemonset.ready_nodes" semantic conventions. It represents the number
	// of nodes that should be running the daemon pod and have one or more of the
	// daemon pod running and ready.
	// Instrument: updowncounter
	// Unit: {node}
	// Stability: development
	K8SDaemonSetReadyNodesName        = "k8s.daemonset.ready_nodes"
	K8SDaemonSetReadyNodesUnit        = "{node}"
	K8SDaemonSetReadyNodesDescription = "Number of nodes that should be running the daemon pod and have one or more of the daemon pod running and ready"
	// K8SDeploymentAvailablePods is the metric conforming to the
	// "k8s.deployment.available_pods" semantic conventions. It represents the
	// total number of available replica pods (ready for at least minReadySeconds)
	// targeted by this deployment.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SDeploymentAvailablePodsName        = "k8s.deployment.available_pods"
	K8SDeploymentAvailablePodsUnit        = "{pod}"
	K8SDeploymentAvailablePodsDescription = "Total number of available replica pods (ready for at least minReadySeconds) targeted by this deployment"
	// K8SDeploymentDesiredPods is the metric conforming to the
	// "k8s.deployment.desired_pods" semantic conventions. It represents the number
	// of desired replica pods in this deployment.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SDeploymentDesiredPodsName        = "k8s.deployment.desired_pods"
	K8SDeploymentDesiredPodsUnit        = "{pod}"
	K8SDeploymentDesiredPodsDescription = "Number of desired replica pods in this deployment"
	// K8SHpaCurrentPods is the metric conforming to the "k8s.hpa.current_pods"
	// semantic conventions. It represents the current number of replica pods
	// managed by this horizontal pod autoscaler, as last seen by the autoscaler.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SHpaCurrentPodsName        = "k8s.hpa.current_pods"
	K8SHpaCurrentPodsUnit        = "{pod}"
	K8SHpaCurrentPodsDescription = "Current number of replica pods managed by this horizontal pod autoscaler, as last seen by the autoscaler"
	// K8SHpaDesiredPods is the metric conforming to the "k8s.hpa.desired_pods"
	// semantic conventions. It represents the desired number of replica pods
	// managed by this horizontal pod autoscaler, as last calculated by the
	// autoscaler.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SHpaDesiredPodsName        = "k8s.hpa.desired_pods"
	K8SHpaDesiredPodsUnit        = "{pod}"
	K8SHpaDesiredPodsDescription = "Desired number of replica pods managed by this horizontal pod autoscaler, as last calculated by the autoscaler"
	// K8SHpaMaxPods is the metric conforming to the "k8s.hpa.max_pods" semantic
	// conventions. It represents the upper limit for the number of replica pods to
	// which the autoscaler can scale up.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SHpaMaxPodsName        = "k8s.hpa.max_pods"
	K8SHpaMaxPodsUnit        = "{pod}"
	K8SHpaMaxPodsDescription = "The upper limit for the number of replica pods to which the autoscaler can scale up"
	// K8SHpaMinPods is the metric conforming to the "k8s.hpa.min_pods" semantic
	// conventions. It represents the lower limit for the number of replica pods to
	// which the autoscaler can scale down.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SHpaMinPodsName        = "k8s.hpa.min_pods"
	K8SHpaMinPodsUnit        = "{pod}"
	K8SHpaMinPodsDescription = "The lower limit for the number of replica pods to which the autoscaler can scale down"
	// K8SJobActivePods is the metric conforming to the "k8s.job.active_pods"
	// semantic conventions. It represents the number of pending and actively
	// running pods for a job.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SJobActivePodsName        = "k8s.job.active_pods"
	K8SJobActivePodsUnit        = "{pod}"
	K8SJobActivePodsDescription = "The number of pending and actively running pods for a job"
	// K8SJobDesiredSuccessfulPods is the metric conforming to the
	// "k8s.job.desired_successful_pods" semantic conventions. It represents the
	// desired number of successfully finished pods the job should be run with.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SJobDesiredSuccessfulPodsName        = "k8s.job.desired_successful_pods"
	K8SJobDesiredSuccessfulPodsUnit        = "{pod}"
	K8SJobDesiredSuccessfulPodsDescription = "The desired number of successfully finished pods the job should be run with"
	// K8SJobFailedPods is the metric conforming to the "k8s.job.failed_pods"
	// semantic conventions. It represents the number of pods which reached phase
	// Failed for a job.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SJobFailedPodsName        = "k8s.job.failed_pods"
	K8SJobFailedPodsUnit        = "{pod}"
	K8SJobFailedPodsDescription = "The number of pods which reached phase Failed for a job"
	// K8SJobMaxParallelPods is the metric conforming to the
	// "k8s.job.max_parallel_pods" semantic conventions. It represents the max
	// desired number of pods the job should run at any given time.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SJobMaxParallelPodsName        = "k8s.job.max_parallel_pods"
	K8SJobMaxParallelPodsUnit        = "{pod}"
	K8SJobMaxParallelPodsDescription = "The max desired number of pods the job should run at any given time"
	// K8SJobSuccessfulPods is the metric conforming to the
	// "k8s.job.successful_pods" semantic conventions. It represents the number of
	// pods which reached phase Succeeded for a job.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SJobSuccessfulPodsName        = "k8s.job.successful_pods"
	K8SJobSuccessfulPodsUnit        = "{pod}"
	K8SJobSuccessfulPodsDescription = "The number of pods which reached phase Succeeded for a job"
	// K8SNamespacePhase is the metric conforming to the "k8s.namespace.phase"
	// semantic conventions. It represents the describes number of K8s namespaces
	// that are currently in a given phase.
	// Instrument: updowncounter
	// Unit: {namespace}
	// Stability: development
	K8SNamespacePhaseName        = "k8s.namespace.phase"
	K8SNamespacePhaseUnit        = "{namespace}"
	K8SNamespacePhaseDescription = "Describes number of K8s namespaces that are currently in a given phase."
	// K8SNodeCPUTime is the metric conforming to the "k8s.node.cpu.time" semantic
	// conventions. It represents the total CPU time consumed.
	// Instrument: counter
	// Unit: s
	// Stability: development
	K8SNodeCPUTimeName        = "k8s.node.cpu.time"
	K8SNodeCPUTimeUnit        = "s"
	K8SNodeCPUTimeDescription = "Total CPU time consumed"
	// K8SNodeCPUUsage is the metric conforming to the "k8s.node.cpu.usage"
	// semantic conventions. It represents the node's CPU usage, measured in cpus.
	// Range from 0 to the number of allocatable CPUs.
	// Instrument: gauge
	// Unit: {cpu}
	// Stability: development
	K8SNodeCPUUsageName        = "k8s.node.cpu.usage"
	K8SNodeCPUUsageUnit        = "{cpu}"
	K8SNodeCPUUsageDescription = "Node's CPU usage, measured in cpus. Range from 0 to the number of allocatable CPUs"
	// K8SNodeMemoryUsage is the metric conforming to the "k8s.node.memory.usage"
	// semantic conventions. It represents the memory usage of the Node.
	// Instrument: gauge
	// Unit: By
	// Stability: development
	K8SNodeMemoryUsageName        = "k8s.node.memory.usage"
	K8SNodeMemoryUsageUnit        = "By"
	K8SNodeMemoryUsageDescription = "Memory usage of the Node"
	// K8SNodeNetworkErrors is the metric conforming to the
	// "k8s.node.network.errors" semantic conventions. It represents the node
	// network errors.
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	K8SNodeNetworkErrorsName        = "k8s.node.network.errors"
	K8SNodeNetworkErrorsUnit        = "{error}"
	K8SNodeNetworkErrorsDescription = "Node network errors"
	// K8SNodeNetworkIo is the metric conforming to the "k8s.node.network.io"
	// semantic conventions. It represents the network bytes for the Node.
	// Instrument: counter
	// Unit: By
	// Stability: development
	K8SNodeNetworkIoName        = "k8s.node.network.io"
	K8SNodeNetworkIoUnit        = "By"
	K8SNodeNetworkIoDescription = "Network bytes for the Node"
	// K8SNodeUptime is the metric conforming to the "k8s.node.uptime" semantic
	// conventions. It represents the time the Node has been running.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	K8SNodeUptimeName        = "k8s.node.uptime"
	K8SNodeUptimeUnit        = "s"
	K8SNodeUptimeDescription = "The time the Node has been running"
	// K8SPodCPUTime is the metric conforming to the "k8s.pod.cpu.time" semantic
	// conventions. It represents the total CPU time consumed.
	// Instrument: counter
	// Unit: s
	// Stability: development
	K8SPodCPUTimeName        = "k8s.pod.cpu.time"
	K8SPodCPUTimeUnit        = "s"
	K8SPodCPUTimeDescription = "Total CPU time consumed"
	// K8SPodCPUUsage is the metric conforming to the "k8s.pod.cpu.usage" semantic
	// conventions. It represents the pod's CPU usage, measured in cpus. Range from
	// 0 to the number of allocatable CPUs.
	// Instrument: gauge
	// Unit: {cpu}
	// Stability: development
	K8SPodCPUUsageName        = "k8s.pod.cpu.usage"
	K8SPodCPUUsageUnit        = "{cpu}"
	K8SPodCPUUsageDescription = "Pod's CPU usage, measured in cpus. Range from 0 to the number of allocatable CPUs"
	// K8SPodMemoryUsage is the metric conforming to the "k8s.pod.memory.usage"
	// semantic conventions. It represents the memory usage of the Pod.
	// Instrument: gauge
	// Unit: By
	// Stability: development
	K8SPodMemoryUsageName        = "k8s.pod.memory.usage"
	K8SPodMemoryUsageUnit        = "By"
	K8SPodMemoryUsageDescription = "Memory usage of the Pod"
	// K8SPodNetworkErrors is the metric conforming to the "k8s.pod.network.errors"
	// semantic conventions. It represents the pod network errors.
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	K8SPodNetworkErrorsName        = "k8s.pod.network.errors"
	K8SPodNetworkErrorsUnit        = "{error}"
	K8SPodNetworkErrorsDescription = "Pod network errors"
	// K8SPodNetworkIo is the metric conforming to the "k8s.pod.network.io"
	// semantic conventions. It represents the network bytes for the Pod.
	// Instrument: counter
	// Unit: By
	// Stability: development
	K8SPodNetworkIoName        = "k8s.pod.network.io"
	K8SPodNetworkIoUnit        = "By"
	K8SPodNetworkIoDescription = "Network bytes for the Pod"
	// K8SPodUptime is the metric conforming to the "k8s.pod.uptime" semantic
	// conventions. It represents the time the Pod has been running.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	K8SPodUptimeName        = "k8s.pod.uptime"
	K8SPodUptimeUnit        = "s"
	K8SPodUptimeDescription = "The time the Pod has been running"
	// K8SReplicaSetAvailablePods is the metric conforming to the
	// "k8s.replicaset.available_pods" semantic conventions. It represents the
	// total number of available replica pods (ready for at least minReadySeconds)
	// targeted by this replicaset.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SReplicaSetAvailablePodsName        = "k8s.replicaset.available_pods"
	K8SReplicaSetAvailablePodsUnit        = "{pod}"
	K8SReplicaSetAvailablePodsDescription = "Total number of available replica pods (ready for at least minReadySeconds) targeted by this replicaset"
	// K8SReplicaSetDesiredPods is the metric conforming to the
	// "k8s.replicaset.desired_pods" semantic conventions. It represents the number
	// of desired replica pods in this replicaset.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SReplicaSetDesiredPodsName        = "k8s.replicaset.desired_pods"
	K8SReplicaSetDesiredPodsUnit        = "{pod}"
	K8SReplicaSetDesiredPodsDescription = "Number of desired replica pods in this replicaset"
	// K8SReplicationControllerAvailablePods is the metric conforming to the
	// "k8s.replication_controller.available_pods" semantic conventions. It
	// represents the total number of available replica pods (ready for at least
	// minReadySeconds) targeted by this replication controller.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SReplicationControllerAvailablePodsName        = "k8s.replication_controller.available_pods"
	K8SReplicationControllerAvailablePodsUnit        = "{pod}"
	K8SReplicationControllerAvailablePodsDescription = "Total number of available replica pods (ready for at least minReadySeconds) targeted by this replication controller"
	// K8SReplicationControllerDesiredPods is the metric conforming to the
	// "k8s.replication_controller.desired_pods" semantic conventions. It
	// represents the number of desired replica pods in this replication
	// controller.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SReplicationControllerDesiredPodsName        = "k8s.replication_controller.desired_pods"
	K8SReplicationControllerDesiredPodsUnit        = "{pod}"
	K8SReplicationControllerDesiredPodsDescription = "Number of desired replica pods in this replication controller"
	// K8SStatefulSetCurrentPods is the metric conforming to the
	// "k8s.statefulset.current_pods" semantic conventions. It represents the
	// number of replica pods created by the statefulset controller from the
	// statefulset version indicated by currentRevision.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SStatefulSetCurrentPodsName        = "k8s.statefulset.current_pods"
	K8SStatefulSetCurrentPodsUnit        = "{pod}"
	K8SStatefulSetCurrentPodsDescription = "The number of replica pods created by the statefulset controller from the statefulset version indicated by currentRevision"
	// K8SStatefulSetDesiredPods is the metric conforming to the
	// "k8s.statefulset.desired_pods" semantic conventions. It represents the
	// number of desired replica pods in this statefulset.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SStatefulSetDesiredPodsName        = "k8s.statefulset.desired_pods"
	K8SStatefulSetDesiredPodsUnit        = "{pod}"
	K8SStatefulSetDesiredPodsDescription = "Number of desired replica pods in this statefulset"
	// K8SStatefulSetReadyPods is the metric conforming to the
	// "k8s.statefulset.ready_pods" semantic conventions. It represents the number
	// of replica pods created for this statefulset with a Ready Condition.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SStatefulSetReadyPodsName        = "k8s.statefulset.ready_pods"
	K8SStatefulSetReadyPodsUnit        = "{pod}"
	K8SStatefulSetReadyPodsDescription = "The number of replica pods created for this statefulset with a Ready Condition"
	// K8SStatefulSetUpdatedPods is the metric conforming to the
	// "k8s.statefulset.updated_pods" semantic conventions. It represents the
	// number of replica pods created by the statefulset controller from the
	// statefulset version indicated by updateRevision.
	// Instrument: updowncounter
	// Unit: {pod}
	// Stability: development
	K8SStatefulSetUpdatedPodsName        = "k8s.statefulset.updated_pods"
	K8SStatefulSetUpdatedPodsUnit        = "{pod}"
	K8SStatefulSetUpdatedPodsDescription = "Number of replica pods created by the statefulset controller from the statefulset version indicated by updateRevision"
	// KestrelActiveConnections is the metric conforming to the
	// "kestrel.active_connections" semantic conventions. It represents the number
	// of connections that are currently active on the server.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: stable
	KestrelActiveConnectionsName        = "kestrel.active_connections"
	KestrelActiveConnectionsUnit        = "{connection}"
	KestrelActiveConnectionsDescription = "Number of connections that are currently active on the server."
	// KestrelActiveTLSHandshakes is the metric conforming to the
	// "kestrel.active_tls_handshakes" semantic conventions. It represents the
	// number of TLS handshakes that are currently in progress on the server.
	// Instrument: updowncounter
	// Unit: {handshake}
	// Stability: stable
	KestrelActiveTLSHandshakesName        = "kestrel.active_tls_handshakes"
	KestrelActiveTLSHandshakesUnit        = "{handshake}"
	KestrelActiveTLSHandshakesDescription = "Number of TLS handshakes that are currently in progress on the server."
	// KestrelConnectionDuration is the metric conforming to the
	// "kestrel.connection.duration" semantic conventions. It represents the
	// duration of connections on the server.
	// Instrument: histogram
	// Unit: s
	// Stability: stable
	KestrelConnectionDurationName        = "kestrel.connection.duration"
	KestrelConnectionDurationUnit        = "s"
	KestrelConnectionDurationDescription = "The duration of connections on the server."
	// KestrelQueuedConnections is the metric conforming to the
	// "kestrel.queued_connections" semantic conventions. It represents the number
	// of connections that are currently queued and are waiting to start.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: stable
	KestrelQueuedConnectionsName        = "kestrel.queued_connections"
	KestrelQueuedConnectionsUnit        = "{connection}"
	KestrelQueuedConnectionsDescription = "Number of connections that are currently queued and are waiting to start."
	// KestrelQueuedRequests is the metric conforming to the
	// "kestrel.queued_requests" semantic conventions. It represents the number of
	// HTTP requests on multiplexed connections (HTTP/2 and HTTP/3) that are
	// currently queued and are waiting to start.
	// Instrument: updowncounter
	// Unit: {request}
	// Stability: stable
	KestrelQueuedRequestsName        = "kestrel.queued_requests"
	KestrelQueuedRequestsUnit        = "{request}"
	KestrelQueuedRequestsDescription = "Number of HTTP requests on multiplexed connections (HTTP/2 and HTTP/3) that are currently queued and are waiting to start."
	// KestrelRejectedConnections is the metric conforming to the
	// "kestrel.rejected_connections" semantic conventions. It represents the
	// number of connections rejected by the server.
	// Instrument: counter
	// Unit: {connection}
	// Stability: stable
	KestrelRejectedConnectionsName        = "kestrel.rejected_connections"
	KestrelRejectedConnectionsUnit        = "{connection}"
	KestrelRejectedConnectionsDescription = "Number of connections rejected by the server."
	// KestrelTLSHandshakeDuration is the metric conforming to the
	// "kestrel.tls_handshake.duration" semantic conventions. It represents the
	// duration of TLS handshakes on the server.
	// Instrument: histogram
	// Unit: s
	// Stability: stable
	KestrelTLSHandshakeDurationName        = "kestrel.tls_handshake.duration"
	KestrelTLSHandshakeDurationUnit        = "s"
	KestrelTLSHandshakeDurationDescription = "The duration of TLS handshakes on the server."
	// KestrelUpgradedConnections is the metric conforming to the
	// "kestrel.upgraded_connections" semantic conventions. It represents the
	// number of connections that are currently upgraded (WebSockets). .
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: stable
	KestrelUpgradedConnectionsName        = "kestrel.upgraded_connections"
	KestrelUpgradedConnectionsUnit        = "{connection}"
	KestrelUpgradedConnectionsDescription = "Number of connections that are currently upgraded (WebSockets). ."
	// MessagingClientConsumedMessages is the metric conforming to the
	// "messaging.client.consumed.messages" semantic conventions. It represents the
	// number of messages that were delivered to the application.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	MessagingClientConsumedMessagesName        = "messaging.client.consumed.messages"
	MessagingClientConsumedMessagesUnit        = "{message}"
	MessagingClientConsumedMessagesDescription = "Number of messages that were delivered to the application."
	// MessagingClientOperationDuration is the metric conforming to the
	// "messaging.client.operation.duration" semantic conventions. It represents
	// the duration of messaging operation initiated by a producer or consumer
	// client.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	MessagingClientOperationDurationName        = "messaging.client.operation.duration"
	MessagingClientOperationDurationUnit        = "s"
	MessagingClientOperationDurationDescription = "Duration of messaging operation initiated by a producer or consumer client."
	// MessagingClientPublishedMessages is the metric conforming to the
	// "messaging.client.published.messages" semantic conventions. It represents
	// the deprecated. Use `messaging.client.sent.messages` instead.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	// Deprecated: Replaced by `messaging.client.sent.messages`.
	MessagingClientPublishedMessagesName        = "messaging.client.published.messages"
	MessagingClientPublishedMessagesUnit        = "{message}"
	MessagingClientPublishedMessagesDescription = "Deprecated. Use `messaging.client.sent.messages` instead."
	// MessagingClientSentMessages is the metric conforming to the
	// "messaging.client.sent.messages" semantic conventions. It represents the
	// number of messages producer attempted to send to the broker.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	MessagingClientSentMessagesName        = "messaging.client.sent.messages"
	MessagingClientSentMessagesUnit        = "{message}"
	MessagingClientSentMessagesDescription = "Number of messages producer attempted to send to the broker."
	// MessagingProcessDuration is the metric conforming to the
	// "messaging.process.duration" semantic conventions. It represents the
	// duration of processing operation.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	MessagingProcessDurationName        = "messaging.process.duration"
	MessagingProcessDurationUnit        = "s"
	MessagingProcessDurationDescription = "Duration of processing operation."
	// MessagingProcessMessages is the metric conforming to the
	// "messaging.process.messages" semantic conventions. It represents the
	// deprecated. Use `messaging.client.consumed.messages` instead.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	// Deprecated: Replaced by `messaging.client.consumed.messages`.
	MessagingProcessMessagesName        = "messaging.process.messages"
	MessagingProcessMessagesUnit        = "{message}"
	MessagingProcessMessagesDescription = "Deprecated. Use `messaging.client.consumed.messages` instead."
	// MessagingPublishDuration is the metric conforming to the
	// "messaging.publish.duration" semantic conventions. It represents the
	// deprecated. Use `messaging.client.operation.duration` instead.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	// Deprecated: Replaced by `messaging.client.operation.duration`.
	MessagingPublishDurationName        = "messaging.publish.duration"
	MessagingPublishDurationUnit        = "s"
	MessagingPublishDurationDescription = "Deprecated. Use `messaging.client.operation.duration` instead."
	// MessagingPublishMessages is the metric conforming to the
	// "messaging.publish.messages" semantic conventions. It represents the
	// deprecated. Use `messaging.client.produced.messages` instead.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	// Deprecated: Replaced by `messaging.client.produced.messages`.
	MessagingPublishMessagesName        = "messaging.publish.messages"
	MessagingPublishMessagesUnit        = "{message}"
	MessagingPublishMessagesDescription = "Deprecated. Use `messaging.client.produced.messages` instead."
	// MessagingReceiveDuration is the metric conforming to the
	// "messaging.receive.duration" semantic conventions. It represents the
	// deprecated. Use `messaging.client.operation.duration` instead.
	// Instrument: histogram
	// Unit: s
	// Stability: development
	// Deprecated: Replaced by `messaging.client.operation.duration`.
	MessagingReceiveDurationName        = "messaging.receive.duration"
	MessagingReceiveDurationUnit        = "s"
	MessagingReceiveDurationDescription = "Deprecated. Use `messaging.client.operation.duration` instead."
	// MessagingReceiveMessages is the metric conforming to the
	// "messaging.receive.messages" semantic conventions. It represents the
	// deprecated. Use `messaging.client.consumed.messages` instead.
	// Instrument: counter
	// Unit: {message}
	// Stability: development
	// Deprecated: Replaced by `messaging.client.consumed.messages`.
	MessagingReceiveMessagesName        = "messaging.receive.messages"
	MessagingReceiveMessagesUnit        = "{message}"
	MessagingReceiveMessagesDescription = "Deprecated. Use `messaging.client.consumed.messages` instead."
	// ProcessContextSwitches is the metric conforming to the
	// "process.context_switches" semantic conventions. It represents the number of
	// times the process has been context switched.
	// Instrument: counter
	// Unit: {count}
	// Stability: development
	ProcessContextSwitchesName        = "process.context_switches"
	ProcessContextSwitchesUnit        = "{count}"
	ProcessContextSwitchesDescription = "Number of times the process has been context switched."
	// ProcessCPUTime is the metric conforming to the "process.cpu.time" semantic
	// conventions. It represents the total CPU seconds broken down by different
	// states.
	// Instrument: counter
	// Unit: s
	// Stability: development
	ProcessCPUTimeName        = "process.cpu.time"
	ProcessCPUTimeUnit        = "s"
	ProcessCPUTimeDescription = "Total CPU seconds broken down by different states."
	// ProcessCPUUtilization is the metric conforming to the
	// "process.cpu.utilization" semantic conventions. It represents the difference
	// in process.cpu.time since the last measurement, divided by the elapsed time
	// and number of CPUs available to the process.
	// Instrument: gauge
	// Unit: 1
	// Stability: development
	ProcessCPUUtilizationName        = "process.cpu.utilization"
	ProcessCPUUtilizationUnit        = "1"
	ProcessCPUUtilizationDescription = "Difference in process.cpu.time since the last measurement, divided by the elapsed time and number of CPUs available to the process."
	// ProcessDiskIo is the metric conforming to the "process.disk.io" semantic
	// conventions. It represents the disk bytes transferred.
	// Instrument: counter
	// Unit: By
	// Stability: development
	ProcessDiskIoName        = "process.disk.io"
	ProcessDiskIoUnit        = "By"
	ProcessDiskIoDescription = "Disk bytes transferred."
	// ProcessMemoryUsage is the metric conforming to the "process.memory.usage"
	// semantic conventions. It represents the amount of physical memory in use.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	ProcessMemoryUsageName        = "process.memory.usage"
	ProcessMemoryUsageUnit        = "By"
	ProcessMemoryUsageDescription = "The amount of physical memory in use."
	// ProcessMemoryVirtual is the metric conforming to the
	// "process.memory.virtual" semantic conventions. It represents the amount of
	// committed virtual memory.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	ProcessMemoryVirtualName        = "process.memory.virtual"
	ProcessMemoryVirtualUnit        = "By"
	ProcessMemoryVirtualDescription = "The amount of committed virtual memory."
	// ProcessNetworkIo is the metric conforming to the "process.network.io"
	// semantic conventions. It represents the network bytes transferred.
	// Instrument: counter
	// Unit: By
	// Stability: development
	ProcessNetworkIoName        = "process.network.io"
	ProcessNetworkIoUnit        = "By"
	ProcessNetworkIoDescription = "Network bytes transferred."
	// ProcessOpenFileDescriptorCount is the metric conforming to the
	// "process.open_file_descriptor.count" semantic conventions. It represents the
	// number of file descriptors in use by the process.
	// Instrument: updowncounter
	// Unit: {count}
	// Stability: development
	ProcessOpenFileDescriptorCountName        = "process.open_file_descriptor.count"
	ProcessOpenFileDescriptorCountUnit        = "{count}"
	ProcessOpenFileDescriptorCountDescription = "Number of file descriptors in use by the process."
	// ProcessPagingFaults is the metric conforming to the "process.paging.faults"
	// semantic conventions. It represents the number of page faults the process
	// has made.
	// Instrument: counter
	// Unit: {fault}
	// Stability: development
	ProcessPagingFaultsName        = "process.paging.faults"
	ProcessPagingFaultsUnit        = "{fault}"
	ProcessPagingFaultsDescription = "Number of page faults the process has made."
	// ProcessThreadCount is the metric conforming to the "process.thread.count"
	// semantic conventions. It represents the process threads count.
	// Instrument: updowncounter
	// Unit: {thread}
	// Stability: development
	ProcessThreadCountName        = "process.thread.count"
	ProcessThreadCountUnit        = "{thread}"
	ProcessThreadCountDescription = "Process threads count."
	// ProcessUptime is the metric conforming to the "process.uptime" semantic
	// conventions. It represents the time the process has been running.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	ProcessUptimeName        = "process.uptime"
	ProcessUptimeUnit        = "s"
	ProcessUptimeDescription = "The time the process has been running."
	// RPCClientDuration is the metric conforming to the "rpc.client.duration"
	// semantic conventions. It represents the measures the duration of outbound
	// RPC.
	// Instrument: histogram
	// Unit: ms
	// Stability: development
	RPCClientDurationName        = "rpc.client.duration"
	RPCClientDurationUnit        = "ms"
	RPCClientDurationDescription = "Measures the duration of outbound RPC."
	// RPCClientRequestSize is the metric conforming to the
	// "rpc.client.request.size" semantic conventions. It represents the measures
	// the size of RPC request messages (uncompressed).
	// Instrument: histogram
	// Unit: By
	// Stability: development
	RPCClientRequestSizeName        = "rpc.client.request.size"
	RPCClientRequestSizeUnit        = "By"
	RPCClientRequestSizeDescription = "Measures the size of RPC request messages (uncompressed)."
	// RPCClientRequestsPerRPC is the metric conforming to the
	// "rpc.client.requests_per_rpc" semantic conventions. It represents the
	// measures the number of messages received per RPC.
	// Instrument: histogram
	// Unit: {count}
	// Stability: development
	RPCClientRequestsPerRPCName        = "rpc.client.requests_per_rpc"
	RPCClientRequestsPerRPCUnit        = "{count}"
	RPCClientRequestsPerRPCDescription = "Measures the number of messages received per RPC."
	// RPCClientResponseSize is the metric conforming to the
	// "rpc.client.response.size" semantic conventions. It represents the measures
	// the size of RPC response messages (uncompressed).
	// Instrument: histogram
	// Unit: By
	// Stability: development
	RPCClientResponseSizeName        = "rpc.client.response.size"
	RPCClientResponseSizeUnit        = "By"
	RPCClientResponseSizeDescription = "Measures the size of RPC response messages (uncompressed)."
	// RPCClientResponsesPerRPC is the metric conforming to the
	// "rpc.client.responses_per_rpc" semantic conventions. It represents the
	// measures the number of messages sent per RPC.
	// Instrument: histogram
	// Unit: {count}
	// Stability: development
	RPCClientResponsesPerRPCName        = "rpc.client.responses_per_rpc"
	RPCClientResponsesPerRPCUnit        = "{count}"
	RPCClientResponsesPerRPCDescription = "Measures the number of messages sent per RPC."
	// RPCServerDuration is the metric conforming to the "rpc.server.duration"
	// semantic conventions. It represents the measures the duration of inbound
	// RPC.
	// Instrument: histogram
	// Unit: ms
	// Stability: development
	RPCServerDurationName        = "rpc.server.duration"
	RPCServerDurationUnit        = "ms"
	RPCServerDurationDescription = "Measures the duration of inbound RPC."
	// RPCServerRequestSize is the metric conforming to the
	// "rpc.server.request.size" semantic conventions. It represents the measures
	// the size of RPC request messages (uncompressed).
	// Instrument: histogram
	// Unit: By
	// Stability: development
	RPCServerRequestSizeName        = "rpc.server.request.size"
	RPCServerRequestSizeUnit        = "By"
	RPCServerRequestSizeDescription = "Measures the size of RPC request messages (uncompressed)."
	// RPCServerRequestsPerRPC is the metric conforming to the
	// "rpc.server.requests_per_rpc" semantic conventions. It represents the
	// measures the number of messages received per RPC.
	// Instrument: histogram
	// Unit: {count}
	// Stability: development
	RPCServerRequestsPerRPCName        = "rpc.server.requests_per_rpc"
	RPCServerRequestsPerRPCUnit        = "{count}"
	RPCServerRequestsPerRPCDescription = "Measures the number of messages received per RPC."
	// RPCServerResponseSize is the metric conforming to the
	// "rpc.server.response.size" semantic conventions. It represents the measures
	// the size of RPC response messages (uncompressed).
	// Instrument: histogram
	// Unit: By
	// Stability: development
	RPCServerResponseSizeName        = "rpc.server.response.size"
	RPCServerResponseSizeUnit        = "By"
	RPCServerResponseSizeDescription = "Measures the size of RPC response messages (uncompressed)."
	// RPCServerResponsesPerRPC is the metric conforming to the
	// "rpc.server.responses_per_rpc" semantic conventions. It represents the
	// measures the number of messages sent per RPC.
	// Instrument: histogram
	// Unit: {count}
	// Stability: development
	RPCServerResponsesPerRPCName        = "rpc.server.responses_per_rpc"
	RPCServerResponsesPerRPCUnit        = "{count}"
	RPCServerResponsesPerRPCDescription = "Measures the number of messages sent per RPC."
	// SignalrServerActiveConnections is the metric conforming to the
	// "signalr.server.active_connections" semantic conventions. It represents the
	// number of connections that are currently active on the server.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: stable
	SignalrServerActiveConnectionsName        = "signalr.server.active_connections"
	SignalrServerActiveConnectionsUnit        = "{connection}"
	SignalrServerActiveConnectionsDescription = "Number of connections that are currently active on the server."
	// SignalrServerConnectionDuration is the metric conforming to the
	// "signalr.server.connection.duration" semantic conventions. It represents the
	// duration of connections on the server.
	// Instrument: histogram
	// Unit: s
	// Stability: stable
	SignalrServerConnectionDurationName        = "signalr.server.connection.duration"
	SignalrServerConnectionDurationUnit        = "s"
	SignalrServerConnectionDurationDescription = "The duration of connections on the server."
	// SystemCPUFrequency is the metric conforming to the "system.cpu.frequency"
	// semantic conventions. It represents the reports the current frequency of the
	// CPU in Hz.
	// Instrument: gauge
	// Unit: {Hz}
	// Stability: development
	SystemCPUFrequencyName        = "system.cpu.frequency"
	SystemCPUFrequencyUnit        = "{Hz}"
	SystemCPUFrequencyDescription = "Reports the current frequency of the CPU in Hz"
	// SystemCPULogicalCount is the metric conforming to the
	// "system.cpu.logical.count" semantic conventions. It represents the reports
	// the number of logical (virtual) processor cores created by the operating
	// system to manage multitasking.
	// Instrument: updowncounter
	// Unit: {cpu}
	// Stability: development
	SystemCPULogicalCountName        = "system.cpu.logical.count"
	SystemCPULogicalCountUnit        = "{cpu}"
	SystemCPULogicalCountDescription = "Reports the number of logical (virtual) processor cores created by the operating system to manage multitasking"
	// SystemCPUPhysicalCount is the metric conforming to the
	// "system.cpu.physical.count" semantic conventions. It represents the reports
	// the number of actual physical processor cores on the hardware.
	// Instrument: updowncounter
	// Unit: {cpu}
	// Stability: development
	SystemCPUPhysicalCountName        = "system.cpu.physical.count"
	SystemCPUPhysicalCountUnit        = "{cpu}"
	SystemCPUPhysicalCountDescription = "Reports the number of actual physical processor cores on the hardware"
	// SystemCPUTime is the metric conforming to the "system.cpu.time" semantic
	// conventions. It represents the seconds each logical CPU spent on each mode.
	// Instrument: counter
	// Unit: s
	// Stability: development
	SystemCPUTimeName        = "system.cpu.time"
	SystemCPUTimeUnit        = "s"
	SystemCPUTimeDescription = "Seconds each logical CPU spent on each mode"
	// SystemCPUUtilization is the metric conforming to the
	// "system.cpu.utilization" semantic conventions. It represents the difference
	// in system.cpu.time since the last measurement, divided by the elapsed time
	// and number of logical CPUs.
	// Instrument: gauge
	// Unit: 1
	// Stability: development
	SystemCPUUtilizationName        = "system.cpu.utilization"
	SystemCPUUtilizationUnit        = "1"
	SystemCPUUtilizationDescription = "Difference in system.cpu.time since the last measurement, divided by the elapsed time and number of logical CPUs"
	// SystemDiskIo is the metric conforming to the "system.disk.io" semantic
	// conventions.
	// Instrument: counter
	// Unit: By
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemDiskIoName = "system.disk.io"
	SystemDiskIoUnit = "By"
	// SystemDiskIoTime is the metric conforming to the "system.disk.io_time"
	// semantic conventions. It represents the time disk spent activated.
	// Instrument: counter
	// Unit: s
	// Stability: development
	SystemDiskIoTimeName        = "system.disk.io_time"
	SystemDiskIoTimeUnit        = "s"
	SystemDiskIoTimeDescription = "Time disk spent activated"
	// SystemDiskLimit is the metric conforming to the "system.disk.limit" semantic
	// conventions. It represents the total storage capacity of the disk.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemDiskLimitName        = "system.disk.limit"
	SystemDiskLimitUnit        = "By"
	SystemDiskLimitDescription = "The total storage capacity of the disk"
	// SystemDiskMerged is the metric conforming to the "system.disk.merged"
	// semantic conventions.
	// Instrument: counter
	// Unit: {operation}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemDiskMergedName = "system.disk.merged"
	SystemDiskMergedUnit = "{operation}"
	// SystemDiskOperationTime is the metric conforming to the
	// "system.disk.operation_time" semantic conventions. It represents the sum of
	// the time each operation took to complete.
	// Instrument: counter
	// Unit: s
	// Stability: development
	SystemDiskOperationTimeName        = "system.disk.operation_time"
	SystemDiskOperationTimeUnit        = "s"
	SystemDiskOperationTimeDescription = "Sum of the time each operation took to complete"
	// SystemDiskOperations is the metric conforming to the
	// "system.disk.operations" semantic conventions.
	// Instrument: counter
	// Unit: {operation}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemDiskOperationsName = "system.disk.operations"
	SystemDiskOperationsUnit = "{operation}"
	// SystemFilesystemLimit is the metric conforming to the
	// "system.filesystem.limit" semantic conventions. It represents the total
	// storage capacity of the filesystem.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemFilesystemLimitName        = "system.filesystem.limit"
	SystemFilesystemLimitUnit        = "By"
	SystemFilesystemLimitDescription = "The total storage capacity of the filesystem"
	// SystemFilesystemUsage is the metric conforming to the
	// "system.filesystem.usage" semantic conventions. It represents the reports a
	// filesystem's space usage across different states.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemFilesystemUsageName        = "system.filesystem.usage"
	SystemFilesystemUsageUnit        = "By"
	SystemFilesystemUsageDescription = "Reports a filesystem's space usage across different states."
	// SystemFilesystemUtilization is the metric conforming to the
	// "system.filesystem.utilization" semantic conventions.
	// Instrument: gauge
	// Unit: 1
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemFilesystemUtilizationName = "system.filesystem.utilization"
	SystemFilesystemUtilizationUnit = "1"
	// SystemLinuxMemoryAvailable is the metric conforming to the
	// "system.linux.memory.available" semantic conventions. It represents an
	// estimate of how much memory is available for starting new applications,
	// without causing swapping.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemLinuxMemoryAvailableName        = "system.linux.memory.available"
	SystemLinuxMemoryAvailableUnit        = "By"
	SystemLinuxMemoryAvailableDescription = "An estimate of how much memory is available for starting new applications, without causing swapping"
	// SystemLinuxMemorySlabUsage is the metric conforming to the
	// "system.linux.memory.slab.usage" semantic conventions. It represents the
	// reports the memory used by the Linux kernel for managing caches of
	// frequently used objects.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemLinuxMemorySlabUsageName        = "system.linux.memory.slab.usage"
	SystemLinuxMemorySlabUsageUnit        = "By"
	SystemLinuxMemorySlabUsageDescription = "Reports the memory used by the Linux kernel for managing caches of frequently used objects."
	// SystemMemoryLimit is the metric conforming to the "system.memory.limit"
	// semantic conventions. It represents the total memory available in the
	// system.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemMemoryLimitName        = "system.memory.limit"
	SystemMemoryLimitUnit        = "By"
	SystemMemoryLimitDescription = "Total memory available in the system."
	// SystemMemoryShared is the metric conforming to the "system.memory.shared"
	// semantic conventions. It represents the shared memory used (mostly by
	// tmpfs).
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemMemorySharedName        = "system.memory.shared"
	SystemMemorySharedUnit        = "By"
	SystemMemorySharedDescription = "Shared memory used (mostly by tmpfs)."
	// SystemMemoryUsage is the metric conforming to the "system.memory.usage"
	// semantic conventions. It represents the reports memory in use by state.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemMemoryUsageName        = "system.memory.usage"
	SystemMemoryUsageUnit        = "By"
	SystemMemoryUsageDescription = "Reports memory in use by state."
	// SystemMemoryUtilization is the metric conforming to the
	// "system.memory.utilization" semantic conventions.
	// Instrument: gauge
	// Unit: 1
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemMemoryUtilizationName = "system.memory.utilization"
	SystemMemoryUtilizationUnit = "1"
	// SystemNetworkConnections is the metric conforming to the
	// "system.network.connections" semantic conventions.
	// Instrument: updowncounter
	// Unit: {connection}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemNetworkConnectionsName = "system.network.connections"
	SystemNetworkConnectionsUnit = "{connection}"
	// SystemNetworkDropped is the metric conforming to the
	// "system.network.dropped" semantic conventions. It represents the count of
	// packets that are dropped or discarded even though there was no error.
	// Instrument: counter
	// Unit: {packet}
	// Stability: development
	SystemNetworkDroppedName        = "system.network.dropped"
	SystemNetworkDroppedUnit        = "{packet}"
	SystemNetworkDroppedDescription = "Count of packets that are dropped or discarded even though there was no error"
	// SystemNetworkErrors is the metric conforming to the "system.network.errors"
	// semantic conventions. It represents the count of network errors detected.
	// Instrument: counter
	// Unit: {error}
	// Stability: development
	SystemNetworkErrorsName        = "system.network.errors"
	SystemNetworkErrorsUnit        = "{error}"
	SystemNetworkErrorsDescription = "Count of network errors detected"
	// SystemNetworkIo is the metric conforming to the "system.network.io" semantic
	// conventions.
	// Instrument: counter
	// Unit: By
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemNetworkIoName = "system.network.io"
	SystemNetworkIoUnit = "By"
	// SystemNetworkPackets is the metric conforming to the
	// "system.network.packets" semantic conventions.
	// Instrument: counter
	// Unit: {packet}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemNetworkPacketsName = "system.network.packets"
	SystemNetworkPacketsUnit = "{packet}"
	// SystemPagingFaults is the metric conforming to the "system.paging.faults"
	// semantic conventions.
	// Instrument: counter
	// Unit: {fault}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemPagingFaultsName = "system.paging.faults"
	SystemPagingFaultsUnit = "{fault}"
	// SystemPagingOperations is the metric conforming to the
	// "system.paging.operations" semantic conventions.
	// Instrument: counter
	// Unit: {operation}
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemPagingOperationsName = "system.paging.operations"
	SystemPagingOperationsUnit = "{operation}"
	// SystemPagingUsage is the metric conforming to the "system.paging.usage"
	// semantic conventions. It represents the unix swap or windows pagefile usage.
	// Instrument: updowncounter
	// Unit: By
	// Stability: development
	SystemPagingUsageName        = "system.paging.usage"
	SystemPagingUsageUnit        = "By"
	SystemPagingUsageDescription = "Unix swap or windows pagefile usage"
	// SystemPagingUtilization is the metric conforming to the
	// "system.paging.utilization" semantic conventions.
	// Instrument: gauge
	// Unit: 1
	// Stability: development
	// NOTE: The description (brief) for this metric is not defined in the semantic-conventions repository.
	SystemPagingUtilizationName = "system.paging.utilization"
	SystemPagingUtilizationUnit = "1"
	// SystemProcessCount is the metric conforming to the "system.process.count"
	// semantic conventions. It represents the total number of processes in each
	// state.
	// Instrument: updowncounter
	// Unit: {process}
	// Stability: development
	SystemProcessCountName        = "system.process.count"
	SystemProcessCountUnit        = "{process}"
	SystemProcessCountDescription = "Total number of processes in each state"
	// SystemProcessCreated is the metric conforming to the
	// "system.process.created" semantic conventions. It represents the total
	// number of processes created over uptime of the host.
	// Instrument: counter
	// Unit: {process}
	// Stability: development
	SystemProcessCreatedName        = "system.process.created"
	SystemProcessCreatedUnit        = "{process}"
	SystemProcessCreatedDescription = "Total number of processes created over uptime of the host"
	// SystemUptime is the metric conforming to the "system.uptime" semantic
	// conventions. It represents the time the system has been running.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	SystemUptimeName        = "system.uptime"
	SystemUptimeUnit        = "s"
	SystemUptimeDescription = "The time the system has been running"
	// VCSChangeCount is the metric conforming to the "vcs.change.count" semantic
	// conventions. It represents the number of changes (pull requests/merge
	// requests/changelists) in a repository, categorized by their state (e.g. open
	// or merged).
	// Instrument: updowncounter
	// Unit: {change}
	// Stability: development
	VCSChangeCountName        = "vcs.change.count"
	VCSChangeCountUnit        = "{change}"
	VCSChangeCountDescription = "The number of changes (pull requests/merge requests/changelists) in a repository, categorized by their state (e.g. open or merged)"
	// VCSChangeDuration is the metric conforming to the "vcs.change.duration"
	// semantic conventions. It represents the time duration a change (pull
	// request/merge request/changelist) has been in a given state.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	VCSChangeDurationName        = "vcs.change.duration"
	VCSChangeDurationUnit        = "s"
	VCSChangeDurationDescription = "The time duration a change (pull request/merge request/changelist) has been in a given state."
	// VCSChangeTimeToApproval is the metric conforming to the
	// "vcs.change.time_to_approval" semantic conventions. It represents the amount
	// of time since its creation it took a change (pull request/merge
	// request/changelist) to get the first approval.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	VCSChangeTimeToApprovalName        = "vcs.change.time_to_approval"
	VCSChangeTimeToApprovalUnit        = "s"
	VCSChangeTimeToApprovalDescription = "The amount of time since its creation it took a change (pull request/merge request/changelist) to get the first approval."
	// VCSChangeTimeToMerge is the metric conforming to the
	// "vcs.change.time_to_merge" semantic conventions. It represents the amount of
	// time since its creation it took a change (pull request/merge
	// request/changelist) to get merged into the target(base) ref.
	// Instrument: gauge
	// Unit: s
	// Stability: development
	VCSChangeTimeToMergeName        = "vcs.change.time_to_merge"
	VCSChangeTimeToMergeUnit        = "s"
	VCSChangeTimeToMergeDescription = "The amount of time since its creation it took a change (pull request/merge request/changelist) to get merged into the target(base) ref."
	// VCSContributorCount is the metric conforming to the "vcs.contributor.count"
	// semantic conventions. It represents the number of unique contributors to a
	// repository.
	// Instrument: gauge
	// Unit: {contributor}
	// Stability: development
	VCSContributorCountName        = "vcs.contributor.count"
	VCSContributorCountUnit        = "{contributor}"
	VCSContributorCountDescription = "The number of unique contributors to a repository"
	// VCSRefCount is the metric conforming to the "vcs.ref.count" semantic
	// conventions. It represents the number of refs of type branch or tag in a
	// repository.
	// Instrument: updowncounter
	// Unit: {ref}
	// Stability: development
	VCSRefCountName        = "vcs.ref.count"
	VCSRefCountUnit        = "{ref}"
	VCSRefCountDescription = "The number of refs of type branch or tag in a repository."
	// VCSRefLinesDelta is the metric conforming to the "vcs.ref.lines_delta"
	// semantic conventions. It represents the number of lines added/removed in a
	// ref (branch) relative to the ref from the `vcs.ref.base.name` attribute.
	// Instrument: gauge
	// Unit: {line}
	// Stability: development
	VCSRefLinesDeltaName        = "vcs.ref.lines_delta"
	VCSRefLinesDeltaUnit        = "{line}"
	VCSRefLinesDeltaDescription = "The number of lines added/removed in a ref (branch) relative to the ref from the `vcs.ref.base.name` attribute."
	// VCSRefRevisionsDelta is the metric conforming to the
	// "vcs.ref.revisions_delta" semantic conventions. It represents the number of
	// revisions (commits) a ref (branch) is ahead/behind the branch from the
	// `vcs.ref.base.name` attribute.
	// Instrument: gauge
	// Unit: {revision}
	// Stability: development
	VCSRefRevisionsDeltaName        = "vcs.ref.revisions_delta"
	VCSRefRevisionsDeltaUnit        = "{revision}"
	VCSRefRevisionsDeltaDescription = "The number of revisions (commits) a ref (branch) is ahead/behind the branch from the `vcs.ref.base.name` attribute"
	// VCSRefTime is the metric conforming to the "vcs.ref.time" semantic
	// conventions. It represents the time a ref (branch) created from the default
	// branch (trunk) has existed. The `ref.type` attribute will always be `branch`
	// .
	// Instrument: gauge
	// Unit: s
	// Stability: development
	VCSRefTimeName        = "vcs.ref.time"
	VCSRefTimeUnit        = "s"
	VCSRefTimeDescription = "Time a ref (branch) created from the default branch (trunk) has existed. The `ref.type` attribute will always be `branch`"
	// VCSRepositoryCount is the metric conforming to the "vcs.repository.count"
	// semantic conventions. It represents the number of repositories in an
	// organization.
	// Instrument: updowncounter
	// Unit: {repository}
	// Stability: development
	VCSRepositoryCountName        = "vcs.repository.count"
	VCSRepositoryCountUnit        = "{repository}"
	VCSRepositoryCountDescription = "The number of repositories in an organization."
)
