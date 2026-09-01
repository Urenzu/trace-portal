package api

import (
	"net/http"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/tenant"
)

// Sessions is the part of sign-in the read API depends on: given a request,
// whose it is. Narrow on purpose -- the query API has no business minting a
// session, revoking one, or reaching a directory.
type Sessions interface {
	Session(r *http.Request) (auth.Account, bool)
}

// FromSession serves each signed-in browser its own tenant's data.
//
// This closes the last gap in the cloud path. Ingest has resolved the tenant
// from a collector credential since it was written; until now the read side
// still answered from whatever archive the process itself captured into, so a
// server holding ten customers would have shown all ten of them the operator's
// laptop. The two sides now resolve the same way -- from a credential, into a
// storage root -- and differ only in which credential.
//
// A missing session is ErrNoSession, and a tenant the registry refuses is not
// distinguished from one that does not exist; see tenant.ErrUnknownTenant.
func FromSession(sessions Sessions, registry *tenant.Registry) Resolver {
	return sessionResolver{sessions: sessions, registry: registry}
}

type sessionResolver struct {
	sessions Sessions
	registry *tenant.Registry
}

func (sr sessionResolver) Scope(r *http.Request) (Scope, error) {
	acct, ok := sr.sessions.Session(r)
	if !ok {
		return Scope{}, ErrNoSession
	}
	// acct.TenantID came from the directory, which resolved it from a verified
	// id_token. Nothing in the request reached it, which is the property that
	// makes the storage below this line unreachable from another tenant.
	storage, err := sr.registry.For(acct.TenantID)
	if err != nil {
		return Scope{}, err
	}
	// No Coverage: a server has no tailer reading this tenant's disk. Reporting
	// the operator's own ingest coverage here would describe a machine the
	// person looking at it has never touched.
	return Scope{Store: storage.Store, Compact: storage.Compactor}, nil
}
