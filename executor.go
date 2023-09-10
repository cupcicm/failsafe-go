package failsafe

import (
	"context"
	"math"
	"sync"

	"github.com/failsafe-go/failsafe-go/common"
)

/*
Executor handles failures according to configured policies. An executor can be created for a policy via:

	failsafe.With(policy)

An Executor can also be created for a composition of policies:

	failsafe.With(outerPolicy, innerPolicy)

See the [With] docs for details.
*/
type Executor[R any] interface {
	// Compose returns a new Executor that composes the currently configured policies around the given innerPolicy. For
	// example, consider:
	//
	//     failsafe.With(fallback).Compose(retryPolicy).Compose(circuitBreaker)
	//
	// This results in the following internal composition when executing a func and handling its result:
	//
	//     Fallback(RetryPolicy(CircuitBreaker(func)))
	Compose(innerPolicy Policy[R]) Executor[R]

	// WithContext returns a new copy of the Executor with the ctx configured. Any executions created with the resulting
	// Executor will be canceled when the ctx is done. Executions can cooperate with cancellation by checking
	// Execution.Canceled or Execution.IsCanceled.
	//
	// Note: This setting will cause a goroutine to be created for each execution, in order to propagate cancellations from
	// the ctx to the execution.
	WithContext(ctx context.Context) Executor[R]

	// OnComplete registers the listener to be called when an execution is complete.
	OnComplete(listener func(ExecutionCompletedEvent[R])) Executor[R]

	// OnSuccess registers the listener to be called when an execution is successful. If multiple policies, are configured,
	// this handler is called when execution is complete and all policies succeed. If all policies do not succeed, then the
	// OnFailure registered listener is called instead.
	OnSuccess(listener func(ExecutionCompletedEvent[R])) Executor[R]

	// OnFailure registers the listener to be called when an execution fails. This occurs when the execution fails according
	// to some policy, and all policies have been exceeded.
	OnFailure(listener func(ExecutionCompletedEvent[R])) Executor[R]

	// Run executes the fn until successful or until the configured policies are exceeded.
	//
	// Any panic causes the execution to stop immediately without calling any event listeners.
	Run(fn func() error) (err error)

	// RunWithExecution executes the fn until successful or until the configured policies are exceeded, while providing an
	// Execution to the fn.
	//
	// Any panic causes the execution to stop immediately without calling any event listeners.
	RunWithExecution(fn func(exec Execution[R]) error) (err error)

	// Get executes the fn until a successful result is returned or the configured policies are exceeded.
	//
	// Any panic causes the execution to stop immediately without calling any event listeners.
	Get(fn func() (R, error)) (R, error)

	// GetWithExecution executes the fn until a successful result is returned or the configured policies are exceeded, while
	// providing an Execution to the fn.
	//
	// Any panic causes the execution to stop immediately without calling any event listeners.
	GetWithExecution(fn func(exec Execution[R]) (R, error)) (R, error)
}

type executor[R any] struct {
	policies   []Policy[R]
	ctx        context.Context
	onComplete func(ExecutionCompletedEvent[R])
	onSuccess  func(ExecutionCompletedEvent[R])
	onFailure  func(ExecutionCompletedEvent[R])
}

/*
With creates and returns a new Executor for result type R that will handle failures according to the given policies. The
policies are composed around an execution and will handle execution results in reverse, with the last policy being
applied first. For example, consider:

	failsafe.With(fallback, retryPolicy, circuitBreaker).Get(func)

This is equivalent to composition using the Compose method:

	failsafe.With(fallback).Compose(retryPolicy).Compose(circuitBreaker).Get(func)

These result in the following internal composition when executing a func and handling its result:

	Fallback(RetryPolicy(CircuitBreaker(func)))
*/
func With[R any](outerPolicy Policy[R], innerPolicies ...Policy[R]) Executor[R] {
	innerPolicies = append([]Policy[R]{outerPolicy}, innerPolicies...)
	return &executor[R]{
		policies: innerPolicies,
	}
}

func (e *executor[R]) Compose(innerPolicy Policy[R]) Executor[R] {
	e.policies = append(e.policies, innerPolicy)
	return e
}

