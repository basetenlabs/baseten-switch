package gateway

import (
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const desktopModelIDPrefix = "claude-baseten-model-"

type desktopModelDiscovery struct {
	LastServedAt string `json:"last_served_at"`
	ModelCount   int    `json:"model_count"`
	Revision     string `json:"revision,omitempty"`
}

func (g *Gateway) recordDesktopModelDiscovery(cl *clientListener, generation uint64, count int) {
	g.routingMu.RLock()
	defer g.routingMu.RUnlock()
	g.mu.Lock()
	current := g.clients[config.ClaudeDesktopClientName] == cl
	g.mu.Unlock()
	if !current {
		return
	}
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	if g.activeGeneration == generation {
		g.desktopModelDiscovery = &desktopModelDiscovery{
			Revision:     config.DesktopModelRevision(cl.cfg.SelectedModelAliases, cl.cfg.ModelAliases),
			LastServedAt: time.Now().UTC().Format(time.RFC3339Nano), ModelCount: count,
		}
	}
}

// desktopModelID keeps the routing identity separate from its display name.
// The versioned input and numeric encoding are part of the stable ID contract.
func desktopModelID(alias, slug string) string {
	payload, _ := json.Marshal([3]string{"desktop-model-v1", alias, slug})
	digest := sha256.Sum256(payload)
	return desktopModelIDPrefix + new(big.Int).SetBytes(digest[:]).String()
}
