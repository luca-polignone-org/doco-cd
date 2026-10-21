package docker

// Fork (rollback): the fork's failure store (<data>/fork/failures) survives a restart, the
// upstream DeploymentFailureStore does not.

// RestoreForkFailure puts a failure read from the fork's disk store back into memory under the
// full target, so the upstream retry of a failed deployment (force recreate, #1702) still runs
// after a restart, and returns the failure memory holds. A failure already in memory wins: it is
// the one of this process, with its retry budget.
//
// The restored desired state carries only the revision. Admit then sees a new desired state and
// the retry starts with a fresh budget, as it does upstream after a restart.
func (s *DeploymentFailureStore) RestoreForkFailure(target DeploymentTarget, failure DeploymentFailure) DeploymentFailure {
	s.mu.Lock()
	defer s.mu.Unlock()

	target = target.normalized()

	if state, ok := s.failures[target]; ok {
		return state.failure
	}

	failure = truncateDeploymentFailure(failure)
	s.failures[target] = deploymentFailureState{
		failure: failure,
		desired: DeploymentDesiredState{Revision: failure.CommitSHA},
	}

	return failure
}
