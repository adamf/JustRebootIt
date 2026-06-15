package aidiag

import "testing"

func TestModelSelectorFarAlwaysCheap(t *testing.T) {
	s := NewModelSelector("sonnet", "opus", true, 3)
	if p := s.Plan("latency|far", "far"); p.Eval || p.Model != "sonnet" {
		t.Errorf("far should always use cheap with no eval, got %+v", p)
	}
}

func TestModelSelectorEvalDisabled(t *testing.T) {
	s := NewModelSelector("sonnet", "opus", false, 3)
	if p := s.Plan("latency|near", "near"); p.Eval || p.Model != "opus" {
		t.Errorf("eval off: near should use expensive, got %+v", p)
	}
	if p := s.Plan("latency|far", "far"); p.Model != "sonnet" {
		t.Errorf("eval off: far should still use cheap, got %+v", p)
	}
}

func TestModelSelectorChoosesCheapWhenGoodEnough(t *testing.T) {
	s := NewModelSelector("sonnet", "opus", true, 3)
	class := "latency|near"

	// During eval, Plan asks for a dual-model run.
	if p := s.Plan(class, "near"); !p.Eval {
		t.Fatalf("class should start in eval mode, got %+v", p)
	}
	// Cheap agrees on 2 of 3 samples (majority) -> cheap is good enough.
	s.Record(class, true)
	s.Record(class, false)
	decided, chosen := s.Record(class, true)
	if !decided || chosen != "sonnet" {
		t.Fatalf("after 3 evals (2 agree) class should lock to cheap, got decided=%t chosen=%s", decided, chosen)
	}
	if p := s.Plan(class, "near"); p.Eval || p.Model != "sonnet" {
		t.Errorf("decided class should use cheap with no further eval, got %+v", p)
	}
}

func TestModelSelectorKeepsExpensiveWhenCheapWeak(t *testing.T) {
	s := NewModelSelector("sonnet", "opus", true, 3)
	class := "loss|shared"
	s.Record(class, false)
	s.Record(class, true)
	decided, chosen := s.Record(class, false) // 1 of 3 agree -> minority
	if !decided || chosen != "opus" {
		t.Fatalf("cheap weak (1/3) should lock to expensive, got decided=%t chosen=%s", decided, chosen)
	}
}
