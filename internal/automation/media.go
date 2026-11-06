package automation

import (
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"
)

func (e *engine) show(arguments []jsc.Value) (any, error) {
	if e.current.emit == nil {
		return nil, E.New("an image is shown only to swipt repl run and the js tool of MCP")
	}
	if len(arguments) == 0 {
		return nil, E.New("expected the image")
	}
	data, isBytes := arguments[0].Bytes()
	if !isBytes {
		return nil, E.New("the image is not a Uint8Array")
	}
	e.current.emit(Output{Kind: OutputImage, Image: data})
	return nil, nil
}
