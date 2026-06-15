package aidiag

import "sync"

// ModelSelector learns, per problem class, whether the cheaper model is "good
// enough" so most investigations can run on it. Far problems always use the
// cheap model (a distant outage isn't worth an expensive analysis). For near
// and shared classes it evaluates: the first evalSamples events run BOTH models
// plus a judge, and once enough samples agree it locks in the cheaper model;
// otherwise it sticks with the expensive one. Safe for concurrent use.
type ModelSelector struct {
	cheap, expensive string
	evalEnabled      bool
	evalSamples      int

	mu      sync.Mutex
	classes map[string]*classState
}

type classState struct {
	decided bool
	model   string
	evals   int
	agree   int
}

// Plan tells the caller how to investigate one event.
type Plan struct {
	// Eval is true when both models should run and be judged (eval phase).
	Eval bool
	// Model is the single model to use when Eval is false.
	Model string
}

// NewModelSelector builds a selector. evalSamples is the number of dual-model
// evaluations per class before deciding; it is clamped to at least 1.
func NewModelSelector(cheap, expensive string, evalEnabled bool, evalSamples int) *ModelSelector {
	if evalSamples < 1 {
		evalSamples = 1
	}
	return &ModelSelector{
		cheap: cheap, expensive: expensive,
		evalEnabled: evalEnabled, evalSamples: evalSamples,
		classes: make(map[string]*classState),
	}
}

// Plan decides how to investigate an event of the given class and scope.
func (s *ModelSelector) Plan(class, scope string) Plan {
	// Far problems are never our internet problem — always use the cheap model.
	if scope == "far" {
		return Plan{Model: s.cheap}
	}
	if !s.evalEnabled {
		return Plan{Model: s.expensive}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if cs, ok := s.classes[class]; ok && cs.decided {
		return Plan{Model: cs.model}
	}
	// Unknown / still-evaluating class: run both and judge.
	return Plan{Eval: true}
}

// Record folds one evaluation result into a class. cheapAgreed reports whether
// the judge found the cheap model's analysis as good as the expensive one. It
// returns whether the class just became decided and which model won.
func (s *ModelSelector) Record(class string, cheapAgreed bool) (decided bool, chosen string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.classes[class]
	if cs == nil {
		cs = &classState{}
		s.classes[class] = cs
	}
	if cs.decided {
		return false, cs.model
	}
	cs.evals++
	if cheapAgreed {
		cs.agree++
	}
	if cs.evals >= s.evalSamples {
		cs.decided = true
		// Cheap is "good enough" if it agreed in a majority of the samples.
		if cs.agree*2 >= cs.evals {
			cs.model = s.cheap
		} else {
			cs.model = s.expensive
		}
		return true, cs.model
	}
	return false, ""
}
