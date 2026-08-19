package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// TestServerRunReturnsImmediately guards main.go's startup order. Run starts the
// session orchestrator, the CPO emulators and the pull sync, and only then does
// main.go serve:8080. All three own blocking loops (manager.consume, the
// emulator heartbeat, ocpi.Syncer's ticker), so any of them awaited inline would
// stop the process before it ever binds the port: the platform would log its
// boot traffic and then answer nobody, and the Flutter app would report "server
// took too long to respond" instead of a connection refusal. Run must return.
func TestServerRunReturnsImmediately(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Built exactly as main.go builds it — through NewServer, so the sync is the
	// real five-minute Syncer — but with the emulators dropped: a test must not
	// push a whole network at a port nothing is listening on.
	srv := api.NewServer(config.Config{Gateway: "local"}, st)
	srv.Emulators = nil
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		srv.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Run did not return: main.go can never reach ListenAndServe, so :8080 is never bound")
	}
}
