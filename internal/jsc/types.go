package jsc

import E "github.com/sagernet/sing/common/exceptions"

var ErrTerminated = E.New("jsc: the script was terminated")

type Exception struct {
	Message string
}

func (e *Exception) Error() string {
	return e.Message
}

type Function func(arguments []Value) (any, error)

type Bytes []byte
