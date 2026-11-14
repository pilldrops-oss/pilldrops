package orchestrator

import "sync"

// privateConn is one open private connection a sandbox holds through a connector.
type privateConn struct {
	sandboxID, target string
	close             func()
}

// privateConnSet tracks open private connections so a revoked grant or a
// destroyed sandbox can cut them at once.
type privateConnSet struct {
	mu    sync.Mutex
	conns map[*privateConn]struct{}
}

func (p *privateConnSet) add(c *privateConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = map[*privateConn]struct{}{}
	}
	p.conns[c] = struct{}{}
}

func (p *privateConnSet) remove(c *privateConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, c)
}

// closeFor closes the sandbox's connections to target; an empty target means all of them.
func (p *privateConnSet) closeFor(sandboxID, target string) {
	p.mu.Lock()
	var hit []*privateConn
	for c := range p.conns {
		if c.sandboxID == sandboxID && (target == "" || c.target == target) {
			hit = append(hit, c)
		}
	}
	p.mu.Unlock()
	for _, c := range hit {
		c.close()
	}
}
