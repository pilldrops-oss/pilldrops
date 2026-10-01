package httpapi

// agentFeature carries nothing: the agent surface is not part of this build.
type agentFeature struct{}

func newAgentFeature() agentFeature { return agentFeature{} }
