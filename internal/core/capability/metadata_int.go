// metadata_int.go — exported metadata helpers used by the CLI report
// renderer.
//
// These exist because int64FromMeta (the executor gate's helper)
// is unexported. The CLI's grant-operator report wants to render
// the same numeric value the gate reads, so we expose the helper
// without changing the gate's signature.
//
// Keep this in lockstep with int64FromMeta in executor.go: any
// future numeric type the gate accepts must be accepted here too.
package capability

// Int64FromMeta reads a known-int metadata key and returns it as
// int64. Returns 0 if the key is absent or not parseable as int64.
//
// Mirrors the executor's int64FromMeta exactly: same JSON-decoded
// numeric types (int64, int, float64) are accepted. Use this from
// CLI handlers / report renderers that want to show the same value
// the gate will check.
//
// Note: this is intentionally NOT a replacement for the executor
// gate's internal helper. The executor path stays package-private
// to keep the gate's surface minimal; the CLI path is the public
// reporting counterpart.
func Int64FromMeta(m CapabilityMetadata, key string) int64 {
	return int64FromMeta(m, key)
}
