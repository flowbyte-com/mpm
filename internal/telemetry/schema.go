// internal/telemetry/schema.go — constants shared across the telemetry package.
package telemetry

// SchemaVersion is the wire-format version accepted by the collector.
// Unknown versions are rejected with a structured error.
const SchemaVersion = "v1"
