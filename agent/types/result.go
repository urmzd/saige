package types

// ResultKind distinguishes intermediate from terminal results.
type ResultKind string

// Result kinds.
const (
	ResultDelta ResultKind = "delta"
	ResultFinal ResultKind = "final"
)

// Result wraps a value with a kind tag.
type Result[T any] struct {
	Kind  ResultKind
	Value T
}

// NewDelta creates an intermediate result.
func NewDelta[T any](v T) Result[T] {
	return Result[T]{Kind: ResultDelta, Value: v}
}

// NewFinal creates a terminal result.
func NewFinal[T any](v T) Result[T] {
	return Result[T]{Kind: ResultFinal, Value: v}
}
