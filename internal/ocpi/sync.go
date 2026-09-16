package ocpi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// The pull half of OCPI 2.3.0: the receivers in this package learn
// about the network only when a CPO chooses to push it, which leaves ChargeNet
// blind to a push that was lost — the CPO was restarting, the network blipped —
// or that a partner which never implemented push has never sent at all. The
// [Syncer] is the second, independent copy of the same truth: it reads each
// active partner's `locations` and `tariffs` modules with Token B and upserts
// them through the very code path the receivers use, so a document that arrives
// either way lands identically.

// DefaultSyncInterval is how often the pull sync runs. It is short enough to repair a lost push during a demo and
// long enough that a fleet of partners is not polled needlessly.
const DefaultSyncInterval = 5 * time.Minute

// The module paths the pull reads. Unlike ChargeNet's own `VersionURLPath`
// (`/ocpi/emsp/2.3.0`), these are the *partner's* paths — the same shape as
// [CommandsPath] — because the base URL ChargeNet holds is the CPO's version
// root, not ours.
const (
	locationsPath = "/" + VersionNumber + "/" + ModuleLocations
	tariffsPath   = "/" + VersionNumber + "/" + ModuleTariffs
)

// Syncer reconciles ChargeNet's copy of the network with what each active CPO
// currently publishes.
type Syncer struct {
	store    *store.Store
	client   *Client
	interval time.Duration
}

// SyncOption configures a [Syncer] at construction.
type SyncOption func(*Syncer)

// WithSyncClient replaces the HTTP client the sync calls partners with. Tests
// point it at an httptest server's own client; production keeps the default.
func WithSyncClient(c *Client) SyncOption {
	return func(s *Syncer) {
		if c != nil {
			s.client = c
		}
	}
}

// WithSyncInterval overrides [DefaultSyncInterval]. A non-positive interval
// disables the periodic loop ([Syncer.Run] returns at once), which is what a
// test that drives [Syncer.SyncAll] by hand wants.
func WithSyncInterval(d time.Duration) SyncOption {
	return func(s *Syncer) { s.interval = d }
}

// NewSyncer builds the pull sync over a store.
func NewSyncer(st *store.Store, opts ...SyncOption) *Syncer {
	s := &Syncer{store: st, client: NewClient(), interval: DefaultSyncInterval}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run waits one interval before the first pull because emulators push their
// network at startup and the HTTP listener must be ready first.
func (s *Syncer) Run(ctx context.Context) {
	if s.interval <= 0 {
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SyncAll(ctx); err != nil {
				log.Printf("ocpi: periodic sync: %v", err)
			}
		}
	}
}

// SyncAll pulls and reconciles every active partner. One partner's failure does
// not stop the others: each is logged and the first error is returned, so a
// single unreachable CPO cannot leave the rest of the network stale.
func (s *Syncer) SyncAll(ctx context.Context) error {
	cpos, err := s.store.ListActiveCPOs(ctx)
	if err != nil {
		return fmt.Errorf("list active CPOs: %w", err)
	}
	var firstErr error
	for _, cpo := range cpos {
		if err := s.SyncOne(ctx, cpo); err != nil {
			log.Printf("ocpi: sync %s (%s): %v", cpo.Slug, cpo.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SyncOne pulls one partner's locations and tariffs and upserts them.
//
// Locations are required — a partner with no locations has nothing to offer —
// so a failed locations pull fails the whole sync. Tariffs are optional "): a partner that does not serve the module is
// not an error, and a connector that already references a tariff keeps its
// price. Malformed locations are skipped independently, matching push handling.
func (s *Syncer) SyncOne(ctx context.Context, cpo *store.CpoAccount) error {
	if cpo == nil || strings.TrimSpace(cpo.BaseURL) == "" || strings.TrimSpace(cpo.TokenB) == "" {
		return nil
	}

	locations, err := s.pullLocations(ctx, cpo)
	if err != nil {
		return fmt.Errorf("locations: %w", err)
	}
	for _, loc := range locations {
		in, err := locationInput(cpo, loc.ID, loc)
		if err != nil {
			log.Printf("ocpi: sync %s: skipping location %s: %v", cpo.Slug, loc.ID, err)
			continue
		}
		if _, err := s.store.UpsertOCPILocation(ctx, *in); err != nil {
			return fmt.Errorf("store location %s: %w", loc.ID, err)
		}
	}

	if err := s.syncTariffs(ctx, cpo); err != nil {
		log.Printf("ocpi: sync %s: tariffs: %v", cpo.Slug, err)
	}
	return nil
}

// pullLocations reads `GET {base_url}/2.3.0/locations`.
func (s *Syncer) pullLocations(ctx context.Context, cpo *store.CpoAccount) ([]Location, error) {
	data, err := s.client.Do(ctx, cpo, http.MethodGet, locationsPath, nil)
	if err != nil {
		return nil, err
	}
	return decodeList[Location](data)
}

// syncTariffs reads `GET {base_url}/2.3.0/tariffs` and upserts what it finds.
func (s *Syncer) syncTariffs(ctx context.Context, cpo *store.CpoAccount) error {
	data, err := s.client.Do(ctx, cpo, http.MethodGet, tariffsPath, nil)
	if err != nil {
		return err
	}
	tariffs, err := decodeList[Tariff](data)
	if err != nil {
		return err
	}
	for _, t := range tariffs {
		if err := checkTariff(t); err != nil {
			if e := s.store.InvalidateOCPITariff(ctx, cpo.ID, t.ID, t.RawJSON); e != nil {
				return e
			}
			log.Printf("ocpi: sync %s: skipping tariff %s: %v", cpo.Slug, t.ID, err)
			continue
		}
		in := store.OCPITariffInput{
			CpoID:       cpo.ID,
			ID:          t.ID,
			Currency:    strings.TrimSpace(t.Currency),
			PricePerKWh: firstEnergyPrice(t),
			Unsupported: !simpleEnergyTariff(t), RawJSON: t.RawJSON,
		}
		if err := s.store.UpsertOCPITariff(ctx, in); err != nil {
			return fmt.Errorf("store tariff %s: %w", t.ID, err)
		}
	}
	return nil
}

// decodeList treats an empty or null OCPI data body as an empty list.
func decodeList[T any](data json.RawMessage) ([]T, error) {
	switch trimmed := strings.TrimSpace(string(data)); trimmed {
	case "", "null":
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("read the list: %w", err)
	}
	return out, nil
}
