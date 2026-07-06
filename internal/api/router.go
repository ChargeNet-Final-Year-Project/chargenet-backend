// Package api composes driver, portal and OCPI routes with shared middleware.
package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/ocpi/emulator"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// Server carries the dependencies shared by the handlers.
type Server struct {
	Cfg   config.Config
	Store *store.Store
	// Gateway is the configured charge transport.
	Gateway gateway.CPOGateway
	// Sessions owns everything that happens around a charge: start validation,
	// telemetry, settlement and the SSE fan-out.
	Sessions *sessions.Manager
	// OCPI is the receiver surface mounted under /ocpi/emsp. It is the
	// same object [Server.Handler] registers, kept here so the OCPI gateway can
	// be installed as its sink before any request arrives.
	OCPI *ocpi.Server
	// Emulators are the three in-process CPO emulators, mounted under
	// /emulator/{slug}/ocpi. They run whenever the platform runs, so the whole
	// CPO world is demoable over HTTP loopback with no real operator.
	Emulators []*emulator.Emulator
	// Sync is the periodic pull of the OCPI gateway: every five minutes it re-reads
	// each active CPO's locations and tariffs over OCPI and reconciles them,
	// covering a push a partner missed or never sent.
	Sync *ocpi.Syncer
	// portalLimiter throttles CPO-portal login attempts:
	// 5 failures per 5 minutes per IP+email, then 429.
	portalLimiter *portalLimiter
}

// NewServer builds the API server and the session orchestrator behind it.
//
// main.go calls [Server.Run] to start the orchestrator and [Server.Close] on
// the way out; tests use [NewRouter], which is NewServer + Handler.
func NewServer(cfg config.Config, st *store.Store) *Server {
	gw := newGateway(cfg, st)

	receiver := ocpi.New(cfg, st)
	// When ChargeNet commands real CPOs, the OCPI gateway is the seam of both
	// halves of OCPI receivers: the orchestrator's CPOGateway *and* the receiver's sinks,
	// so a CPO's push becomes telemetry and its asynchronous command result
	// wakes whoever is waiting on it. The local simulator has neither, and the
	// receivers then just persist what they own and accept the rest.
	if g, ok := gw.(*ocpi.Gateway); ok {
		receiver.Sink = g
		receiver.Commands = g
	}

	return &Server{
		Cfg:           cfg,
		Store:         st,
		Gateway:       gw,
		Sessions:      sessions.NewManager(st, gw, cfg),
		OCPI:          receiver,
		Emulators:     newEmulators(cfg, st),
		Sync:          ocpi.NewSyncer(st),
		portalLimiter: newPortalLimiter(5, 5*time.Minute),
	}
}

// newEmulators builds the CPO emulators from their profiles.
func newEmulators(cfg config.Config, st *store.Store) []*emulator.Emulator {
	emus := make([]*emulator.Emulator, 0, len(emulator.Profiles))
	for _, prof := range emulator.Profiles {
		emus = append(emus, emulator.New(cfg, st, prof))
	}
	return emus
}

// newGateway picks the charge transport.
//
// "ocpi" is the default gateway: it sends
// START_SESSION/STOP_SESSION/UNLOCK_CONNECTOR to a CPO over the commands module
// and turns the CPO's pushes back into telemetry. "local" is the in-process
// simulator, kept for debugging without the emulators running. Unknown values
// log a warning and use the simulator so the platform remains usable.
func newGateway(cfg config.Config, st *store.Store) gateway.CPOGateway {
	switch cfg.Gateway {
	case "local":
		return gateway.NewLocalGateway(cfg.SimTimeScale)
	case "", "ocpi":
		return ocpi.NewGateway(st, cfg)
	default:
		log.Printf("api: GATEWAY=%q is not a known transport; using the local simulator", cfg.Gateway)
		return gateway.NewLocalGateway(cfg.SimTimeScale)
	}
}

// NewRouter builds the top-level handler. Middleware order (outer→inner):
// panic recovery, request logging, CORS.
func NewRouter(cfg config.Config, st *store.Store) http.Handler {
	return NewServer(cfg, st).Handler()
}

// Handler composes the middleware chain and every route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	registerHealthRoutes(mux, s)
	registerAuthRoutes(mux, s)
	registerStationRoutes(mux, s)
	registerWalletRoutes(mux, s)
	registerVehicleRoutes(mux, s)
	registerSessionRoutes(mux, s)

	// The CPO portal lives under /admin with its API.
	registerPortalRoutes(mux, s)

	// OCPI endpoints use their own token scheme and bypass mobile bearer auth.
	if s.OCPI != nil {
		s.OCPI.Register(mux)
	} else {
		ocpi.New(s.Cfg, s.Store).Register(mux)
	}

	// CPO emulators live under /emulator/{slug}/ocpi and authenticate
	// with their own token_b, so they are mounted straight onto the mux too.
	for _, emu := range s.Emulators {
		emu.Mount(mux)
	}

	return Chain(mux, Recover, LogRequests, CORS(s.Cfg.PortalOrigin))
}

// Run re-arms the charges a restart interrupted, starts consuming gateway
// telemetry, and starts the CPO emulators' heartbeats. It returns at once; the
// work stops with ctx.
func (s *Server) Run(ctx context.Context) {
	s.Sessions.Run(ctx)
	for _, emu := range s.Emulators {
		emu.Run(ctx)
	}
	// The pull sync runs alongside them: it is what repairs a network whose
	// pushes were lost while ChargeNet was down. Like [Manager.Run]
	// and [emulator.Emulator.Run], [ocpi.Syncer.Run] owns a blocking ticker
	// loop, so it must be started in its own goroutine — awaited inline it
	// would never let main.go reach ListenAndServe and the platform would look
	// up but answer nobody.
	if s.Sync != nil {
		go s.Sync.Run(ctx)
	}
}

// Close ends every open SSE stream, stops the charge transport and stops the CPO
// emulators. It is what lets an http.Server shutdown finish: a stream that is
// left open would otherwise sit out the whole graceful-shutdown timeout.
func (s *Server) Close() {
	s.Sessions.Shutdown()
	if c, ok := s.Gateway.(interface{ Close() }); ok {
		c.Close()
	}
	for _, emu := range s.Emulators {
		emu.Close()
	}
}

// registerHealthRoutes installs GET /healthz.
func registerHealthRoutes(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := store.Health(ctx, s.Store.DB); err != nil {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "degraded",
			"db":     "down",
		})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"db":     "up",
	})
}
