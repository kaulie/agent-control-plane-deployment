package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Identity is the phase-1 (简单) caller identity for the deployment-triggering
// APIs. It travels in two plain request headers:
//
//	identity_role: user | agent
//	identity_id:   user_001 / agent_002 / ...
//
// There is no secret/token yet: the check only makes deploys attributable
// (audit trail + the panel's "触发者" column). See README "身份校验（第一阶段）".
type Identity struct {
	Role string `json:"role"`
	ID   string `json:"id"`
}

const (
	identityRoleHeader = "identity_role"
	identityIDHeader   = "identity_id"

	identityRoleUser  = "user"
	identityRoleAgent = "agent"

	// maxIdentityIDLen bounds the echoed/stored id so a header can not bloat a
	// DB row or the panel.
	maxIdentityIDLen = 64
)

// Known reports whether an identity was supplied (false = caller unidentified,
// e.g. requests recorded before this feature or when enforcement is off).
func (i Identity) Known() bool { return i.Role != "" || i.ID != "" }

// String renders the identity as "role:id" ("" when unknown).
func (i Identity) String() string {
	if !i.Known() {
		return ""
	}
	return i.Role + ":" + i.ID
}

// parseIdentity validates the phase-1 identity headers. A missing or malformed
// identity is an error so callers can reject the request.
func parseIdentity(role, id string) (Identity, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	id = strings.TrimSpace(id)
	if role == "" || id == "" {
		return Identity{}, errors.New(
			"missing identity: set headers identity_role (user|agent) and identity_id")
	}
	if role != identityRoleUser && role != identityRoleAgent {
		return Identity{}, fmt.Errorf("invalid identity_role %q: must be %q or %q",
			role, identityRoleUser, identityRoleAgent)
	}
	if len(id) > maxIdentityIDLen {
		return Identity{}, fmt.Errorf("identity_id too long (max %d chars)", maxIdentityIDLen)
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return Identity{}, errors.New("identity_id must not contain whitespace")
	}
	return Identity{Role: role, ID: id}, nil
}

// identityFromRequest reads the identity headers off an incoming request.
func identityFromRequest(r *http.Request) (Identity, error) {
	return parseIdentity(r.Header.Get(identityRoleHeader), r.Header.Get(identityIDHeader))
}

// requireIdentity resolves the caller identity for a deploy-triggering request.
// With enforcement on (the default) a missing/invalid identity writes 401 and
// returns ok=false, so the handler must stop. With IDENTITY_ENFORCE=0 the
// request proceeds unidentified (log-only) — an escape hatch for rolling the
// headers out to every caller without breaking existing clients.
func (s *apiServer) requireIdentity(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, err := identityFromRequest(r)
	if err == nil {
		return id, true
	}
	if s.cfg.IdentityEnforce {
		writeError(w, http.StatusUnauthorized, err.Error())
		return Identity{}, false
	}
	log.Printf("[identity] %s %s: %v (IDENTITY_ENFORCE=0: allowed, recorded as unknown)",
		r.Method, r.URL.Path, err)
	return Identity{}, true
}
