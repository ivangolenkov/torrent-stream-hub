package engine

import "github.com/anacrolix/torrent"

// Main and offline clients share the same physical limits. Main has at most
// two hasher slots, and the single storage mutation worker admits one offline
// client. Waiting for this budget does not create a second worker per piece.
var sharedVerificationBudget = torrent.NewHashBudget(2, 1, 64, 8)

func (e *Engine) VerificationBudgetStats() torrent.HashBudgetStats {
	return sharedVerificationBudget.Stats()
}
