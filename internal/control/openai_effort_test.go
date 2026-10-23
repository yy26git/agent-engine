package control

import (
	"strings"
	"testing"
)

func TestOpenAIEffort_ModelSpecificLaunch(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6", "gpt-5.5"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "ultra"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				p := Profile{Name: "worker", Tool: "codex", Model: model, Effort: effort}
				spec, err := p.CreateSpec("/tmp/workspace", "test")
				unsupported := model == "gpt-5.5" && (effort == "max" || effort == "ultra") ||
					strings.HasSuffix(model, "-luna") && effort == "ultra"
				if unsupported {
					if err == nil {
						t.Fatal("unsupported selection admitted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if spec.Effort != effort || spec.Model != model {
					t.Fatalf("selection changed: %+v", spec)
				}
			})
		}
	}
}

func TestOpenAIEffort_CustomLaunchAndDefaults(t *testing.T) {
	p := Profile{Name: "reviewer", Tool: "codex", Model: "gpt-6-luna", Effort: "max", LaunchCommand: []string{"/usr/bin/codex", "--model", "{model}", "-c", "model_reasoning_effort={effort}"}}
	spec, err := p.CreateSpec("/tmp/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Wrapper, "model_reasoning_effort=max") || spec.Effort != "" {
		t.Fatalf("custom launch dropped effort: %+v", spec)
	}
	p.LaunchCommand, p.Effort = nil, ""
	spec, err = p.CreateSpec("/tmp/workspace", "test")
	if err != nil || spec.Effort != "" {
		t.Fatalf("default changed: %+v %v", spec, err)
	}
	p.Effort = "Extra High"
	if _, err = p.CreateSpec("/tmp/workspace", "test"); err == nil {
		t.Fatal("display label accepted as protocol value")
	}
	p.Tool, p.Model, p.Effort = "mcode", "MiniMax-M2.7", "max"
	if _, err = p.CreateSpec("/tmp/workspace", "test"); err != nil {
		t.Fatalf("MiniMax selection affected: %v", err)
	}
}
