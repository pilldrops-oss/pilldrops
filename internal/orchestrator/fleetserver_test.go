package orchestrator

import "encoding/json"

// Helpers for the only configuration that accepts heartbeats: NODE_KEY_SECRET
// set, every node authenticated with its own derived token.

// testNodeKeySecret stands in for NODE_KEY_SECRET; no test node is handed it.
const testNodeKeySecret = "control-plane-only-test-secret"

// fleetServer is NewServer for a control plane that has a fleet.
func fleetServer(apiKey, nodeToken string) *Server {
	s := NewServer(apiKey, nodeToken)
	s.nodeKeySecret = testNodeKeySecret
	return s
}

// nodeBearer is the heartbeat token a node with this id+zone must present.
func nodeBearer(id, zone string) string {
	return (&Server{nodeKeySecret: testNodeKeySecret}).nodeHeartbeatToken(id, zone)
}

// bearerFor derives the token for whichever node a heartbeat body claims to be.
// An unparseable body yields an empty bearer, which surfaces as a 401.
func bearerFor(hb string) string {
	var b heartbeatBody
	if json.Unmarshal([]byte(hb), &b) != nil {
		return ""
	}
	return nodeBearer(b.ID, b.Zone)
}
