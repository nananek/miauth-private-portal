package configstore

import (
	"context"
	"time"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Seed performs ADR-0006 §2-4's automatic, idempotent migration from
// file/env to DB: for every db-eligible key with no existing app_config
// row, it writes one from cfg's own current effective value
// (cfg.Redacted()) — the same file/env/default resolution that produced
// cfg in the first place. An existing row is never touched, so this is
// safe to call on every startup; only the set of not-yet-seeded keys
// shrinks over time (or grows back if an operator Unsets a key,
// reproducing pre-ADR-0006 behavior for it until the next seed sees it
// unset again).
//
// Every new row and its audit entry are written inside one
// domain.UnitOfWork.WithinTx transaction, so a mid-seed failure leaves
// no partially-seeded state for this call: either every key that needed
// seeding got a row and an audit entry, or none did.
//
// seededBy is the actor id attributed to every row this call writes —
// cmd/server passes domain.ActorSystem's id, since this runs
// unconditionally at every startup, before an owner may even be bound
// yet (miauthctl config's own writes attribute to the owner actor
// instead; see cmd/miauthctl/config.go's ownerActorID).
//
// It returns how many keys were newly seeded, for a startup log line.
func Seed(ctx context.Context, uow domain.UnitOfWork, cfg *config.Config, seededBy string, now time.Time) (int, error) {
	redacted := cfg.Redacted()
	seeded := 0

	err := uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		existing, err := repos.Config.List(ctx)
		if err != nil {
			return err
		}
		already := make(map[string]bool, len(existing))
		for _, e := range existing {
			already[e.Key] = true
		}

		for _, key := range config.DBEligibleKeys() {
			if already[key] {
				continue
			}
			value := redacted[key]
			if err := repos.Config.Set(ctx, key, value, 0, seededBy, now); err != nil {
				return err
			}
			if err := repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
				ID: domain.NewID(), Key: key, OldValue: nil, NewValue: &value,
				Version: 1, ChangedAt: now, ChangedBy: seededBy,
			}); err != nil {
				return err
			}
			seeded++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return seeded, nil
}
