package server

import "time"

// ShutdownBudget is how long the process may spend in Server.Shutdown after SIGTERM.
// Docker and the reference unit must wait longer than this or they kill the process mid-drain.
func ShutdownBudget(drain time.Duration) time.Duration {
	if drain < 0 {
		drain = 0
	}
	return drain + 5*time.Second
}

// DrainReason is the stable code sent to clients (RTVI error and close reason)
// when the drain deadline expires: reconnect to another instance.
const DrainReason = "server_draining"
