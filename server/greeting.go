package server

// queueGreeting speaks a greeting through say (the assistant path's Say). A greeting never fails a call:
// if it cannot be queued (the session is closing) onFail is told and the call goes on. A TTS failure after
// queueing is already counted by the TTS error metrics.
func queueGreeting(say func(string) error, text string, onFail func(error)) {
	if err := say(text); err != nil && onFail != nil {
		onFail(err)
	}
}
