package httpapi

// agentFeature carries nothing in the core build.
type agentFeature struct{}

func newAgentFeature() agentFeature { return agentFeature{} }

// StartAgentMirror is a no-op in the core build. Returns a callable stop func because callers defer it.
func (s *Server) StartAgentMirror() (stop func()) { return func() {} }
