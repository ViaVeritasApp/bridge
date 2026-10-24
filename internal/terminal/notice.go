package terminal

import "bridge/internal/logx"

// announce tells the person at this computer, on its screen, that ViaVeritas
// opened something on it: best effort, in the background (a computer without
// a screen, or nobody signed in, shows nothing). The log says it either way.
func announce(text string) {
	logInfo(text)
	go func() {
		if err := showNotice("ViaVeritas", text); err != nil {
			logx.Debug("remote", "the notice couldn't show on this computer's screen: %v", err)
		}
	}()
}

func logInfo(text string) { logx.Info("remote", "%s", text) }