func (e *executor[R]) WithContext(ctx context.Context) Executor[R] {
	c := *e
	c.ctx = ctx
	return &c
}

func (e *executor[R]) OnComplete(listener func(ExecutionCompletedEvent[R])) Executor[R] {
	e.onComplete = listener
	return e
}

func (e *executor[R]) OnSuccess(listener func(ExecutionCompletedEvent[R])) Executor[R] {
	e.onSuccess = listener
	return e
}

func (e *executor[R]) OnFailure(listener func(ExecutionCompletedEvent[R])) Executor[R] {
	e.onFailure = listener
	return e
}

func (e *executor[R]) Run(fn func() error) error {
	_, err := e.execute(func(exec Execution[R]) (R, error) {
		return *(new(R)), fn()
	})
	return err
}

func (e *executor[R]) RunWithExecution(fn func(exec Execution[R]) error) error {
	_, err := e.execute(func(exec Execution[R]) (R, error) {
		return *(new(R)), fn(exec)
	})
	return err
}

func (e *executor[R]) Get(fn func() (R, error)) (R, error) {
	return e.execute(func(exec Execution[R]) (R, error) {
		return fn()
	})
}

func (e *executor[R]) GetWithExecution(fn func(exec Execution[R]) (R, error)) (R, error) {
	return e.execute(func(exec Execution[R]) (R, error) {
		return fn(exec)
	})
}

// This type mirrors part of spi.ExecutionInternal, which we don't import here to avoid a cycle.
type executionInternal[R any] interface {
	Record(result *common.ExecutionResult[R]) *common.ExecutionResult[R]
}

// This type mirrors part of spi.PolicyExecutor, which we don't import here to avoid a cycle.
type policyExecutor[R any] interface {
	Apply(innerFn func(Execution[R]) *common.ExecutionResult[R]) func(Execution[R]) *common.ExecutionResult[R]
}

func (e *executor[R]) execute(fn func(exec Execution[R]) (R, error)) (R, error) {
	outerFn := func(exec Execution[R]) *common.ExecutionResult[R] {
		// Copy exec before passing to user provided func
		execCopy := *(exec.(*execution[R]))
		result, err := fn(&execCopy)
		er := &common.ExecutionResult[R]{
			Result:     result,
			Error:      err,
			Complete:   true,
			Success:    true,
			SuccessAll: true,
		}
		execInternal := exec.(executionInternal[R])
		r := execInternal.Record(er)
		return r
	}

	// Compose policy executors from the innermost policy to the outermost
	for i, policyIndex := len(e.policies)-1, 0; i >= 0; i, policyIndex = i-1, policyIndex+1 {
		pp := e.policies[i].ToExecutor(policyIndex, *(new(R)))
		pe := pp.(policyExecutor[R])
		outerFn = pe.Apply(outerFn)
	}

	// Prepare execution
	canceledIndex := -1
	exec := &execution[R]{
		mtx:           &sync.Mutex{},
		canceled:      make(chan any),
		canceledIndex: &canceledIndex,
		ctx:           e.ctx,
	}

	// Propagate context cancellations to the execution
	ctx := e.ctx
	var executionDone chan any
	if ctx != nil {
		// This can be replaced with context.AfterFunc in 1.21
		executionDone = make(chan any)
		go func() {
			select {
			case <-ctx.Done():
				exec.Cancel(math.MaxInt, &common.ExecutionResult[R]{
					Error:    ctx.Err(),
					Complete: true,
				})
			case <-executionDone:
			}
		}()
	}

	// Initialize first attempt and execute
	exec.InitializeAttempt(canceledIndex)
	er := outerFn(exec)

	// Stop the Context AfterFunc and call listeners
	if executionDone != nil {
		close(executionDone)
	}
	if e.onSuccess != nil && er.SuccessAll {
		e.onSuccess(newExecutionCompletedEvent(er, exec))
	} else if e.onFailure != nil && !er.SuccessAll {
		e.onFailure(newExecutionCompletedEvent(er, exec))
	}
	if e.onComplete != nil {
		e.onComplete(newExecutionCompletedEvent(er, exec))
	}
	return er.Result, er.Error
}
