package trace

// Identity names who produced a turn. It is the one field family that cannot be
// added later.
//
// Everything else in this model is a measurement the transcript carries, so a
// build that starts recording it can reconstruct history by re-reading the logs.
// Attribution is not in the logs at all: a Claude Code transcript records what a
// session did, never who ran it. The only moment the answer is known is the
// moment of capture, on the machine of the person it belongs to. A build that
// adds these fields after real data exists leaves a permanent population of
// turns with no owner, and no later pass can repair it — which is why identity
// lands before the collector split rather than with it.
//
// The ids are deliberately opaque. They are minted by this system, not by
// whichever identity provider handles login: a provider's subject written
// straight onto a turn would make the archive's durable form depend on a vendor
// that may be swapped, and swapping it would mean rewriting every partition.
// The auth layer maps a provider subject onto a UserID; the archive never learns
// which provider issued it.
type Identity struct {
	// TenantID is the account the data belongs to — one person, or one
	// organisation. It is the isolation boundary, and once storage is
	// partitioned it appears in the path rather than in a predicate.
	TenantID string `json:"tenant_id,omitempty"`

	// UserID is the engineer whose machine produced the turn. Stable across
	// their machines, which is what makes per-engineer attribution possible.
	UserID string `json:"user_id,omitempty"`

	// MachineID distinguishes that engineer's laptop from their desktop. It
	// matters for deduplication rather than for reporting: the same session can
	// be observed by two collectors, and knowing which one spoke is how a
	// replayed batch is told apart from genuinely new work.
	MachineID string `json:"machine_id,omitempty"`
}

// Attributed reports whether a record can be assigned to an owner. A turn
// missing either id can still be read and costed, but it can never appear in a
// per-engineer or per-team view, so the archive counts these rather than
// quietly folding them into someone else's total.
func (i Identity) Attributed() bool { return i.TenantID != "" && i.UserID != "" }

// Zero reports an identity that carries nothing.
func (i Identity) Zero() bool { return i == Identity{} }

// Merge fills empty fields from o, never overwriting what is already set.
//
// Two sources can observe the same call — a tailed log and the proxy — and only
// one of them may know the identity. Whichever observed a field keeps it, which
// is the same merge rule the rest of the event model uses.
func (i *Identity) Merge(o Identity) {
	if i.TenantID == "" {
		i.TenantID = o.TenantID
	}
	if i.UserID == "" {
		i.UserID = o.UserID
	}
	if i.MachineID == "" {
		i.MachineID = o.MachineID
	}
}
