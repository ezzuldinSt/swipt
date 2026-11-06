package automation

import (
	"encoding/json"
	"errors"
	"strconv"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/jsc"
)

var errThrown = E.New("the selector function threw")

type scriptCallbacks struct {
	engine   *engine
	callback jsc.Value
}

func shallow(element *device.ViewElement) (json.RawMessage, error) {
	copied := *element
	copied.Children = nil
	return json.Marshal(&copied)
}

func (c scriptCallbacks) Predicate(id int, element *device.ViewElement) (bool, error) {
	data, err := shallow(element)
	if err != nil {
		return false, err
	}
	return c.call("predicate", id, string(data))
}

func (c scriptCallbacks) Match(id int, text string) (bool, error) {
	return c.call("match", id, text)
}

func (c scriptCallbacks) call(kind string, id int, value string) (bool, error) {
	arguments := []jsc.Value{c.engine.context.NewString(kind), c.engine.context.NewString(strconv.Itoa(id)), c.engine.context.NewString(value)}
	defer func() {
		for _, argument := range arguments {
			argument.Release()
		}
	}()
	result, err := c.callback.Call(arguments...)
	if err != nil {
		return false, err
	}
	defer result.Release()
	answer, err := result.ToString()
	if err != nil {
		return false, err
	}
	if answer == "throw" {
		return false, errThrown
	}
	return answer == "true", nil
}

func (e *engine) match(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 4)
	if err != nil {
		return nil, err
	}
	if len(arguments) < 5 {
		return nil, E.New("expected the selector callback")
	}
	id, scope, encoded, mode := values[0], values[1], values[2], values[3]
	view, err := e.snapshots.get(id)
	if err != nil {
		return map[string]string{"failure": FailureOf(err)}, nil
	}
	found, err := func() ([]*device.ViewElement, error) {
		selector, err := device.ParseSelector([]byte(encoded))
		if err != nil {
			return nil, err
		}
		callbacks := scriptCallbacks{engine: e, callback: arguments[4]}
		if mode == "all" {
			return view.FindAll(scope, selector, callbacks)
		}
		element, err := view.Find(scope, selector, callbacks)
		if element == nil {
			return nil, err
		}
		return []*device.ViewElement{element}, err
	}()
	switch {
	case errors.Is(err, errThrown):
		return map[string]bool{"thrown": true}, nil
	case err != nil:
		return map[string]string{"failure": FailureOf(err)}, nil
	}
	return map[string][]*device.ViewElement{"elements": found}, nil
}
