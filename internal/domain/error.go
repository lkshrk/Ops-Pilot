package domain

import "fmt"

type ErrorClass string

const (
	ErrorUsage         ErrorClass = "usage"
	ErrorConfiguration ErrorClass = "configuration"
	ErrorPrerequisite  ErrorClass = "prerequisite"
	ErrorSystem        ErrorClass = "system"
)

type Error struct {
	Class     ErrorClass
	Operation string
	Cause     error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Operation
	}
	return fmt.Sprintf("%s: %v", e.Operation, e.Cause)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func (e *Error) ErrorClass() ErrorClass {
	return e.Class
}

// ErrorClassOf returns the highest-severity typed class anywhere in err's
// wrapped or joined error tree. It returns the empty class for untyped errors.
func ErrorClassOf(err error) ErrorClass {
	var result ErrorClass
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if classified, ok := current.(interface{ ErrorClass() ErrorClass }); ok &&
			errorClassSeverity(classified.ErrorClass()) > errorClassSeverity(result) {
			result = classified.ErrorClass()
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return result
}

func errorClassSeverity(class ErrorClass) int {
	switch class {
	case ErrorUsage:
		return 1
	case ErrorConfiguration:
		return 2
	case ErrorPrerequisite:
		return 3
	case ErrorSystem:
		return 4
	default:
		return 0
	}
}
