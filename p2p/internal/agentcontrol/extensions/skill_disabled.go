package extensions

import "context"

// SkillService is deliberately metadata-only in this in-process server.  The
// server never installs or executes third-party local code.
type SkillService struct{}

func (SkillService) Capability(_ context.Context) error { return ErrLocalExecutionDisabled }
func (SkillService) Install(_ context.Context, _ Mutation) (LifecycleResult, error) {
	return LifecycleResult{}, ErrLocalExecutionDisabled
}
func (SkillService) Execute(_ context.Context, _ string, _ string) error {
	return ErrLocalExecutionDisabled
}
