package ocpi

import (
	"log"
	"net/http"
)

// CommandResultSink receives the asynchronous answer to a command ChargeNet sent
// a CPO. The CPO posts it to the callback URL it was
// given, some time after the synchronous ACCEPTED.
//
// The receiver forwards command results unchanged. This interface stays here to
// avoid a package cycle with the gateway that imports the OCPI client.
type CommandResultSink interface {
	// CommandResult delivers the result of one command. command is the OCPI
	// command (START_SESSION, …) and uid is what that command was given: the
	// session for a start or stop, the EVSE for an unlock.
	CommandResult(command, uid string, res CommandResponse)
}

// handleCommands serves the commands module: the asynchronous result of the
// commands ChargeNet sends to a CPO.
//
// Only the callback path exists here — `POST /commands/{command}/{uid}` — because
// ChargeNet is the sender of every command, not its receiver. The synchronous
// answer comes back on the call that sent it.
func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	parts := objectPath(r, ModuleCommands)
	switch {
	case len(parts) == 0:
		s.notServed(w, ModuleCommands)
	case len(parts) != 2:
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
	case r.Method != http.MethodPost:
		s.notServed(w, ModuleCommands)
	default:
		s.postCommandResult(w, r, parts[0], parts[1])
	}
}

// postCommandResult hands one command result to the gateway that is waiting for
// it.
func (s *Server) postCommandResult(w http.ResponseWriter, r *http.Request, command, uid string) {
	if !knownCommand(command) {
		Fail(w, http.StatusNotImplemented, StatusUnableToUseAPI,
			"ChargeNet does not send the "+command+" command.")
		return
	}
	push, ok := decodePush[CommandResponse](w, r, "command result")
	if !ok {
		return
	}
	if s.Commands == nil {
		// No gateway is running, so no command of ChargeNet's is outstanding.
		Fail(w, http.StatusNotFound, StatusClientError,
			"ChargeNet is not waiting for a command result.")
		return
	}
	s.Commands.CommandResult(command, uid, push.Object)
	log.Printf("ocpi: command %s for %s answered %s", command, uid, push.Object.Result)
	OK(w, http.StatusOK, push.Object)
}

// knownCommand reports whether ChargeNet ever sends this command.
func knownCommand(command string) bool {
	switch command {
	case CommandStartSession, CommandStopSession, CommandUnlockConnector:
		return true
	default:
		return false
	}
}
