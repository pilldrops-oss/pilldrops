package orchestrator

// The unauthenticated on-demand TLS check is rate-limited per source IP at
// 5 rps / burst 10.
//
// Heartbeats used to share that limit, which admitted only ~25 nodes behind one
// address (a node beats every 5 s): past that, measured with a node simulator,
// 90% of beats were 429 and the fleet flapped. One address stands for many
// nodes behind a NAT gateway, and for all of them when this tier sits behind a
// proxy without TRUSTED_PROXY_CIDRS. Heartbeats now get a per-IP ceiling sized
// for ~1000 nodes, still bounding the HMAC work a flood can cause, plus a
// per-node limit applied after the token verifies, so no one can spend a real
// node's allowance. ipLimiter (agentorigin.go) is the package's token bucket;
// it sweeps idle buckets, which matters because handleDomainCheck takes no
// credential and a peer rotating source addresses would otherwise grow the map
// for the life of the process.
const (
	heartbeatRPS   = 5
	heartbeatBurst = 10

	heartbeatIPRPS     = 200
	heartbeatIPBurst   = 400
	heartbeatNodeRPS   = 1
	heartbeatNodeBurst = 5
)
