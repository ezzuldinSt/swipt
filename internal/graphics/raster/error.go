package raster

import (
	F "github.com/sagernet/sing/common/format"
)

// The names the JavaScript sees on a failure (SWError.m). TypeError and RangeError become
// those native errors, every other name a DOMException of that name (SWJavaScript.m).
const (
	NameError        = "Error"
	NameType         = "TypeError"
	NameRange        = "RangeError"
	NameData         = "DataError"
	NameTimeout      = "TimeoutError"
	NameUnavailable  = "UnavailableError"
	NameState        = "InvalidStateError"
	NameAbort        = "AbortError"
	NameNotFound     = "NotFoundError"
	NameNotSupported = "NotSupportedError"
)

// Error is a failure with the name and message the ObjC engine gives it. It marshals to the
// {"name", "message"} object that automation.Failure reads from an outcome.
type Error struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return e.Message
}

func failure(name string, message ...any) *Error {
	return &Error{Name: name, Message: F.ToString(message...)}
}

// failureCause names a failure and keeps only the message of its cause (SWFailCause).
func failureCause(name string, cause error, message ...any) *Error {
	return &Error{Name: name, Message: F.ToString(message...) + ": " + cause.Error()}
}

func aborted() *Error {
	return failure(NameAbort, "the operation was aborted")
}

// UnsupportedError reports a call this engine leaves to another one: text recognition,
// barcodes, recordings, videos and the live screen feed.
type UnsupportedError struct {
	Method string
}

func (e *UnsupportedError) Error() string {
	return "graphics: " + e.Method + " is not available on this platform"
}

func (e *UnsupportedError) Name() string {
	return NameNotSupported
}
