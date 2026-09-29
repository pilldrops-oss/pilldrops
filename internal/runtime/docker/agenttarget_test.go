package docker

import (
	"errors"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestAgentTargetIsUnsupportedWithoutASupervisor(t *testing.T) {
	i := &dockerInstance{id: "sb_1"}
	addr, token, err := i.AgentTarget()
	if !errors.Is(err, rt.ErrUnsupported) {
		t.Errorf("AgentTarget with no supervisor = %v, want rt.ErrUnsupported", err)
	}
	if addr != "" || token != "" {
		t.Errorf("returned addr=%q token=%q despite the error", addr, token)
	}
}

func TestAgentTargetReturnsThePublishedAddressAndToken(t *testing.T) {
	i := &dockerInstance{id: "sb_1", agentHost: "127.0.0.1:49312", agentToken: "tok"}
	addr, token, err := i.AgentTarget()
	if err != nil {
		t.Fatalf("AgentTarget: %v", err)
	}
	if addr != "127.0.0.1:49312" || token != "tok" {
		t.Errorf("addr=%q token=%q", addr, token)
	}
}

func TestDockerInstanceImplementsAgentTargeter(t *testing.T) {
	var _ rt.AgentTargeter = (*dockerInstance)(nil)
}
