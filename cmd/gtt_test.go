package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/machine"
)

func TestGTTLine(t *testing.T) {
	gpus := []machine.GPU{{Device: "card1", GTTUsedMiB: 43008, GTTTotalMiB: 44068, VRAMUsedMiB: 8000, VRAMTotMiB: 8192}}
	models := []machine.HostModel{
		{Server: machine.Ollama, Name: "qwen3:30b", SizeMiB: 30003, Client: "172.21.0.3 agentlab-control-plane"},
		{Server: machine.Lemonade, Name: "gemma3-4b-FLM", SizeMiB: 4608},
	}
	want := "iGPU GTT 43008 of 44068 MiB (in no cgroup), VRAM 8000 of 8192 MiB; the model servers hold ollama qwen3:30b 30003 MiB loaded by 172.21.0.3 agentlab-control-plane, lemonade gemma3-4b-FLM 4608 MiB"
	if got := gttLine(gpus, models); got != want {
		t.Errorf("gttLine =\n%s\nwant\n%s", got, want)
	}
	if got := gttLine(gpus, nil); got != "iGPU GTT 43008 of 44068 MiB (in no cgroup), VRAM 8000 of 8192 MiB; the model servers hold no model" {
		t.Errorf("gttLine without models = %q", got)
	}
	if gttLine(nil, models) != "" {
		t.Error("a machine without amdgpu has a GTT line")
	}
	if because("") != "" || because("x") != "; cause: x" {
		t.Error("because")
	}
}
