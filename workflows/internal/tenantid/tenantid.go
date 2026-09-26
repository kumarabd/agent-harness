// Package tenantid is the ONE place that turns a Clerk user id into a
// tenant slug — imported by both the router (workflows/internal/router/
// core, to resolve which tenant to proxy to) and the automation worker
// (workflows/internal/automation/activities, to provision a tenant at
// exactly the slug the router will later look for). Two independent copies
// of this logic could silently drift and break tenant resolution, so there
// is deliberately only one.
package tenantid

import "strings"

// SlugForSub derives the one deterministic tenant identifier from a Clerk
// user id — e.g. "user_3Eff4LLujKDqbvbvnJWvGqNdleZ" ->
// "user-3eff4llujkdqbvbvnjwvgqndlez". Clerk ids are already restricted to
// [A-Za-z0-9_], so lowercasing + replacing "_" with "-" always produces a
// valid RFC 1123 label (lowercase alphanumeric + hyphen) — the same shape
// this repo's tenant-slug validation already enforces, just derived instead
// of user-chosen. This is simultaneously the tenant's Temporal namespace,
// Kubernetes namespace, and Helm release name — all the same string, by
// convention (docs/components/gateway/web.md, 2026-09-25).
func SlugForSub(sub string) string {
	return strings.ToLower(strings.ReplaceAll(sub, "_", "-"))
}
