package api_test

import (
	"testing"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// TestNewServerSelectsTheChargeTransport verifies `GATEWAY`: "ocpi" is the
// default and builds the real OCPI gateway, pointing the receiver's sinks at it
// so a CPO's pushes and asynchronous command results reach the orchestrator;
// anything else keeps the in-process simulator, whose receiver sinks stay nil.
// An unset gateway takes the default, the same way config.Load does.
func TestNewServerSelectsTheChargeTransport(t *testing.T) {
	cases := []struct {
		name     string
		cfg      string
		wantOCPI bool
	}{
		{"ocpi is the real gateway", "ocpi", true},
		{"empty defaults to the OCPI gateway", "", true},
		{"local is the simulator", "local", false},
		{"an unknown value falls back to the simulator", "typo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			srv := api.NewServer(config.Config{Gateway: tc.cfg}, st)
			t.Cleanup(srv.Close)

			_, isOCPI := srv.Gateway.(*ocpi.Gateway)
			if isOCPI != tc.wantOCPI {
				t.Fatalf("GATEWAY=%q selected %T, want the OCPI gateway: %v", tc.cfg, srv.Gateway, tc.wantOCPI)
			}
			if isOCPI {
				if srv.OCPI == nil || srv.OCPI.Sink == nil || srv.OCPI.Commands == nil {
					t.Fatalf("the OCPI receiver was not wired to the gateway: %+v", srv.OCPI)
				}
				// The sinks must be the gateway itself, or a CPO's push would
				// reach a second object the orchestrator never subscribed to.
				if sink, ok := srv.OCPI.Sink.(*ocpi.Gateway); !ok || sink != srv.Gateway {
					t.Fatalf("Sink = %T, want the OCPI gateway", srv.OCPI.Sink)
				}
				return
			}
			if _, ok := srv.Gateway.(*gateway.LocalGateway); !ok {
				t.Fatalf("GATEWAY=%q selected %T, want the local simulator", tc.cfg, srv.Gateway)
			}
			if srv.OCPI == nil || srv.OCPI.Sink != nil || srv.OCPI.Commands != nil {
				t.Fatalf("the simulator's receiver should have nil sinks: %+v", srv.OCPI)
			}
		})
	}
}
